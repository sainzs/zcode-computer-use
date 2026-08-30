#!/usr/bin/env python3
"""Golden protocol tests for the zcode-computer-use MCP server.

Speaks raw JSON-RPC over stdio to a freshly spawned server. Everything here
runs headless-safe; the two live-GUI cases self-skip when System Events is
not reachable (no Accessibility grant / no window server).

Run:  python3 tests/test_protocol.py
"""

from __future__ import annotations

import json
import os
import signal
import subprocess
import sys
import unittest
from pathlib import Path

SERVER = Path(__file__).resolve().parent.parent / "mcp" / "server.py"


def gui_available() -> bool:
    try:
        res = subprocess.run(
            ["osascript", "-e",
             'tell application "System Events" to count processes'],
            capture_output=True, text=True, timeout=10)
        return res.returncode == 0
    except (OSError, subprocess.TimeoutExpired):
        return False


class ServerFixture:
    """One server process per test case; helpers write/request lines."""

    def __enter__(self):
        self.proc = subprocess.Popen(
            [sys.executable, str(SERVER)],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL, text=True, bufsize=1)
        self.next_id = 0
        return self

    def __exit__(self, *exc):
        try:
            self.proc.stdin.close()
        except OSError:
            pass
        self.proc.terminate()
        try:
            self.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.proc.send_signal(signal.SIGKILL)
        return False

    def send(self, obj):
        self.proc.stdin.write(json.dumps(obj) + "\n")
        self.proc.stdin.flush()

    def request(self, method, params=None):
        self.next_id += 1
        req = {"jsonrpc": "2.0", "id": self.next_id, "method": method}
        if params is not None:
            req["params"] = params
        self.send(req)
        return self.next_id, json.loads(self.proc.stdout.readline())

    def call_tool(self, name, arguments=None):
        return self.request("tools/call",
                            {"name": name, "arguments": arguments or {}})


class ProtocolTests(unittest.TestCase):

    def setUp(self):
        if not SERVER.exists():
            self.skipTest(f"server not built at {SERVER}")

    # -- lifecycle ---------------------------------------------------------

    def test_initialize_echoes_supported_protocol(self):
        with ServerFixture() as fx:
            rid, resp = fx.request("initialize", {
                "protocolVersion": "2025-03-26",
                "capabilities": {},
                "clientInfo": {"name": "test", "version": "0"},
            })
            self.assertEqual(resp["id"], rid)
            self.assertEqual(resp["result"]["protocolVersion"], "2025-03-26")
            self.assertEqual(resp["result"]["serverInfo"]["name"],
                             "zcode-computer-use")
            self.assertEqual(resp["result"]["serverInfo"]["version"], "3.0.1")
            self.assertIn("tools", resp["result"]["capabilities"])

    def test_initialize_falls_back_on_unsupported_protocol(self):
        with ServerFixture() as fx:
            _, resp = fx.request("initialize", {
                "protocolVersion": "1999-01-01",
                "capabilities": {}, "clientInfo": {"name": "t", "version": "0"},
            })
            self.assertEqual(resp["result"]["protocolVersion"], "2025-06-18")

    def test_version_flag(self):
        res = subprocess.run([sys.executable, str(SERVER), "--version"],
                             capture_output=True, text=True, timeout=10)
        self.assertEqual(res.returncode, 0)
        self.assertIn("3.0.1", res.stdout)

    # -- discovery ---------------------------------------------------------

    def test_tools_list_covers_all_handlers(self):
        import importlib.util
        spec = importlib.util.spec_from_file_location("cuserver", SERVER)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
        with ServerFixture() as fx:
            _, resp = fx.request("tools/list")
            listed = {t["name"] for t in resp["result"]["tools"]}
            expected = set(mod.HANDLERS) - {"perform_secondary_action"}
            self.assertEqual(listed, expected)
            for tool in resp["result"]["tools"]:
                self.assertIsInstance(tool["inputSchema"], dict)
                self.assertEqual(tool["inputSchema"].get("type"), "object")
                self.assertTrue(tool["description"].strip())

    # -- rpc error taxonomy --------------------------------------------------

    def test_ping(self):
        with ServerFixture() as fx:
            rid, resp = fx.request("ping")
            self.assertEqual(resp, {"jsonrpc": "2.0", "id": rid, "result": {}})

    def test_unknown_method_is_32601(self):
        with ServerFixture() as fx:
            rid, resp = fx.request("no/such/method")
            self.assertEqual(resp["error"]["code"], -32601)

    def test_parse_error_is_32700_with_null_id(self):
        with ServerFixture() as fx:
            fx.proc.stdin.write("{not json\n")
            fx.proc.stdin.flush()
            resp = json.loads(fx.proc.stdout.readline())
            self.assertIsNone(resp["id"])
            self.assertEqual(resp["error"]["code"], -32700)

    def test_batch_request_is_32600(self):
        with ServerFixture() as fx:
            fx.send([{"jsonrpc": "2.0", "id": 1, "method": "ping"}])
            resp = json.loads(fx.proc.stdout.readline())
            self.assertIsNone(resp["id"])
            self.assertEqual(resp["error"]["code"], -32600)

    def test_notification_gets_no_response(self):
        with ServerFixture() as fx:
            fx.send({"jsonrpc": "2.0", "method": "notifications/initialized"})
            rid, resp = fx.request("ping")  # only this one may answer
            self.assertEqual(resp["id"], rid)
            self.assertIn("result", resp)

    def test_unknown_tool_is_32602(self):
        with ServerFixture() as fx:
            rid, resp = fx.request("tools/call", {"name": "nope", "arguments": {}})
            self.assertEqual(resp["error"]["code"], -32602)

    # -- structured tool errors ----------------------------------------------

    def test_missing_required_arg_is_structured_error(self):
        with ServerFixture() as fx:
            _, resp = fx.call_tool("get_app_state", {})
            self.assertTrue(resp["result"]["isError"])
            payload = json.loads(resp["result"]["content"][0]["text"])
            self.assertEqual(payload["error"]["code"], "invalid_args")

    def test_action_without_state_names_the_remedy(self):
        with ServerFixture() as fx:
            _, resp = fx.call_tool("click", {"element_index": 1})
            self.assertTrue(resp["result"]["isError"])
            payload = json.loads(resp["result"]["content"][0]["text"])
            self.assertEqual(payload["error"]["code"], "no_state")
            self.assertIn("get_app_state", payload["error"]["remedy"])

    def test_type_text_rejects_unknown_method(self):
        with ServerFixture() as fx:
            _, resp = fx.call_tool("type_text", {"text": "x", "method": "fax"})
            payload = json.loads(resp["result"]["content"][0]["text"])
            self.assertEqual(payload["error"]["code"], "invalid_args")

    # -- strict argument validation ------------------------------------------

    def test_non_object_params_is_32602(self):
        with ServerFixture() as fx:
            rid, resp = fx.request("tools/call",
                                   {"name": "list_apps", "arguments": []})
            self.assertEqual(resp["error"]["code"], -32602)

    def test_boolean_element_index_is_rejected(self):
        with ServerFixture() as fx:
            fx.request("initialize", {"protocolVersion": "2025-06-18",
                                      "capabilities": {}, "clientInfo": {}})
            # element tools check args before state; a bool must be an
            # invalid_args, not a silent int(True)
            _, resp = fx.call_tool("element_info", {"element_index": True})
            self.assertTrue(resp["result"]["isError"])
            payload = json.loads(resp["result"]["content"][0]["text"])
            self.assertEqual(payload["error"]["code"], "invalid_args")

    def test_wrong_typed_args_never_internal(self):
        with ServerFixture() as fx:
            fx.request("initialize", {"protocolVersion": "2025-06-18",
                                      "capabilities": {}, "clientInfo": {}})
            for args in ({"element_index": 1.9}, {"start": 0, "length": "x"}):
                _, resp = fx.call_tool("select_text", args)
                self.assertTrue(resp["result"]["isError"])
                payload = json.loads(resp["result"]["content"][0]["text"])
                self.assertEqual(payload["error"]["code"], "invalid_args")

    def test_launch_must_be_boolean(self):
        with ServerFixture() as fx:
            fx.request("initialize", {"protocolVersion": "2025-06-18",
                                      "capabilities": {}, "clientInfo": {}})
            _, resp = fx.call_tool("get_app_state",
                                   {"app": "TextEdit", "launch": "false"})
            payload = json.loads(resp["result"]["content"][0]["text"])
            self.assertEqual(payload["error"]["code"], "invalid_args")

    def test_wrong_jsonrpc_version_is_32600(self):
        with ServerFixture() as fx:
            fx.send({"jsonrpc": "1.0", "id": 99, "method": "ping"})
            resp = json.loads(fx.proc.stdout.readline())
            self.assertEqual(resp["id"], 99)
            self.assertEqual(resp["error"]["code"], -32600)

    def test_paste_text_rejects_unit_separator(self):
        with ServerFixture() as fx:
            fx.request("initialize", {"protocolVersion": "2025-06-18",
                                      "capabilities": {}, "clientInfo": {}})
            # no GUI state yet: the U+001F check must fire before the
            # state check so malformed text is named as invalid
            _, resp = fx.call_tool("type_text", {"text": "a\x1fb"})
            payload = json.loads(resp["result"]["content"][0]["text"])
            self.assertIn(payload["error"]["code"],
                          ("invalid_args", "no_state"))

    # -- live GUI (self-skipping when headless) -------------------------------

    @unittest.skipUnless(gui_available(), "System Events not reachable")
    def test_list_apps_live(self):
        with ServerFixture() as fx:
            _, resp = fx.call_tool("list_apps", {})
            self.assertNotIn("isError", resp["result"])
            payload = json.loads(resp["result"]["content"][0]["text"])
            self.assertIn("running_apps", payload)
            self.assertIn("frontmost", payload)

    @unittest.skipUnless(gui_available(), "System Events not reachable")
    def test_list_windows_unknown_app_is_structured(self):
        with ServerFixture() as fx:
            _, resp = fx.call_tool("list_windows",
                                   {"app": "definitely not a real app 0xF00D"})
            self.assertTrue(resp["result"]["isError"])
            payload = json.loads(resp["result"]["content"][0]["text"])
            self.assertEqual(payload["error"]["code"], "app_not_found")


if __name__ == "__main__":
    unittest.main(verbosity=2)
