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
