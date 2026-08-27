#!/usr/bin/env python3
"""zcode-computer-use MCP server (stdio) — v3.

A stdlib-only macOS GUI-control server:

  - get_app_state returns a per-window screenshot (PNG file path) plus an
    indexed accessibility tree rendered as text, with optional diff markers
    against the previous capture of the same window. The agent reasons over
    both.
  - Actions target elements by index (AXPress preferred) or by screenshot
    PIXEL coordinates; the server rescales pixels -> screen points using the
    capture's backing-store factor before synthesizing events.
  - After every action the state is marked stale; element-index actions
    require a fresh get_app_state (forced re-fetch discipline — index
    mapping can never drift from the rendered tree).

v3 additions over v2: MCP protocol negotiation, structured tool errors
(code/message/remedy JSON), list_windows, per-window targeting (by id,
title, or position), background observe (activate:false — no focus steal),
select_text, horizontal scroll, type_text with a keystrokes method that
never touches the clipboard, explicit launch opt-in (no silent app
launches), and a permissions-aware selftest.

Driving stack: System Events (AppleScript) for the AX tree and element
actions, cliclick for coordinate mouse events, osascript for key chords and
unicode text, screencapture for window PNGs, JXA's ObjC bridge for
CGWindowList lookups. Requires Accessibility and Screen Recording grants
for the hosting app.

Usage: spoken to over stdio as a Model Context Protocol server. Also:
  server.py selftest     dependency + permission preflight
  server.py --version
"""

from __future__ import annotations

import json
import os
import re
import shutil
import struct
import subprocess
import sys
import tempfile
import time
from pathlib import Path

SERVER_NAME = "zcode-computer-use"
SERVER_VERSION = "3.0.0"
SUPPORTED_PROTOCOLS = ("2025-03-26", "2025-06-18")
LATEST_PROTOCOL = SUPPORTED_PROTOCOLS[-1]

OSA_TIMEOUT_SEC = 25          # per AppleScript invocation
SCROLL_PIXELS_PER_PAGE = 450  # wheel pixels per "page"
MAX_ELEMENTS = 350
MAX_DEPTH_DEFAULT = 8
MAX_DEPTH_HARD = 12
KEEP_SCREENSHOTS = 24
KEYPAGE_DELAY = 0.25          # settle after activation before capture
TYPE_KEYS_MAX = 2000          # keystrokes method is slow; cap it
SELECT_KEYS_MAX = 400         # keyboard selection walk is slower still


# ---------------------------------------------------------------- errors

class ToolError(Exception):
    """A tool failure with a machine-readable code and an agent-usable
    remedy. Rendered into the tool result as structured JSON so the caller
    can branch on `code` instead of parsing prose."""

    def __init__(self, code: str, message: str, remedy: str = ""):
        super().__init__(message)
        self.code = code
        self.message = message
        self.remedy = remedy

    def payload(self) -> dict:
        err = {"code": self.code, "message": self.message}
        if self.remedy:
            err["remedy"] = self.remedy
        return {"error": err}


def classify_osascript_error(exc_message: str) -> ToolError:
    low = (exc_message or "").lower()
    if "assistive access" in low or "-1719" in low or "-25211" in low or \
            "not allowed assistive" in low:
        return ToolError(
            "permission_accessibility",
            "the host app lacks the Accessibility grant",
            "grant Accessibility to the host app (ZCode or your terminal) in "
            "System Settings > Privacy & Security > Accessibility, then "
            "restart the host app")
    if "screencapture" in low or "screen recording" in low:
        return ToolError(
            "permission_screen_recording",
            "window capture failed — the host app may lack Screen Recording",
            "grant Screen Recording in System Settings > Privacy & Security "
            "> Screen Recording, then restart the host app")
    return ToolError("osascript_error", exc_message or "osascript failed",
                     "see the server log for the full AppleScript error")


# ---------------------------------------------------------------- helpers

def data_dir() -> Path:
    env = os.environ.get("ZCODE_CUA_DATA") or os.environ.get("ZCODE_PLUGIN_DATA")
    base = Path(env) if env else Path.home() / ".cache" / SERVER_NAME
    base.mkdir(parents=True, exist_ok=True)
    try:
        base.chmod(0o700)
    except OSError:
        pass
    return base


def log_path() -> Path:
    return data_dir() / "server.log"


def log(event: str, **fields) -> None:
    """JSONL log: one JSON object per line, grep-able by event name."""
    try:
        rec = {"ts": round(time.time(), 3), "event": event}
        rec.update(fields)
        with open(log_path(), "a") as fh:
            fh.write(json.dumps(rec, ensure_ascii=False) + "\n")
    except OSError:
        pass


def run(cmd, timeout=OSA_TIMEOUT_SEC, input_text=None):
    """Run a subprocess; return CompletedProcess or None on failure/timeout."""
    try:
        return subprocess.run(
            cmd,
            input=input_text,
            capture_output=True,
            text=True,
            timeout=timeout,
        )
    except (subprocess.TimeoutExpired, OSError) as exc:
        log("subprocess_failed", cmd=cmd[:3], error=str(exc))
        return None


def osascript(source: str, lang: str = "AppleScript",
              timeout: float = OSA_TIMEOUT_SEC) -> str:
    res = run(["osascript", "-l", lang, "-e", source], timeout=timeout)
    if res is None or res.returncode != 0:
        err = (res.stderr if res else "").strip()[-300:]
        raise classify_osascript_error(err or "timeout")
    return res.stdout.rstrip("\n")


def as_str(value) -> str:
    return "" if value is None else str(value)


def as_esc(text: str) -> str:
    """Escape a string for an AppleScript double-quoted literal."""
    out = (
        text.replace("\\", "\\\\")
        .replace('"', '\\"')
        .replace("\n", "\\n")
        .replace("\r", "\\r")
        .replace("\t", "\\t")
    )
    leftover = {c for c in out if ord(c) < 0x20 or ord(c) == 0x7F}
    if leftover:
        raise ToolError(
            "invalid_args",
            "control characters in the text are not representable in an "
            "AppleScript literal",
            "strip control characters or use type_text method=paste with "
            "clean text")
    return out


def snippet(text: str, limit: int = 60) -> str:
    text = as_str(text).replace("\n", "\\n").replace("\r", "")
    return text if len(text) <= limit else text[: limit - 1] + "…"


# ------------------------------------------------- strict arg coercion

def arg_int(args: dict, key: str, default=None, minimum=None, maximum=None) -> int:
    """Exact-integer argument: booleans and floats are rejected rather than
    silently coerced (1.9 must not target element 1)."""
    value = args.get(key, default)
    if value is None:
        raise ToolError("invalid_args", f"{key} is required")
    if isinstance(value, bool) or not isinstance(value, int):
        raise ToolError("invalid_args", f"{key} must be an integer",
                        f"got {value!r}")
    if minimum is not None and value < minimum:
        raise ToolError("invalid_args", f"{key} must be >= {minimum}")
    if maximum is not None and value > maximum:
        raise ToolError("invalid_args", f"{key} must be <= {maximum}")
    return value


def arg_number(args: dict, key: str, default=None) -> float:
    value = args.get(key, default)
    if value is None:
        raise ToolError("invalid_args", f"{key} is required")
    if isinstance(value, bool) or not isinstance(value, (int, float)) \
            or isinstance(value, float) and (value != value or value in
                                             (float("inf"), float("-inf"))):
        raise ToolError("invalid_args", f"{key} must be a finite number")
    return float(value)


def arg_bool(args: dict, key: str, default: bool) -> bool:
    """Strict boolean: the string "false" must not act as truthy."""
    value = args.get(key, default)
    if not isinstance(value, bool):
        raise ToolError("invalid_args", f"{key} must be a boolean",
                        f"got {value!r}")
    return value


def arg_str(args: dict, key: str, required: bool = True):
    value = args.get(key)
    if value is None or value == "":
        if required:
            raise ToolError("invalid_args", f"{key} is required")
        return None
    if not isinstance(value, str):
        raise ToolError("invalid_args", f"{key} must be a string")
    return value


# ---------------------------------------------------------------- state

STATE = {
    "app": None,          # System Events process name
    "screenshot": None,   # latest PNG path
    "scale": None,        # screenshot pixels per screen point
    "origin": None,       # (x, y) screen points of the captured window origin
    "win_id": None,       # CGWindowID of the captured window
    "win_src": None,      # System Events address fragment of the captured
                          # window, e.g. '(window 1 of process "TextEdit")' —
                          # built fresh at capture time so element addresses
                          # always resolve against the captured window
    "elements": {},       # index -> {"chain": "1.2.3", "body": "AXButton …"}
    "stale": True,
    "counter": 0,
}

PREV_DIFF = {"key": None, "bodies": {}}   # chain -> element body text


def prune_screenshots(keep: Path = None) -> None:
    shots = sorted(data_dir().glob("state-*.png"))
    if keep is not None:
        shots = [s for s in shots if s != keep]
    for old in shots[:-KEEP_SCREENSHOTS]:
        try:
            old.unlink()
        except OSError:
            pass


# ---------------------------------------------------------------- windows (JXA)

WINDOW_LIST_JXA = """
ObjC.import('CoreGraphics');
function mk() {
  const opts = $.kCGWindowListOptionOnScreenOnly | $.kCGWindowListExcludeDesktopElements;
  const ref = $.CGWindowListCopyWindowInfo(opts, $.kCGNullWindowID);
  const n = $.CFArrayGetCount(ref);
  const out = [];
  for (let i = 0; i < n; i++) {
    const w = ObjC.deepUnwrap(ObjC.castRefToObject($.CFArrayGetValueAtIndex(ref, i), $.NSDictionary));
    if (w.kCGWindowLayer !== 0) continue;
    if (!w.kCGWindowOwnerName || !w.kCGWindowBounds) continue;
    out.push({
      owner: w.kCGWindowOwnerName,
      name: w.kCGWindowName || '',
      id: w.kCGWindowNumber,
      x: w.kCGWindowBounds.X, y: w.kCGWindowBounds.Y,
      w: w.kCGWindowBounds.Width, h: w.kCGWindowBounds.Height
    });
  }
  // NB: no CFRelease — JXA's bridge owns the ref and manually releasing it
  // segfaults osascript; each invocation is a fresh process, so nothing
  // actually leaks.
  return JSON.stringify(out);
}
mk();
"""


def window_list():
    raw = osascript(WINDOW_LIST_JXA, lang="JavaScript")
    return json.loads(raw) if raw else []


MAIN_DISPLAY_JXA = """
ObjC.import('CoreGraphics');
const r = $.CGDisplayBounds($.CGMainDisplayID());
JSON.stringify({x: $.CGRectGetMinX(r), y: $.CGRectGetMinY(r),
                w: $.CGRectGetWidth(r), h: $.CGRectGetHeight(r)});
"""


def main_display():
    return json.loads(osascript(MAIN_DISPLAY_JXA, lang="JavaScript"))


def windows_of(owner: str):
    """On-screen layer-0 windows owned by `owner`, front-to-back (the list
    comes back front-to-back, so the first match is the key window)."""
    low = owner.lower()
    return [w for w in window_list()
            if (w.get("owner") or "").lower() == low
            and (w.get("w") or 0) * (w.get("h") or 0) > 100]


def find_window(owner: str):
    wins = windows_of(owner)
    return wins[0] if wins else None


def find_window_by_id(win_id):
    for w in window_list():
        if w.get("id") == win_id:
            return w
    return None


def pick_window(owner: str, window) -> dict:
    """Resolve the `window` argument against the app's on-screen windows.

    int   -> CGWindowID match (hard error if gone)
    str   -> case-insensitive substring of the window title
    None  -> frontmost window of the app
    """
    wins = windows_of(owner)
    if not wins:
        raise ToolError("window_gone", f"no on-screen windows for {owner!r}",
                        "check the app is running with a visible window")
    if window is None or window == "":
        return wins[0]
    if isinstance(window, int):
        for w in wins:
            if w["id"] == window:
                return w
        raise ToolError("window_gone",
                        f"window id {window} is gone (closed/minimized)",
                        "call get_app_state again or list_windows")
    needle = str(window).lower()
    for w in wins:
        if needle in (w.get("name") or "").lower():
            return w
    raise ToolError(
        "window_gone",
        f"no window title matching {window!r}",
        "call list_windows to see current titles")


# ---------------------------------------------------------------- png size

def png_size(path: Path):
    data = path.read_bytes()[: 24]
    if len(data) < 24 or data[:8] != b"\x89PNG\r\n\x1a\n" or data[12:16] != b"IHDR":
        raise RuntimeError("not a PNG capture")
    w, h = struct.unpack(">II", data[16:24])
    return w, h


# ---------------------------------------------------------------- app queries

def running_apps():
    src = """
tell application "System Events"
    set names to name of every process whose background only is false
    set frontName to name of first application process whose frontmost is true
end tell
set d to AppleScript's text item delimiters
set AppleScript's text item delimiters to ","
set joined to names as text
set AppleScript's text item delimiters to d
return frontName & "|" & joined
"""
    raw = osascript(src)
    front, _, rest = raw.partition("|")
    apps = [a.strip() for a in rest.split(",") if a.strip()]
    return apps, front.strip()


def resolve_app(app: str):
    apps, _ = running_apps()
    for name in apps:
        if name.lower() == app.lower():
            return name
    for name in apps:
        if app.lower() in name.lower():
            return name
    return None


def activate_app(name: str):
    osascript(f'tell application "{as_esc(name)}" to activate')
    osascript(
        'tell application "System Events" to set frontmost of process "'
        f'{as_esc(name)}" to true'
    )
    time.sleep(KEYPAGE_DELAY)


# ---------------------------------------------------------------- AX tree

def se_windows_dump(process_name: str):
    """Index, name, position, and size of every System Events window of the
    process — used to map a CGWindowID onto a System Events window address."""
    src = (
        'tell application "System Events"\n'
        f'    set wins to windows of process "{as_esc(process_name)}"\n'
        "    set out to \"\"\n"
        "    repeat with i from 1 to (count of wins)\n"
        "        set w to item i of wins\n"
        "        set p to position of w\n"
        "        set s to size of w\n"
        "        set nm to \"\"\n"
        "        try\n"
        "            set nm to (name of w) as text\n"
        "        end try\n"
        "        set out to out & i & \"|\" & nm & \"|\" & ((item 1 of p) as "
        "text) & \",\" & ((item 2 of p) as text) & \"|\" & ((item 1 of s) as "
        "text) & \"x\" & ((item 2 of s) as text) & linefeed\n"
        "    end repeat\n"
        "    return out\n"
        "end tell"
    )
    out = []
    for ln in osascript(src).splitlines():
        parts = ln.split("|")
        if len(parts) != 4 or not parts[0].strip().isdigit():
            continue
        try:
            px, py = (int(float(v)) for v in parts[2].split(","))
            sw, sh = (int(float(v)) for v in parts[3].split("x"))
        except ValueError:
            continue
        out.append({"index": int(parts[0]), "name": parts[1],
                    "x": px, "y": py, "w": sw, "h": sh})
    return out


def se_window_src(process_name: str, cg_win: dict) -> str:
    """System Events address fragment for the window captured as `cg_win`.

    Match order: title, then position (CG and SE both report global
    top-left-origin points; +/-2pt tolerance covers rounding). Falls back to
    `window 1` with the position-mismatch recorded in the state header.
    """
    se_wins = se_windows_dump(process_name)
    title = (cg_win.get("name") or "").strip()
    if title:
        for w in se_wins:
            if w["name"].strip() == title:
                return f'(window {w["index"]} of process "{as_esc(process_name)}")'
    for w in se_wins:
        if abs(w["x"] - cg_win["x"]) <= 2 and abs(w["y"] - cg_win["y"]) <= 2 \
                and abs(w["w"] - cg_win["w"]) <= 2:
            return f'(window {w["index"]} of process "{as_esc(process_name)}")'
    return f'(window 1 of process "{as_esc(process_name)}")'


TREE_DUMP = """
property idxCounter : 0

on txt(v)
    if v is missing value then return ""
    try
        set v to (v as text)
        set v to my replaceText(v, linefeed, " ")
        set v to my replaceText(v, return, " ")
        return my replaceText(v, tab, " ")
    on error
        return ""
    end try
end txt

on dumpEl(addr, chainStr, depth, maxDepth, maxEls)
    set out to ""
    tell application "System Events"
        set kids to UI elements of addr
        repeat with i from 1 to (count of kids)
            if idxCounter >= maxEls then exit repeat
            set idxCounter to idxCounter + 1
            set kid to (item i of kids)
            try
                set props to (properties of kid)
                set ln to "[" & idxCounter & "] " & my txt(role of props)
                try
                    if name of props is not missing value then set ln to ln & " \\"" & (my txt(name of props)) & "\\""
                end try
                try
                    if title of props is not missing value and (title of props as text) is not "" then set ln to ln & " t=\\"" & (my txt(title of props)) & "\\""
                end try
                set ln to ln & (my stringOfValue(props)) & (my stringOfFrame(props))
            on error
                set ln to "[" & idxCounter & "] AXUnreadable"
            end try
            if chainStr is "" then
                set kidChain to (i as text)
            else
                set kidChain to chainStr & "." & (i as text)
            end if
            set out to out & ln & " @@" & kidChain & linefeed
            if depth < maxDepth then
                set out to out & (my dumpEl(kid, kidChain, depth + 1, maxDepth, maxEls))
            end if
        end repeat
    end tell
    return out
end dumpEl

on stringOfValue(props)
    try
        set v to value of props
        if v is missing value then return ""
        if class of v is text or class of v is string then
            set t to v
            if (count of t) > 60 then set t to (text 1 thru 60 of t) & "…"
            set t to my replaceText(t, linefeed, " ")
            set t to my replaceText(t, return, " ")
            return " v=\\"" & t & "\\""
        else if class of v is boolean or class of v is integer or class of v is real then
            return " v=" & (v as text)
        end if
    end try
    return ""
end stringOfValue

on stringOfFrame(props)
    try
        set p to position of props
        set s to size of props
        if p is missing value or s is missing value then return ""
        return " @px:" & ((item 1 of p) as text) & "," & ((item 2 of p) as text) & ":" & ((item 1 of s) as text) & "x" & ((item 2 of s) as text)
    end try
    return ""
end stringOfFrame

on replaceText(t, f, r)
    set d to AppleScript's text item delimiters
    set AppleScript's text item delimiters to f
    set parts to text items of t
    set AppleScript's text item delimiters to r
    set out to parts as text
    set AppleScript's text item delimiters to d
    return out
end replaceText

tell application "System Events"
    set winAddr to %WIN%
    set header to "window " & (my txt(name of winAddr)) & linefeed
end tell
set idxCounter to 0
set body to my dumpEl(winAddr, "", 1, %MAXDEPTH%, %MAXELS%)
return header & body
"""

# element lines: optional diff marker (+ new, ~ changed), index, body, chain
ELEMENT_LINE_RE = re.compile(r"^\[([+~]?)(\d+)\] (.*)$")


def dump_tree(win_src: str, max_depth: int, max_els: int):
    """Render the indexed AX tree. Each element line ends with ` @@<chain>`
    where chain is a dot-path of child indices (digits and dots only,
    regex-validated) — values containing ' @@' can never forge an address.
    Returns (lines, registry) where registry maps index -> {chain, body}."""
    src = TREE_DUMP.replace("%WIN%", win_src) \
                   .replace("%MAXDEPTH%", str(max_depth)) \
                   .replace("%MAXELS%", str(max_els))
    raw = osascript(src)
    lines, registry = [], {}
    for ln in raw.splitlines():
        if not ln.startswith("["):
            continue
        if " @@" in ln:
            head, _, chain = ln.rpartition(" @@")
            chain = chain.strip()
            if re.fullmatch(r"[0-9]+(\.[0-9]+)*", chain):
                m = ELEMENT_LINE_RE.match(head)
                if m:
                    idx = int(m.group(2))
                    lines.append(head)
                    registry[idx] = {"chain": chain, "body": m.group(3)}
                continue
        lines.append(ln)
    return lines, registry


def diff_tree(registry: dict, app: str, win_id) -> tuple[dict, dict]:
    """Diff registry bodies against the previous capture of the SAME
    app+window. Returns (counts, marked) where marked maps index ->
    (marker, body) with marker in {"", "+", "~"}. Keying by chain (not
    index) is what makes this survive index shifts: an element that moved
    keeps its identity."""
    key = f"{app}|{win_id}"
    prev = PREV_DIFF["bodies"] if PREV_DIFF["key"] == key else {}
    by_chain = {e["chain"]: e["body"] for e in registry.values()}
    marked = {}
    new = changed = same = 0
    for idx, e in registry.items():
        old = prev.get(e["chain"])
        if old is None:
            marked[idx] = ("+", e["body"])
            new += 1
        elif old != e["body"]:
            marked[idx] = ("~", e["body"])
            changed += 1
        else:
            marked[idx] = ("", e["body"])
            same += 1
    gone = sum(1 for c in prev if c not in by_chain)
    PREV_DIFF["key"] = key
    PREV_DIFF["bodies"] = by_chain
    counts = {"new": new, "changed": changed, "unchanged": same, "gone": gone,
              "first_capture": not prev}
    return counts, marked


def element_source(index: int) -> str:
    """Rebuild the System Events address from the stored index chain with a
    freshly escaped process name — never trust round-tripped text."""
    entry = STATE["elements"].get(index)
    if not entry:
        raise ToolError(
            "unknown_element",
            f"unknown element_index {index}",
            "call get_app_state and use an index from the current tree")
    src = STATE["win_src"] or \
        f'(window 1 of process "{as_esc(STATE["app"])}")'
    for part in entry["chain"].split("."):
        src = f"(UI element {int(part)} of {src})"
    return src


# ---------------------------------------------------------------- actions

def require_state(for_elements: bool):
    if not STATE["app"]:
        raise ToolError("no_state", "no app state yet",
                        "call get_app_state first")
    if for_elements and STATE["stale"]:
        raise ToolError(
            "stale_state",
            "state is stale after a previous action",
            "call get_app_state again before using element_index "
            "(index mapping must never outlive the tree it came from)")


def pixel_to_point(x: float, y: float, reanchor: bool = True):
    if not STATE["scale"] or not STATE["origin"]:
        raise ToolError("no_state", "no screenshot scale on record",
                        "call get_app_state first")
    if reanchor and STATE["stale"]:
        # an action may have moved the window since the capture; re-anchor
        # by the CAPTURED window id (not by owner — another window of the
        # same app must not silently steal the coordinate space)
        win = find_window_by_id(STATE["win_id"]) if STATE["win_id"] else None
        if win:
            STATE["origin"] = (win["x"], win["y"])
        else:
            raise ToolError(
                "window_gone",
                "the captured window is gone (closed/minimized)",
                "call get_app_state before further coordinate actions")
    ox, oy = STATE["origin"]
    return ox + x / STATE["scale"], oy + y / STATE["scale"]


def mark_stale():
    STATE["stale"] = True
    STATE["elements"] = {}


SCROLL_PY = r"""
import Quartz, sys, time
axis, total = sys.argv[1], int(sys.argv[2])
remaining = abs(total)
while remaining > 0:
    n = min(remaining, 120)
    ev = Quartz.CGEventCreateScrollWheelEvent(None, Quartz.kCGScrollEventUnitPixel, 1, 0)
    field = Quartz.kCGScrollWheelEventDeltaAxis1 if axis == "v" \
        else Quartz.kCGScrollWheelEventDeltaAxis2
    Quartz.CGEventSetIntegerValueField(ev, field, n if total > 0 else -n)
    # kCGSessionEventTap is load-bearing: wheel events posted to
    # kCGHIDEventTap are silently dropped (verified Safari, 2026-08-27)
    Quartz.CGEventPost(Quartz.kCGSessionEventTap, ev)
    remaining -= n
    time.sleep(0.02)
"""


def quartz_scroll(axis: str, signed_pixels: int):
    """Post wheel events via Apple's bundled python3 (pyobjc ships with the
    Command Line Tools python, not with Homebrew builds)."""
    candidates = ["/usr/bin/python3", shutil.which("python3")]
    last_stderr = ""
    for py in filter(None, candidates):
        res = run([py, "-c", SCROLL_PY, axis, str(signed_pixels)], timeout=30)
        if res is not None and res.returncode == 0:
            return
        if res is not None:
            last_stderr = res.stderr.strip()[-200:]
            if "No module named" in res.stderr:
                continue  # no pyobjc on this interpreter; try the next
    raise ToolError(
        "unsupported",
        f"could not post scroll events ({last_stderr or 'no interpreter'})",
        "scroll needs Apple's python3 with the bundled pyobjc — present on "
        "standard dev Macs at /usr/bin/python3 (Xcode Command Line Tools)")


def scroll_wheel(direction: str, pages: float):
    """direction is one of up/down/left/right. Sign convention (verified in
    Safari 2026-08-27): axis1 negative scrolls down, axis2 negative scrolls
    right — so down/right are the negative directions."""
    pixels = int(pages * SCROLL_PIXELS_PER_PAGE)
    signed = {"up": +pixels, "down": -pixels,
              "right": -pixels, "left": +pixels}[direction]
    quartz_scroll("v" if direction in ("up", "down") else "h", signed)


def cliclick(*commands: str, wait: int = 0):
    argv = ["cliclick"]
    if wait:
        argv += ["-w", str(wait)]
    argv += list(commands)
    res = run(argv, timeout=30)
    if res is None or res.returncode != 0:
        raise ToolError(
            "cliclick_error",
            f"cliclick failed: {(res.stderr if res else '').strip()}",
            "install it with: brew install cliclick")


def _frontmost_stmt() -> str:
    return f'    set frontmost of process "{as_esc(STATE["app"])}" to true\n'


def ax_press(index: int, times: int):
    addr = element_source(index)
    src = (
        'tell application "System Events"\n'
        + _frontmost_stmt()
        + f"    repeat {times} times\n"
        + f'        perform action "AXPress" of {addr}\n'
        + "    end repeat\n"
        + "end tell"
    )
    try:
        osascript(src)
    except ToolError as exc:
        if exc.code == "osascript_error" and "AXPress" in exc.message:
            raise ToolError(
                "unsupported",
                f"element {index} does not expose AXPress",
                "inspect it with element_info for available actions, use "
                "perform_action, or click by screenshot coordinates")
        raise


def ax_set_value(index: int, value: str):
    addr = element_source(index)
    src = (
        'tell application "System Events"\n'
        + _frontmost_stmt()
        + f'    set value of {addr} to "{as_esc(value)}"\n'
        + "end tell"
    )
    osascript(src)


AX_ACTION_RE = re.compile(r"^AX[A-Za-z]+$")


def ax_perform_action(index: int, action: str):
    if not AX_ACTION_RE.match(action):
        raise ToolError(
            "invalid_args",
            f"invalid action name {action!r}",
            "must look like AXPick, AXConfirm, AXIncrement (letters only, "
            "AX prefix); discover names with element_info")
    addr = element_source(index)
    src = (
        'tell application "System Events"\n'
        + _frontmost_stmt()
        + f'    perform action "{action}" of {addr}\n'
        + "end tell"
    )
    try:
        osascript(src)
    except ToolError as exc:
        if exc.code == "osascript_error" and action in exc.message:
            raise ToolError(
                "unsupported",
                f"element {index} does not expose action {action}",
                "list what it does expose with element_info")
        raise


def ax_read_sel_and_value(addr: str):
    """(selected_text, value, have_value) of one element, via a single
    osascript round-trip. Missing/unreadable values come back as ""."""
    src = (
        "on txt(v)\n"
        "    if v is missing value then return \"\"\n"
        "    try\n"
        "        return (v as text)\n"
        "    on error\n"
        "        return \"\"\n"
        "    end try\n"
        "end txt\n"
        "tell application \"System Events\"\n"
        f"    set el to {addr}\n"
        "    set sep to character id 31\n"
        "    set sel to \"\"\n"
        "    try\n"
        "        set sel to (value of attribute \"AXSelectedText\" of el) as text\n"
        "    end try\n"
        "    set val to \"\"\n"
        "    set haveVal to \"0\"\n"
        "    try\n"
        "        set val to (value of el) as text\n"
        "        set haveVal to \"1\"\n"
        "    end try\n"
        "    return (my txt(sel)) & sep & (my txt(val)) & sep & haveVal\n"
        "end tell"
    )
    parts = osascript(src).split("\x1f")
    if len(parts) != 3:
        raise ToolError(
            "internal", "selection readback had the wrong field count",
            "call get_app_state again and retry")
    sel, value, have_value = parts
    return sel, value, have_value == "1"


def ax_select_via_keys(addr: str, start: int, length: int):
    """Keyboard-walk a selection: focus, jump to document start (the Cocoa
    cmd+up binding), then arrow right. Slow but honored far more widely
    than scripted AXSelectedTextRange writes."""
    total = start + length
    src = (
        'tell application "System Events"\n'
        + _frontmost_stmt()
        + f"    set el to {addr}\n"
        + "    set focused of el to true\n"
        + "    delay 0.1\n"
        + "    key code 126 using command down\n"
        + (f"    repeat {start} times\n        key code 124\n    end repeat\n"
           if start else "")
        + f"    repeat {length} times\n"
        + "        key code 124 using shift down\n"
        + "    end repeat\n"
        + "end tell"
    )
    osascript(src, timeout=OSA_TIMEOUT_SEC + total * 0.1)


def ax_select_text(index: int, start: int, length: int) -> dict:
    """Select [start, start+length) and VERIFY by content: the AXSelectedText
    readback must equal that slice of the element's value. Apps that ignore
    the attribute write (TextEdit does) get the keyboard fallback."""
    addr = element_source(index)
    sel, value, have_value = ax_read_sel_and_value(addr)
    expected = None
    if have_value:
        if start + length > len(value):
            raise ToolError(
                "invalid_args",
                f"range [{start}:{start + length}) is beyond the element "
                f"text ({len(value)} chars)",
                "shrink the range or set the element value first")
        expected = value[start: start + length]

    def verified() -> bool:
        if expected is not None:
            return sel == expected
        return bool(sel)

    method = None
    try:
        osascript(
            'tell application "System Events"\n'
            + _frontmost_stmt()
            + f"    set el to {addr}\n"
            + f'    set value of attribute "AXSelectedTextRange" of el to '
              f"{{{start}, {length}}}\n"
            + "end tell")
        sel, _, _ = ax_read_sel_and_value(addr)
        if verified():
            method = "ax_range"
    except ToolError:
        pass  # attribute write unsupported; keyboard fallback follows

    if method is None and start + length <= SELECT_KEYS_MAX:
        ax_select_via_keys(addr, start, length)
        sel, _, _ = ax_read_sel_and_value(addr)
        if verified():
            method = "keyboard"

    if method is None:
        raise ToolError(
            "unsupported",
            f"could not verify selection [{start}:{start + length}) on "
            f"element {index}",
            "this app may not honor scripted selection; click to place the "
            "caret and use press_key with shift+arrows instead")
    return {"selected_text": sel, "method": method}


def ax_element_info(index: int) -> dict:
    addr = element_source(index)
    src = (
        "on txt(v)\n"
        "    if v is missing value then return \"\"\n"
        "    try\n"
        "        return (v as text)\n"
        "    on error\n"
        "        return \"\"\n"
        "    end try\n"
        "end txt\n"
        "on joinField(v)\n"
        "    set sep to character id 31\n"
        "    if (count of v) is 0 then return sep\n"
        "    set out to item 1 of v\n"
        "    repeat with j from 2 to (count of v)\n"
        "        set out to out & sep & (item j of v)\n"
        "    end repeat\n"
        "    return out\n"
        "end joinField\n"
        "tell application \"System Events\"\n"
        f"    set el to {addr}\n"
        "    set props to properties of el\n"
        "    set actNames to {}\n"
        "    try\n"
        "        set actNames to name of every action of el\n"
        "    end try\n"
        "    set p to position of el\n"
        "    set s to size of el\n"
        "    set sep to character id 31\n"
        "    return (my txt(role of props)) & sep & (my txt(name of props)) & sep & (my txt(value of props)) & sep & ((enabled of props) as text) & sep & ((focused of props) as text) & sep & ((item 1 of p) as text) & \",\" & ((item 2 of p) as text) & sep & ((item 1 of s) as text) & \"x\" & ((item 2 of s) as text) & sep & (my joinField(actNames))\n"
        "end tell"
    )
    raw = osascript(src)
    parts = raw.split("\x1f")
    if len(parts) != 8:
        # a field value smuggled a U+001F past us — refuse rather than
        # mis-bind role/name/value
        raise ToolError(
            "internal",
            "element_info readback had the wrong field count",
            "call get_app_state again; if it persists, the element text "
            "contains control characters")
    role, name, value, enabled, focused, pos, size, actions = parts
    return {
        "index": index, "role": role, "name": name, "value": value[:200],
        "enabled": enabled, "focused": focused, "position_pt": pos,
        "size_pt": size,
        "actions": [a.strip() for a in actions.split(",") if a.strip()],
    }


# key name -> AppleScript key code
KEY_CODES = {
    "return": 36, "enter": 36, "tab": 48, "escape": 53, "esc": 53,
    "space": 49, "delete": 51, "backspace": 51, "forwarddelete": 117,
    "home": 115, "end": 119, "pageup": 116, "page_up": 116,
    "pagedown": 121, "page_down": 121,
    "left": 123, "right": 124, "down": 125, "up": 126,
    "help": 114, "f1": 122, "f2": 120, "f3": 99, "f4": 118, "f5": 96,
    "f6": 97, "f7": 98, "f8": 100, "f9": 101, "f10": 109, "f11": 103,
    "f12": 111,
    # ANSI numeric keypad (code 90 is skipped by macOS)
    "kp_0": 82, "kp_1": 83, "kp_2": 84, "kp_3": 85, "kp_4": 86,
    "kp_5": 87, "kp_6": 88, "kp_7": 89, "kp_8": 91, "kp_9": 92,
    "kp_decimal": 65, "kp_multiply": 67, "kp_add": 69, "kp_clear": 71,
    "kp_divide": 75, "kp_subtract": 78, "kp_equals": 81,
}
MODIFIERS = {
    "super": "command down", "cmd": "command down", "command": "command down",
    "win": "command down",
    "ctrl": "control down", "control": "control down",
    "alt": "option down", "option": "option down", "meta": "option down",
    "shift": "shift down",
}


def press_key_chord(key: str):
    parts = [p.strip().lower() for p in key.split("+") if p.strip()]
    if not parts:
        raise ToolError("invalid_args", "empty key chord",
                        "use xdotool-style names: return, ctrl+c, super+v, "
                        "kp_0, f5")
    mods = [MODIFIERS[p] for p in parts[:-1] if p in MODIFIERS]
    unknown = [p for p in parts[:-1] if p not in MODIFIERS]
    if unknown:
        raise ToolError("invalid_args",
                        f"unknown modifier(s): {', '.join(unknown)}",
                        "known modifiers: " + ", ".join(sorted(set(MODIFIERS))))
    stem = parts[-1]
    using = ", ".join(mods)
    if stem in KEY_CODES:
        stmt = f"key code {KEY_CODES[stem]}"
    elif re.fullmatch(r"[a-z0-9]", stem):
        stmt = f'keystroke "{stem}"'
    else:
        raise ToolError("invalid_args", f"unsupported key name: {stem}",
                        "see the press_key tool description for the "
                        "supported names")
    if using:
        stmt += f" using {{{using}}}"
    osascript(
        'tell application "System Events"\n'
        + _frontmost_stmt()
        + f"    {stmt}\n"
        + "end tell"
    )


def type_via_clipboard(text: str) -> bool:
    """Paste text through the clipboard; returns whether the previous
    clipboard content was restored."""
    saved, restorable = None, True
    probe = run(["pbpaste", "-Prefer", "txt"], timeout=10)
    if probe is not None and probe.returncode == 0:
        saved = probe.stdout
        # an empty read from a non-empty clipboard means non-text content
        # (image/files) that pbpaste cannot give back — flag it, don't
        # restore ""
        types = run(["osascript", "-e", "return (clipboard info) as text"],
                    timeout=10)
        if not saved and types is not None and \
                types.stdout.strip() not in ("", "0"):
            restorable = False
    else:
        restorable = False
    copy = run(["pbcopy"], timeout=10, input_text=text)
    if copy is None or copy.returncode != 0:
        if restorable and saved is not None:
            run(["pbcopy"], timeout=10, input_text=saved)
        raise ToolError("osascript_error",
                        "pbcopy failed — text not typed, clipboard restored",
                        "retry; if it persists check disk space and "
                        "/usr/bin/pbcopy permissions")
    try:
        time.sleep(0.15)
        press_key_chord("super+v")
        time.sleep(0.15)
    finally:
        if restorable and saved is not None:
            restore = run(["pbcopy"], timeout=10, input_text=saved)
            if restore is None or restore.returncode != 0:
                restorable = False  # couldn't restore; caller reports it
    return restorable


def type_via_keystrokes(text: str):
    """Type literal text with `keystroke` — no clipboard involvement. Slow
    for long strings, and the target must take synthesized keyboard input."""
    if len(text) > TYPE_KEYS_MAX:
        raise ToolError(
            "invalid_args",
            f"keystrokes method is capped at {TYPE_KEYS_MAX} characters "
            f"(got {len(text)})",
            "use the default paste method for long text")
    stmts = []
    for i, chunk in enumerate(text.split("\n")):
        if i:
            stmts.append("keystroke return")
        if chunk:
            stmts.append(f'keystroke "{as_esc(chunk)}"')
    osascript(
        'tell application "System Events"\n'
        + _frontmost_stmt()
        + "\n".join(f"    {s}" for s in stmts) + "\n"
        + "end tell"
    )


# ---------------------------------------------------------------- tool impls

def tool_list_apps(_):
    apps, front = running_apps()
    return {
        "frontmost": front,
        "running_apps": apps,
        "hint": "call get_app_state with one of these names (partial match ok)",
    }


def tool_list_windows(args):
    app = args.get("app")
    if not app:
        raise ToolError("invalid_args", "app is required")
    name = resolve_app(app)
    if not name:
        raise ToolError("app_not_found", f"app {app!r} is not running",
                        "call list_apps for running names")
    wins = windows_of(name)
    return {
        "app": name,
        "windows": [
            {"id": w["id"], "title": w.get("name") or "",
             "bounds_pt": f"{w['x']},{w['y']} {w['w']}x{w['h']}",
             "frontmost": i == 0}
            for i, w in enumerate(wins)
        ],
        "hint": "pass one of these ids (or a title substring) as `window` "
                "to get_app_state",
    }


def tool_get_app_state(args):
    # any failure below must leave the state unusable, never live-looking
    mark_stale()
    app = arg_str(args, "app")
    depth = arg_int(args, "depth", default=MAX_DEPTH_DEFAULT,
                    minimum=0, maximum=MAX_DEPTH_HARD)
    window = args.get("window")
    if window is not None and not isinstance(window, (int, str)) \
            or isinstance(window, bool):
        raise ToolError("invalid_args",
                        "window must be an id (integer) or a title substring",
                        "call list_windows to see ids and titles")
    activate = arg_bool(args, "activate", True)
    launch = arg_bool(args, "launch", False)

    name = resolve_app(app)
    if not name:
        if launch:
            res = run(["open", "-a", app], timeout=15)
            if res is None or res.returncode != 0:
                raise ToolError(
                    "app_not_found",
                    f"could not launch app {app!r}",
                    (res.stderr if res else "").strip()[:200])
            # `open -a` accepts bundle paths, but the process will be
            # known by its basename ("TextEdit"), so poll on that
            poll_name = Path(app).stem if "/" in app else app
            deadline = time.time() + 10
            while time.time() < deadline and not name:
                name = resolve_app(poll_name) or resolve_app(app)
                if not name:
                    time.sleep(0.5)
        if not name:
            raise ToolError(
                "app_not_found", f"app {app!r} is not running",
                "pass launch:true to open it, or pick a running app from "
                "list_apps")
    if activate:
        activate_app(name)

    cg_win = pick_window(name, window)
    if activate and windows_of(name)[0]["id"] != cg_win["id"]:
        # raise the requested window, not just the app
        try:
            osascript(
                'tell application "System Events"\n'
                + _frontmost_stmt()
                + f'    perform action "AXRaise" of {se_window_src(name, cg_win)}\n'
                + "end tell")
            time.sleep(KEYPAGE_DELAY)
        except ToolError:
            pass  # AXRaise is best-effort; capture proceeds regardless

    win = cg_win if cg_win["w"] * cg_win["h"] > 100 else None

    # capture to an unpredictable temp name, then rename onto the counter
    # path — a pre-placed symlink at the predictable name is replaced, not
    # followed, and the file is private to this user
    fd, tmp_name = tempfile.mkstemp(dir=str(data_dir()), suffix=".png")
    os.close(fd)
    tmp_shot = Path(tmp_name)

    def capture(argv):
        res = run(argv, timeout=20)
        if res is None or res.returncode != 0:
            if res is not None:
                log("screencapture_failed", rc=res.returncode,
                    stderr=res.stderr.strip()[:200])
            return False
        return tmp_shot.exists() and tmp_shot.stat().st_size > 0

    try:
        ok = capture(["screencapture", "-x", "-o", "-l", str(cg_win["id"]),
                      str(tmp_shot)]) if win else False
        if ok:
            origin, points_w, win_id = (cg_win["x"], cg_win["y"]), \
                cg_win["w"], cg_win["id"]
            scope = (f"window {cg_win['id']} "
                     f"({snippet(cg_win['name'], 40) or 'unnamed'})")
        else:
            if not capture(["screencapture", "-x", str(tmp_shot)]):
                raise ToolError(
                    "permission_screen_recording",
                    "screencapture produced no image",
                    "grant Screen Recording to the host app in System "
                    "Settings > Privacy & Security > Screen Recording, then "
                    "restart it")
            disp = main_display()
            origin, points_w, win_id = (disp["x"], disp["y"]), disp["w"], None
            scope = ("full-screen (window capture failed; multi-display "
                     "coordinate accuracy is not guaranteed — prefer window "
                     "capture)")

        STATE["counter"] += 1
        shot = data_dir() / f"state-{STATE['counter']:06d}.png"
        os.replace(tmp_shot, shot)
        os.chmod(shot, 0o600)
    finally:
        # after a successful publish the temp name is gone (renamed), so an
        # unconditional unlink only ever cleans up failure paths
        tmp_shot.unlink(missing_ok=True)

    px_w, _ = png_size(shot)
    scale = round(px_w / points_w, 3) if points_w else 2.0
    prune_screenshots(keep=shot)

    win_src = se_window_src(name, cg_win)
    tree_lines, registry = [], {}
    tree_note = "disabled (depth 0)"
    if depth > 0:
        try:
            _, registry = dump_tree(win_src, depth, MAX_ELEMENTS)
            counts, marked = diff_tree(registry, name, win_id)
            tree_lines = [
                f"[{marked[idx][0]}{idx}] {marked[idx][1]} @@{e['chain']}"
                for idx, e in registry.items()
            ]
            tree_note = (f"{len(registry)} elements (depth {depth}); diff vs "
                         f"previous capture of this window: "
                         f"+{counts['new']} new, ~{counts['changed']} changed, "
                         f"{counts['gone']} gone"
                         + (" (first capture)"
                            if counts["first_capture"] else ""))
        except ToolError as exc:
            if exc.code == "permission_accessibility":
                tree_note = ("tree unavailable: Accessibility grant missing "
                             "(screenshot is still valid)")
            else:
                tree_note = f"tree unavailable: {exc}"

    STATE.update(app=name, screenshot=str(shot), scale=scale, origin=origin,
                 win_id=win_id, win_src=win_src, elements=registry,
                 stale=False)

    head = (
        f"app: {name}"
        + (" (frontmost)" if activate else " (background, not activated)")
        + "\n"
        f"capture: {scope}, {px_w}px wide, pixel_scale {scale}\n"
        f"screenshot: {shot}\n"
        f"tree: {tree_note}\n"
        "coordinates in the tree (@px:x,y are screenshot pixels); click "
        "element_index or x/y in screenshot pixels. Tree diff markers: "
        "[+n] new, [~n] changed since the previous capture.\n"
    )
    return {"state": head, "tree": "\n".join(tree_lines) or "(none)"}


def tool_click(args):
    element = args.get("element_index")
    if element is not None:
        element = arg_int(args, "element_index")
        if "mouse_button" in args and args["mouse_button"] != "left":
            raise ToolError(
                "invalid_args",
                "element clicks are left-presses (AXPress); mouse_button "
                "does not apply",
                "for right/middle clicks use screenshot pixel coordinates "
                "x/y with mouse_button")
    require_state(for_elements=(element is not None))
    button = args.get("mouse_button", "left")
    if button not in ("left", "right", "middle"):
        raise ToolError("invalid_args",
                        f"unknown mouse_button {button!r}",
                        "use left, right, or middle")
    count = arg_int(args, "click_count", default=1, minimum=1, maximum=5)
    if element is not None:
        ax_press(element, count)
        how = f"AXPress element {element}"
    else:
        if "x" not in args or "y" not in args:
            raise ToolError("invalid_args", "provide element_index or x/y",
                            "element targets are preferred; coordinates are "
                            "screenshot pixels")
        if count > 2:
            raise ToolError(
                "invalid_args",
                f"coordinate clicks support click_count 1-2, got {count}",
                "chain multiple click calls instead")
        if count == 2 and button != "left":
            raise ToolError(
                "invalid_args",
                f"double-click is left-only, got mouse_button={button}",
                "use two single clicks instead")
        px, py = pixel_to_point(arg_number(args, "x"),
                                arg_number(args, "y"))
        px, py = round(px), round(py)
        cmd = {"left": "c", "right": "rc", "middle": "mc"}[button]
        if count == 2 and button == "left":
            cmd = "dc"
        cliclick(f"{cmd}:{px},{py}", wait=12)
        how = f"{button} click at ({args['x']},{args['y']})px -> ({px},{py})pt"
    mark_stale()
    return {"result": f"{how}. Action completed. Call get_app_state to see "
                      "the updated UI."}


def tool_type_text(args):
    method = args.get("method", "paste")
    if method not in ("paste", "keys"):
        raise ToolError("invalid_args",
                        f"unknown method {method!r}",
                        "method must be 'paste' (default) or 'keys'")
    require_state(for_elements=False)
    text = args.get("text")
    if not text:
        raise ToolError("invalid_args", "text is required and must be "
                                        "non-empty")
    if not isinstance(text, str):
        raise ToolError("invalid_args", "text must be a string")
    if method == "paste" and "\x1f" in text:
        # U+001F is the server's internal AX field separator; it would
        # corrupt later element_info/selection readbacks
        raise ToolError(
            "invalid_args",
            "text contains the control character U+001F",
            "strip it (it is never meaningful in UI text) and retry")
    if method == "paste":
        restorable = type_via_clipboard(text)
        note = "" if restorable else (
            " NOTE: the previous clipboard held non-text content that could "
            "not be restored — re-copy it if needed.")
        how = "Typed via clipboard paste."
    elif method == "keys":
        type_via_keystrokes(text)
        note, how = "", "Typed via synthesized keystrokes (clipboard untouched)."
    mark_stale()
    return {"result": f"{how}{note} Action completed. Call get_app_state to "
                      "see the updated UI."}


def tool_press_key(args):
    require_state(for_elements=False)
    press_key_chord(args.get("key", ""))
    mark_stale()
    return {"result": f"Pressed {args.get('key')}. Action completed. Call "
                      "get_app_state to see the updated UI."}


def tool_scroll(args):
    require_state(for_elements=False)
    direction = args.get("direction", "down")
    if direction not in ("up", "down", "left", "right"):
        raise ToolError("invalid_args",
                        f"unknown direction {direction!r}",
                        "direction must be up, down, left, or right")
    pages = max(0.1, min(arg_number(args, "pages", default=1), 50))
    if "x" in args and "y" in args:
        px, py = pixel_to_point(float(args["x"]), float(args["y"]))
        cliclick(f"m:{round(px)},{round(py)}", wait=12)
    scroll_wheel(direction, pages)
    mark_stale()
    return {"result": f"Scrolled {direction} {pages} page(s). Action "
                      "completed. Call get_app_state to see the updated UI."}


def tool_drag(args):
    require_state(for_elements=False)
    for k in ("from_x", "from_y", "to_x", "to_y"):
        if k not in args:
            raise ToolError("invalid_args", f"{k} is required")
    # anchor once for both endpoints — two separate lookups could straddle
    # a window move between them
    fx, fy = pixel_to_point(arg_number(args, "from_x"),
                            arg_number(args, "from_y"))
    tx, ty = pixel_to_point(arg_number(args, "to_x"),
                            arg_number(args, "to_y"), reanchor=False)
    cliclick(f"dd:{round(fx)},{round(fy)}", f"dm:{round(tx)},{round(ty)}",
             f"du:{round(tx)},{round(ty)}", wait=60)
    mark_stale()
    return {"result": "Drag performed. Action completed. Call get_app_state "
                      "to see the updated UI."}


def tool_set_value(args):
    element = arg_int(args, "element_index")
    if "value" not in args:
        raise ToolError("invalid_args", "element_index and value are required")
    value = arg_str(args, "value")
    require_state(for_elements=True)
    ax_set_value(element, value)
    mark_stale()
    return {"result": f"Value set on element {element}. Action "
                      "completed. Call get_app_state to see the updated UI."}


def tool_select_text(args):
    element = arg_int(args, "element_index")
    start = arg_int(args, "start", default=0, minimum=0)
    length = arg_int(args, "length", minimum=1)
    if start + length > 100_000:
        raise ToolError("invalid_args",
                        "start+length capped at 100000")
    require_state(for_elements=True)
    result = ax_select_text(element, start, length)
    mark_stale()
    return {
        "result": f"Selected [{start}:{start + length}) on element "
                  f"{element} (method: {result['method']}).",
        "selected_text": result["selected_text"],
        "method": result["method"],
        "hint": "selected_text is verified against the element content",
    }


def tool_element_info(args):
    element = arg_int(args, "element_index")
    require_state(for_elements=True)
    return ax_element_info(element)


def tool_perform_action(args):
    element = arg_int(args, "element_index")
    action = arg_str(args, "action")
    require_state(for_elements=True)
    ax_perform_action(element, action)
    mark_stale()
    return {"result": f"Performed {action} on element "
                      f"{element}. Action completed. Call "
                      "get_app_state to see the updated UI."}


# ---------------------------------------------------------------- tool manifest

TOOLS = [
    {
        "name": "list_apps",
        "description": "List running GUI apps and the frontmost one.",
        "inputSchema": {"type": "object", "properties": {}},
    },
    {
        "name": "list_windows",
        "description": "List an app's on-screen windows (id, title, bounds, "
                       "frontmost flag) so you can target one with "
                       "get_app_state's `window` argument.",
        "inputSchema": {
            "type": "object",
            "properties": {"app": {"type": "string"}},
            "required": ["app"],
        },
    },
    {
        "name": "get_app_state",
        "description": (
            "Return an app window's screenshot (PNG path), pixel scale, and "
            "an indexed accessibility tree with diff markers against the "
            "previous capture of the same window ([+n] new, [~n] changed). "
            "Call ONCE per observation — do not re-call without acting in "
            "between. Element targets are preferred; fall back to x/y in "
            "screenshot pixels. `window` targets a specific window by id or "
            "title substring; `activate:false` observes without stealing "
            "focus; `launch:true` is required to open a non-running app."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "app": {"type": "string",
                        "description": "app name, path, or partial match"},
                "depth": {"type": "integer", "minimum": 0,
                          "maximum": MAX_DEPTH_HARD, "default": 8},
                "window": {"description": "window id (integer) or title "
                                          "substring; default frontmost"},
                "activate": {"type": "boolean", "default": True,
                             "description": "false = background observe, "
                                            "no focus steal"},
                "launch": {"type": "boolean", "default": False,
                           "description": "true = open the app if not running"},
            },
            "required": ["app"],
        },
    },
    {
        "name": "click",
        "description": "Click an element by index (preferred; left-press "
                       "via AXPress) or by screenshot pixel coordinates "
                       "(any button, click_count 1-2; double-click is "
                       "left-only).",
        "inputSchema": {
            "type": "object",
            "properties": {
                "element_index": {"type": "integer"},
                "x": {"type": "number"}, "y": {"type": "number"},
                "mouse_button": {"type": "string",
                                 "enum": ["left", "right", "middle"],
                                 "default": "left",
                                 "description": "coordinate clicks only"},
                "click_count": {"type": "integer", "minimum": 1,
                                "maximum": 5, "default": 1},
            },
        },
    },
    {
        "name": "type_text",
        "description": (
            "Type literal text into the frontmost app. method=paste "
            "(default) is fast and unicode-safe but briefly uses the "
            "clipboard (restored as plain text — rich formatting can be "
            "lost; method=keys never touches the clipboard but is slow "
            "and capped at 2000 characters)."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "text": {"type": "string"},
                "method": {"type": "string", "enum": ["paste", "keys"],
                           "default": "paste"},
            },
            "required": ["text"],
        },
    },
    {
        "name": "press_key",
        "description": "Press a key or chord in xdotool syntax (super+c, "
                       "Return, KP_0, ctrl+shift+t).",
        "inputSchema": {
            "type": "object",
            "properties": {"key": {"type": "string"}},
            "required": ["key"],
        },
    },
    {
        "name": "scroll",
        "description": "Scroll up/down/left/right by pages, optionally at "
                       "given screenshot pixel coordinates (recommended: "
                       "wheel events land under the cursor, so pass x/y or "
                       "position first).",
        "inputSchema": {
            "type": "object",
            "properties": {
                "direction": {"type": "string",
                              "enum": ["up", "down", "left", "right"]},
                "pages": {"type": "number", "default": 1},
                "x": {"type": "number"}, "y": {"type": "number"},
            },
        },
    },
    {
        "name": "drag",
        "description": "Drag from one screenshot pixel point to another.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "from_x": {"type": "number"}, "from_y": {"type": "number"},
                "to_x": {"type": "number"}, "to_y": {"type": "number"},
            },
            "required": ["from_x", "from_y", "to_x", "to_y"],
        },
    },
    {
        "name": "set_value",
        "description": "Set the value of a settable accessibility element by "
                       "index (no keystrokes, no clipboard).",
        "inputSchema": {
            "type": "object",
            "properties": {
                "element_index": {"type": "integer"},
                "value": {"type": "string"},
            },
            "required": ["element_index", "value"],
        },
    },
    {
        "name": "select_text",
        "description": "Select a character range [start, start+length) in a "
                       "text element by index. The selection is verified by "
                       "content and the chosen method is reported; apps that "
                       "ignore the AX range write get a keyboard-walk "
                       "fallback (capped at 400 characters).",
        "inputSchema": {
            "type": "object",
            "properties": {
                "element_index": {"type": "integer"},
                "start": {"type": "integer", "minimum": 0},
                "length": {"type": "integer", "minimum": 1},
            },
            "required": ["element_index", "start", "length"],
        },
    },
    {
        "name": "element_info",
        "description": "Inspect one element by index: role, name, value, "
                       "enabled/focused, frame, and the AX actions it "
                       "exposes (for perform_action).",
        "inputSchema": {
            "type": "object",
            "properties": {"element_index": {"type": "integer"}},
            "required": ["element_index"],
        },
    },
    {
        "name": "perform_action",
        "description": "Invoke a named accessibility action on an element "
                       "(AXPick, AXConfirm, AXCancel, AXIncrement, ...). "
                       "Discover names with element_info. (The v2 name "
                       "perform_secondary_action still works as an alias.)",
        "inputSchema": {
            "type": "object",
            "properties": {
                "element_index": {"type": "integer"},
                "action": {"type": "string",
                           "description": "AX action name, e.g. AXPick"},
            },
            "required": ["element_index", "action"],
        },
    },
]

HANDLERS = {
    "list_apps": tool_list_apps,
    "list_windows": tool_list_windows,
    "get_app_state": tool_get_app_state,
    "click": tool_click,
    "type_text": tool_type_text,
    "press_key": tool_press_key,
    "scroll": tool_scroll,
    "drag": tool_drag,
    "set_value": tool_set_value,
    "select_text": tool_select_text,
    "element_info": tool_element_info,
    "perform_action": tool_perform_action,
    # v2 name kept working so callers don't break on upgrade
    "perform_secondary_action": tool_perform_action,
}


# ---------------------------------------------------------------- selftest

def selftest() -> int:
    ok = True
    for name, probe in [
        ("osascript", lambda: osascript('return "1"') == "1"),
        ("cliclick", lambda: shutil.which("cliclick") is not None),
        ("screencapture", lambda: shutil.which("screencapture") is not None),
        ("Accessibility (AX query)", accessibility_probe),
        ("CGWindowList (JXA)", lambda: isinstance(window_list(), list)),
        ("Quartz scroll (pyobjc)", lambda: shutil.which("/usr/bin/python3")
            is not None and osascript_quartz_probe()),
    ]:
        try:
            good = probe()
            detail = ""
        except Exception as exc:
            good, detail = False, f" — {exc}"
        print(f"  {name}: {'ok' if good else 'FAIL'}{detail}")
        ok = ok and good
    print(f"data dir: {data_dir()}")
    print(f"log: {log_path()}")
    print("Permissions live on the HOST app (ZCode or your terminal):")
    print("  Accessibility    > System Settings > Privacy & Security > "
          "Accessibility")
    print("  Screen Recording > System Settings > Privacy & Security > "
          "Screen Recording")
    print("Restart the host app after granting either one.")
    return 0 if ok else 1


def accessibility_probe() -> bool:
    """A real AX query — this is exactly what fails without the grant, so
    the selftest can name the missing permission instead of a cryptic
    osascript error later."""
    try:
        osascript('tell application "System Events" to return '
                  '(name of first application process)')
        return True
    except ToolError as exc:
        if exc.code == "permission_accessibility":
            return False
        raise


def osascript_quartz_probe() -> bool:
    res = run(["/usr/bin/python3", "-c", "import Quartz"], timeout=15)
    return res is not None and res.returncode == 0


# ---------------------------------------------------------------- MCP plumbing

def dispatch(method: str, params):
    if not isinstance(params, dict):
        raise JsonRpcError(-32602, "params must be an object")
    if method == "initialize":
        requested = params.get("protocolVersion")
        version = requested if requested in SUPPORTED_PROTOCOLS \
            else LATEST_PROTOCOL
        return {
            "protocolVersion": version,
            "capabilities": {"tools": {"listChanged": False}},
            "serverInfo": {"name": SERVER_NAME, "version": SERVER_VERSION},
        }
    if method == "tools/list":
        return {"tools": TOOLS}
    if method == "tools/call":
        name = params.get("name")
        handler = HANDLERS.get(name)
        if not handler:
            raise JsonRpcError(-32602, f"unknown tool {name!r}")
        arguments = params.get("arguments", {})
        if not isinstance(arguments, dict):
            raise JsonRpcError(-32602, "arguments must be an object")
        try:
            result = handler(arguments)
            return {
                "content": [
                    {"type": "text",
                     "text": json.dumps(result, ensure_ascii=False, indent=1)}
                ]
            }
        except ToolError as exc:
            log("tool_error", tool=name, code=exc.code, message=exc.message)
            return {
                "content": [{"type": "text",
                             "text": json.dumps(exc.payload(),
                                                ensure_ascii=False)}],
                "isError": True,
            }
        except (RuntimeError, ValueError, TypeError, KeyError, OSError) as exc:
            log("tool_crash", tool=name, error=repr(exc))
            payload = {"error": {"code": "internal", "message": str(exc),
                                 "remedy": "retry once; if it persists, "
                                           "check the server log and "
                                           "re-call get_app_state"}}
            return {
                "content": [{"type": "text",
                             "text": json.dumps(payload, ensure_ascii=False)}],
                "isError": True,
            }
    if method == "logging/setLevel":
        return {}  # accepted; the JSONL file log is always on
    if method == "ping":
        return {}
    if method in ("prompts/list",):
        return {"prompts": []}
    if method in ("resources/list",):
        return {"resources": []}
    raise JsonRpcError(-32601, f"method not found: {method}")


class JsonRpcError(Exception):
    def __init__(self, code, message):
        super().__init__(message)
        self.code = code
        self.message = message


class ClientGone(Exception):
    """Raised by write_response when the client has disconnected; unwinds
    the loop to a clean exit no matter where we were in the protocol."""


def write_response(response: dict) -> None:
    try:
        sys.stdout.write(json.dumps(response) + "\n")
        sys.stdout.flush()
    except BrokenPipeError:
        raise ClientGone("client disconnected")


def serve() -> int:
    log("server_start", version=SERVER_VERSION, pid=os.getpid())
    try:
        for line in sys.stdin:
            line = line.strip()
            if not line:
                continue
            try:
                msg = json.loads(line)
            except json.JSONDecodeError as exc:
                log("bad_json", line=line[:120])
                write_response({
                    "jsonrpc": "2.0", "id": None,
                    "error": {"code": -32700,
                              "message": f"parse error: {exc}"},
                })
                continue
            if not isinstance(msg, dict):
                write_response({
                    "jsonrpc": "2.0", "id": None,
                    "error": {"code": -32600,
                              "message": "invalid request: batches not "
                                         "supported"},
                })
                continue
            if "jsonrpc" in msg and msg["jsonrpc"] != "2.0":
                write_response({
                    "jsonrpc": "2.0", "id": msg.get("id"),
                    "error": {"code": -32600,
                              "message": f"unsupported jsonrpc version: "
                                         f"{msg['jsonrpc']!r}"},
                })
                continue
            if "id" not in msg:
                method = msg.get("method", "")
                if method not in ("notifications/initialized",
                                  "notifications/cancelled"):
                    log("notification_ignored", method=method)
                continue
            if not isinstance(msg.get("method"), str):
                write_response({
                    "jsonrpc": "2.0", "id": msg["id"],
                    "error": {"code": -32600,
                              "message": "invalid request: missing method"},
                })
                continue
            try:
                # absent params -> {}; present-but-not-object -> dispatch
                # raises -32602 (an empty list must not pass as {})
                result = dispatch(msg["method"], msg.get("params", {}))
                response = {"jsonrpc": "2.0", "id": msg["id"],
                            "result": result}
            except JsonRpcError as exc:
                response = {"jsonrpc": "2.0", "id": msg["id"],
                            "error": {"code": exc.code,
                                      "message": exc.message}}
            except Exception as exc:  # never kill the server on one bad call
                log("dispatch_crash", method=msg.get("method"),
                    error=repr(exc))
                response = {"jsonrpc": "2.0", "id": msg["id"],
                            "error": {"code": -32603,
                                      "message": f"internal: {exc}"}}
            write_response(response)
    except ClientGone:
        log("client_disconnected")
        return 0
    log("server_stop")
    return 0


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "selftest":
        raise SystemExit(selftest())
    if len(sys.argv) > 1 and sys.argv[1] == "--version":
        print(f"{SERVER_NAME} {SERVER_VERSION} (protocol "
              f"{', '.join(SUPPORTED_PROTOCOLS)})")
        raise SystemExit(0)
    raise SystemExit(serve())
