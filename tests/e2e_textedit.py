#!/usr/bin/env python3
"""Live end-to-end verification of the zcode-computer-use v3 server against
TextEdit, spoken to over its real stdio protocol.

Creates a scratch document in /tmp, drives it through observe -> act ->
verify cycles, and quits without saving. Self-skips if TextEdit or the
Accessibility grant is unavailable.

Run:  python3 tests/e2e_textedit.py
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import time
from pathlib import Path

SERVER = Path(__file__).resolve().parent.parent / "mcp" / "server.py"
SCRATCH = Path("/tmp/cua-e2e-scratch.txt")

FAILURES = []


def check(name: str, cond: bool, detail: str = ""):
    print(f"  {'PASS' if cond else 'FAIL'}  {name}"
          + (f" — {detail}" if detail and not cond else ""))
    if not cond:
        FAILURES.append(name)


class Client:
    def __init__(self):
        self.proc = subprocess.Popen(
            [sys.executable, str(SERVER)], stdin=subprocess.PIPE,
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True,
            bufsize=1)
        self.next_id = 0

    def call(self, name, arguments=None, timeout=60):
        self.next_id += 1
        req = {"jsonrpc": "2.0", "id": self.next_id,
               "method": "tools/call",
               "params": {"name": name, "arguments": arguments or {}}}
        deadline = time.time() + timeout
        self.proc.stdin.write(json.dumps(req) + "\n")
        self.proc.stdin.flush()
        line = self.proc.stdout.readline()  # server answers 1:1 per request
        if not line and time.time() > deadline:
            raise TimeoutError(f"{name} timed out")
        resp = json.loads(line)
        result = resp["result"]
        if result.get("isError"):
            return None, json.loads(result["content"][0]["text"])
        return json.loads(result["content"][0]["text"]), None

    def close(self):
        try:
            self.proc.stdin.close()
        except OSError:
            pass
        self.proc.terminate()
        self.proc.wait(timeout=5)


def frontmost() -> str:
    res = subprocess.run(
        ["osascript", "-e",
         'tell application "System Events" to return '
         '(name of first application process whose frontmost is true)'],
        capture_output=True, text=True, timeout=10)
    return res.stdout.strip()


def tree_find(tree: str, needle: str):
    """Return (index, chain) of the first tree line containing needle."""
    for ln in tree.splitlines():
        if needle in ln and " @@" in ln:
            head, _, chain = ln.rpartition(" @@")
            idx = head.split("]", 1)[0].lstrip("[+~")
            return int(idx), chain
    return None, None


def main() -> int:
    if not SERVER.exists():
        print("server missing"); return 1
    try:
        subprocess.run(["osascript", "-e",
                        'tell application "System Events" to count processes'],
                       capture_output=True, timeout=10, check=True)
    except Exception:
        print("SKIP: System Events not reachable (Accessibility grant?)")
        return 0

    SCRATCH.write_text("E2E scratch\n", encoding="utf-8")
    subprocess.run(["open", "-a", "TextEdit", str(SCRATCH)], check=True)
    time.sleep(1.5)

    c = Client()
    try:
        # 1. observe (launch already done; app running)
        state, err = c.call("get_app_state",
                            {"app": "TextEdit", "depth": 6})
        check("observe TextEdit", state is not None,
              json.dumps(err or {}))
        if not state:
            return finish(c)
        shot = Path(json.loads(json.dumps(state)) and
                    Path([l for l in state["state"].splitlines()
                          if l.startswith("screenshot: ")][0].replace(
                              "screenshot: ", "")))
        check("screenshot file exists", shot.exists())
        check("first capture noted", "first capture" in state["state"])
        t_idx, _ = tree_find(state["tree"], "AXTextArea")
        check("AXTextArea in tree", t_idx is not None)
        if t_idx is None:
            return finish(c)

        # 2. re-observe: TextEdit materializes one AXGroup after the first
        # AX touch, so capture 2 may show +1; capture 3 must be converged
        state2, _ = c.call("get_app_state", {"app": "TextEdit", "depth": 6})
        state3, _ = c.call("get_app_state", {"app": "TextEdit", "depth": 6})
        note3 = [l for l in state3["state"].splitlines()
                 if l.startswith("tree: ")][0]
        check("diff converges by third capture",
              "+0 new" in note3 and "~0 changed" in note3, note3)

        # 3. type via paste
        _, err = c.call("click", {"element_index": t_idx})
        check("click text area", err is None, json.dumps(err or {}))
        _, err = c.call("type_text", {"text": "paste-line-alpha"})
        check("type_text paste", err is None, json.dumps(err or {}))

        # 4. type via keystrokes (no clipboard)
        _, err = c.call("type_text", {"text": "\nkeys-line-beta",
                                      "method": "keys"})
        check("type_text keys", err is None, json.dumps(err or {}))

        # 5. verify accumulated value via element_info
        s3, _ = c.call("get_app_state", {"app": "TextEdit", "depth": 4})
        info, err = c.call("element_info", {"element_index": t_idx})
        value = (info or {}).get("value", "")
        check("paste text landed", "paste-line-alpha" in value, value)
        check("keys text landed", "keys-line-beta" in value, value)

        # 6. set_value
        _, err = c.call("set_value",
                        {"element_index": t_idx, "value": "SETVALUE-OK tail"})
        check("set_value", err is None, json.dumps(err or {}))
        s4, _ = c.call("get_app_state", {"app": "TextEdit", "depth": 4})
        info, _ = c.call("element_info", {"element_index": t_idx})
        check("set_value visible", "SETVALUE-OK" in (info or {}).get("value", ""),
              (info or {}).get("value", ""))

        # 7. select_text round-trip (TextEdit ignores AX range writes, so
        # this exercises the keyboard fallback and its content verification)
        sel, err = c.call("select_text",
                          {"element_index": t_idx, "start": 0, "length": 8})
        check("select_text call", err is None, json.dumps(err or {}))
        check("select_text returned range",
              (sel or {}).get("selected_text", "") == "SETVALUE",
              json.dumps(sel or {}))
        check("select_text method reported",
              (sel or {}).get("method") in ("ax_range", "keyboard"),
              json.dumps(sel or {}))

        # 8. structured error: element action after action (stale)
        _, err = c.call("click", {"element_index": t_idx})
        check("stale guard fires", err and err["error"]["code"] == "stale_state",
              json.dumps(err or {}))

        # 9. background observe must not steal focus
        subprocess.run(["osascript", "-e", 'tell application "Finder" to activate'],
                       capture_output=True)
        time.sleep(1.0)
        before = frontmost()
        s5, err = c.call("get_app_state",
                         {"app": "TextEdit", "depth": 2, "activate": False})
        after = frontmost()
        check("background observe ok", err is None and s5 is not None,
              json.dumps(err or {}))
        check("background observe kept focus",
              "Finder" in before and "Finder" in after,
              f"before={before!r} after={after!r}")
        check("header says background",
              "background" in (s5 or {}).get("state", ""))

        # 10. list_windows + window targeting by title substring
        lw, err = c.call("list_windows", {"app": "TextEdit"})
        check("list_windows", err is None and lw and lw["windows"],
              json.dumps(err or {}))
        target = (lw or {}).get("windows", [{}])[0].get("title", "")
        s6, err = c.call("get_app_state",
                         {"app": "TextEdit", "depth": 1,
                          "window": target[:6] or "cua-e2e"})
        check("window targeting by title", err is None and s6 is not None,
              json.dumps(err or {}))

        # 11. no silent launch
        _, err = c.call("get_app_state", {"app": " Definitely Not Installed",
                                          "depth": 1})
        check("no silent launch", err and err["error"]["code"] == "app_not_found",
              json.dumps(err or {}))

        # 12. drag: move the TextEdit window by its title bar, verify via CGWindowList
        lw2, err = c.call("list_windows", {"app": "TextEdit"})
        check("drag pre: window listed", err is None and lw2 and lw2["windows"],
              json.dumps(err or {}))
        bx, by = (lw2 or {}).get("windows", [{}])[0].get("bounds_pt", "0,0 0,0").split()[0].split(",")
        bx, by = int(float(bx)), int(float(by))
        s7, err = c.call("get_app_state", {"app": "TextEdit", "depth": 0})
        scale = float([l for l in s7["state"].splitlines()
                       if "pixel_scale" in l][0].split("pixel_scale ")[1])
        # title bar center in screenshot pixels
        tx, ty = 380.0, 14.0 * scale
        _, err = c.call("drag", {"from_x": tx, "from_y": ty,
                                 "to_x": tx + 120 * scale, "to_y": ty + 40 * scale})
        check("drag call", err is None, json.dumps(err or {}))
        lw3, _ = c.call("list_windows", {"app": "TextEdit"})
        ax, ay = (lw3 or {}).get("windows", [{}])[0].get("bounds_pt", "0,0 0,0").split()[0].split(",")
        ax, ay = int(float(ax)), int(float(ay))
        moved = abs(ax - bx - 120) <= 12 and abs(ay - by - 40) <= 12
        check("drag moved window ~+120,+40 pt", moved, f"{bx},{by} -> {ax},{ay}")
        # drag back so the desktop stays tidy
        c.call("drag", {"from_x": tx + 120 * scale, "from_y": ty + 40 * scale,
                        "to_x": tx, "to_y": ty})

        # 13. perform_action: drive a real AX action discovered by element_info
        s8, _ = c.call("get_app_state", {"app": "TextEdit", "depth": 4})
        info2, err = c.call("element_info", {"element_index": t_idx})
        actions = (info2 or {}).get("actions", [])
        check("element exposes AX actions", bool(actions), str(actions))
        # prefer silent actions; AXShowMenu opens a menu we then dismiss
        pick = next((a for a in ("AXScrollToVisible", "AXConfirm", "AXShowMenu")
                     if a in actions), None)
        check("a drivable action exists", pick is not None, str(actions))
        if pick:
            _, err = c.call("perform_action", {"element_index": t_idx,
                                               "action": pick})
            check(f"perform_action {pick}", err is None, json.dumps(err or {}))
            if pick == "AXShowMenu":
                c.call("press_key", {"key": "escape"})

        # 14. press_key: deterministic edit — backspace deletes the trailing X
        s9, _ = c.call("get_app_state", {"app": "TextEdit", "depth": 4})
        _, err = c.call("set_value", {"element_index": t_idx, "value": "PRESSKEY-X"})
        c.call("get_app_state", {"app": "TextEdit", "depth": 2})
        _, err = c.call("press_key", {"key": "super+right"})
        check("press_key super+right", err is None, json.dumps(err or {}))
        _, err = c.call("press_key", {"key": "delete"})
        check("press_key delete", err is None, json.dumps(err or {}))
        sA, _ = c.call("get_app_state", {"app": "TextEdit", "depth": 4})
        infoA, _ = c.call("element_info", {"element_index": t_idx})
        check("press_key edited content", (infoA or {}).get("value", "") == "PRESSKEY-",
              repr((infoA or {}).get("value", "")))

    finally:
        finish(c)
    return 0


def finish(c: Client) -> int:
    c.close()
    subprocess.run(
        ["osascript", "-e",
         'tell application "TextEdit" to quit saving no'],
        capture_output=True, timeout=15)
    SCRATCH.unlink(missing_ok=True)
    print(f"\n{'ALL E2E CHECKS PASSED' if not FAILURES else 'FAILURES: ' + ', '.join(FAILURES)}")
    return 1 if FAILURES else 0


if __name__ == "__main__":
    sys.exit(main())
