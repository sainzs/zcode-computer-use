package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Golden protocol tests for cuu, ported from tests/test_protocol.py. They
// speak raw JSON-RPC over stdio to a freshly spawned `cuu serve` and pin the
// RPC error taxonomy and the strict argument validation. The two live-GUI
// cases self-skip when System Events is not reachable.

// ---------------------------------------------------------------- fixture

type serverFixture struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	nextID int
}

func startServer(t *testing.T) *serverFixture {
	t.Helper()
	bin := buildBinary(t)
	cmd := exec.Command(bin, "serve")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr // failures must be visible, not swallowed
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	fx := &serverFixture{t: t, cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}
	t.Cleanup(func() {
		// close stdin first so the server reaches EOF and exits on its own;
		// terminate only if it will not
		stdin.Close()
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		}
	})
	return fx
}

// buildBinary compiles cuu once per test run into a temp dir.
func buildBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "cuu")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build cuu: %v\n%s", err, out)
	}
	return bin
}

func (fx *serverFixture) send(line string) {
	fx.t.Helper()
	if _, err := io.WriteString(fx.stdin, line+"\n"); err != nil {
		fx.t.Fatalf("write to server: %v", err)
	}
}

func (fx *serverFixture) request(t *testing.T, method string, params map[string]any) (int, map[string]any) {
	t.Helper()
	fx.nextID++
	req := map[string]any{"jsonrpc": "2.0", "id": fx.nextID, "method": method}
	if params != nil {
		req["params"] = params
	}
	raw, _ := json.Marshal(req)
	fx.send(string(raw))
	return fx.nextID, fx.readResponse(t)
}

func (fx *serverFixture) readResponse(t *testing.T) map[string]any {
	t.Helper()
	line, err := fx.reader().ReadString('\n')
	if err != nil {
		t.Fatalf("read from server: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("server line is not JSON: %q", line)
	}
	return resp
}

func (fx *serverFixture) reader() *bufio.Reader { return fx.stdout }

func (fx *serverFixture) callTool(t *testing.T, name string, args map[string]any) map[string]any {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	_, resp := fx.request(t, "tools/call", map[string]any{"name": name, "arguments": args})
	return resp
}

// payloadOf extracts the structured payload from a tool result.
func payloadOf(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in response: %v", resp)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("no content in result: %v", result)
	}
	text, ok := content[0].(map[string]any)["text"].(string)
	if !ok {
		t.Fatalf("content[0].text missing: %v", content)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("payload is not JSON: %q", text)
	}
	return payload
}

func errCode(t *testing.T, payload map[string]any) string {
	t.Helper()
	errObj, ok := payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("payload has no error object: %v", payload)
	}
	code, _ := errObj["code"].(string)
	return code
}

func guiAvailable() bool {
	cmd := exec.Command("osascript", "-e", `tell application "System Events" to count processes`)
	cmd.Stdout = nil
	err := cmd.Run()
	return err == nil
}

// ---------------------------------------------------------------- lifecycle

func TestInitializeEchoesSupportedProtocol(t *testing.T) {
	fx := startServer(t)
	rid, resp := fx.request(t, "initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0"},
	})
	if resp["id"] != float64(rid) {
		t.Fatalf("id mismatch: %v", resp["id"])
	}
	result := resp["result"].(map[string]any)
	if result["protocolVersion"] != "2025-03-26" {
		t.Fatalf("protocolVersion: %v", result["protocolVersion"])
	}
	info := result["serverInfo"].(map[string]any)
	if info["name"] != "zcode-computer-use" || info["version"] != serverVersion {
		t.Fatalf("serverInfo: %v", info)
	}
	if _, ok := result["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatalf("capabilities.tools missing")
	}
}

func TestInitializeFallsBackOnUnsupportedProtocol(t *testing.T) {
	fx := startServer(t)
	_, resp := fx.request(t, "initialize", map[string]any{
		"protocolVersion": "1999-01-01",
		"capabilities":    map[string]any{}, "clientInfo": map[string]any{},
	})
	if resp["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Fatalf("fallback protocolVersion: %v", resp["result"])
	}
}

func TestVersionFlag(t *testing.T) {
	bin := buildBinary(t)
	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("version: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), serverVersion) {
		t.Fatalf("version output %q missing %s", out, serverVersion)
	}
}

// ---------------------------------------------------------------- discovery

func TestToolsListCoversAllHandlers(t *testing.T) {
	fx := startServer(t)
	_, resp := fx.request(t, "tools/list", nil)
	tools := resp["result"].(map[string]any)["tools"].([]any)
	listed := map[string]bool{}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		listed[tool["name"].(string)] = true
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok || schema["type"] != "object" {
			t.Fatalf("tool %s inputSchema: %v", tool["name"], tool["inputSchema"])
		}
		if strings.TrimSpace(tool["description"].(string)) == "" {
			t.Fatalf("tool %s has empty description", tool["name"])
		}
	}
	for name := range cliVerbs {
		if !listed[name] {
			t.Fatalf("handler %s not listed", name)
		}
	}
	if listed["perform_secondary_action"] {
		t.Fatalf("alias must not be listed")
	}
	if len(listed) != len(cliVerbs) {
		t.Fatalf("listed %d tools, want %d", len(listed), len(cliVerbs))
	}
}

// ---------------------------------------------------------------- rpc error taxonomy

func TestPing(t *testing.T) {
	fx := startServer(t)
	rid, resp := fx.request(t, "ping", nil)
	if resp["id"] != float64(rid) {
		t.Fatalf("ping id: %v", resp)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok || len(result) != 0 {
		t.Fatalf("ping result must be {}: %v", resp["result"])
	}
}

func TestUnknownMethodIs32601(t *testing.T) {
	fx := startServer(t)
	_, resp := fx.request(t, "no/such/method", nil)
	if resp["error"].(map[string]any)["code"] != float64(-32601) {
		t.Fatalf("code: %v", resp["error"])
	}
}

func TestParseErrorIs32700WithNullID(t *testing.T) {
	fx := startServer(t)
	fx.send("{not json")
	resp := fx.readResponse(t)
	if resp["id"] != nil {
		t.Fatalf("id must be null: %v", resp["id"])
	}
	if resp["error"].(map[string]any)["code"] != float64(-32700) {
		t.Fatalf("code: %v", resp["error"])
	}
}

func TestBatchRequestIs32600(t *testing.T) {
	fx := startServer(t)
	fx.send(`[{"jsonrpc": "2.0", "id": 1, "method": "ping"}]`)
	resp := fx.readResponse(t)
	if resp["id"] != nil {
		t.Fatalf("id must be null: %v", resp["id"])
	}
	if resp["error"].(map[string]any)["code"] != float64(-32600) {
		t.Fatalf("code: %v", resp["error"])
	}
}

func TestNotificationGetsNoResponse(t *testing.T) {
	fx := startServer(t)
	fx.send(`{"jsonrpc": "2.0", "method": "notifications/initialized"}`)
	rid, resp := fx.request(t, "ping", nil) // only this one may answer
	if resp["id"] != float64(rid) {
		t.Fatalf("notification produced a response: %v", resp)
	}
	if _, ok := resp["result"]; !ok {
		t.Fatalf("ping must answer: %v", resp)
	}
}

func TestUnknownToolIs32602(t *testing.T) {
	fx := startServer(t)
	_, resp := fx.request(t, "tools/call", map[string]any{"name": "nope", "arguments": map[string]any{}})
	if resp["error"].(map[string]any)["code"] != float64(-32602) {
		t.Fatalf("code: %v", resp["error"])
	}
}

func TestNonObjectArgumentsIs32602(t *testing.T) {
	fx := startServer(t)
	_, resp := fx.request(t, "tools/call", map[string]any{
		"name": "list_apps", "arguments": []any{},
	})
	// arguments present but not an object -> protocol error -32602, exactly
	// like the Python ("an empty list must not pass as {}")
	errObj, ok := resp["error"].(map[string]any)
	if !ok || errObj["code"] != float64(-32602) {
		t.Fatalf("expected -32602, got: %v", resp)
	}
	if errObj["message"] != "arguments must be an object" {
		t.Fatalf("message: %v", errObj["message"])
	}
}

func TestWrongJsonrpcVersionIs32600(t *testing.T) {
	fx := startServer(t)
	fx.send(`{"jsonrpc": "1.0", "id": 99, "method": "ping"}`)
	resp := fx.readResponse(t)
	if resp["id"] != float64(99) {
		t.Fatalf("id echo: %v", resp["id"])
	}
	if resp["error"].(map[string]any)["code"] != float64(-32600) {
		t.Fatalf("code: %v", resp["error"])
	}
}

// ---------------------------------------------------------------- structured tool errors

func TestMissingRequiredArgIsStructuredError(t *testing.T) {
	fx := startServer(t)
	resp := fx.callTool(t, "get_app_state", nil)
	if resp["result"].(map[string]any)["isError"] != true {
		t.Fatalf("expected isError: %v", resp)
	}
	if code := errCode(t, payloadOf(t, resp)); code != "invalid_args" {
		t.Fatalf("code: %s", code)
	}
}

func TestActionWithoutStateNamesTheRemedy(t *testing.T) {
	fx := startServer(t)
	resp := fx.callTool(t, "click", map[string]any{"element_index": 1})
	if resp["result"].(map[string]any)["isError"] != true {
		t.Fatalf("expected isError: %v", resp)
	}
	payload := payloadOf(t, resp)
	if code := errCode(t, payload); code != "no_state" {
		t.Fatalf("code: %s", code)
	}
	errObj := payload["error"].(map[string]any)
	if !strings.Contains(errObj["remedy"].(string), "get_app_state") {
		t.Fatalf("remedy: %v", errObj["remedy"])
	}
}

func TestTypeTextRejectsUnknownMethod(t *testing.T) {
	fx := startServer(t)
	resp := fx.callTool(t, "type_text", map[string]any{"text": "x", "method": "fax"})
	if code := errCode(t, payloadOf(t, resp)); code != "invalid_args" {
		t.Fatalf("code: %s", code)
	}
}

// ---------------------------------------------------------------- strict argument validation

func TestBooleanElementIndexIsRejected(t *testing.T) {
	fx := startServer(t)
	fx.request(t, "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{},
	})
	// element tools check args before state; a bool must be an invalid_args,
	// not a silent int(True)
	resp := fx.callTool(t, "element_info", map[string]any{"element_index": true})
	if code := errCode(t, payloadOf(t, resp)); code != "invalid_args" {
		t.Fatalf("code: %s", code)
	}
}

func TestWrongTypedArgsNeverInternal(t *testing.T) {
	fx := startServer(t)
	fx.request(t, "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{},
	})
	for _, args := range []map[string]any{
		{"element_index": 1.9},
		{"start": 0, "length": "x"},
	} {
		resp := fx.callTool(t, "select_text", args)
		if resp["result"].(map[string]any)["isError"] != true {
			t.Fatalf("expected isError for %v: %v", args, resp)
		}
		if code := errCode(t, payloadOf(t, resp)); code != "invalid_args" {
			t.Fatalf("code for %v: %s", args, code)
		}
	}
}

func TestLaunchMustBeBoolean(t *testing.T) {
	fx := startServer(t)
	fx.request(t, "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{},
	})
	resp := fx.callTool(t, "get_app_state", map[string]any{"app": "TextEdit", "launch": "false"})
	if code := errCode(t, payloadOf(t, resp)); code != "invalid_args" {
		t.Fatalf("code: %s", code)
	}
}

func TestPasteTextRejectsUnitSeparator(t *testing.T) {
	fx := startServer(t)
	fx.request(t, "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{},
	})
	// no GUI state yet: the U+001F check must fire before the state check so
	// malformed text is named as invalid — but either code matches the Python
	resp := fx.callTool(t, "type_text", map[string]any{"text": "a\x1fb"})
	code := errCode(t, payloadOf(t, resp))
	if code != "invalid_args" && code != "no_state" {
		t.Fatalf("code: %s", code)
	}
}

// ---------------------------------------------------------------- CLI verbs

func TestCliVerbExitCodes(t *testing.T) {
	bin := buildBinary(t)
	// usage error: unknown verb
	out, err := exec.Command(bin, "definitely-not-a-verb").CombinedOutput()
	if err == nil {
		t.Fatalf("unknown verb must fail: %q", out)
	}
	// exit code 2 is asserted via ExitError
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() != 2 {
		t.Fatalf("unknown verb exit: %d", ee.ExitCode())
	}
	// a tool error prints the structured payload and exits 1
	out, err = exec.Command(bin, "element_info", "--element_index", "3").CombinedOutput()
	if err == nil {
		t.Fatalf("element_info without state must fail: %q", out)
	}
	var payload map[string]any
	if jsonErr := json.Unmarshal(out, &payload); jsonErr != nil {
		t.Fatalf("tool error must be JSON on stdout: %q", out)
	}
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() != 1 {
		t.Fatalf("tool error exit: %d", ee.ExitCode())
	}
	errObj := payload["error"].(map[string]any)
	if errObj["code"] != "no_state" {
		t.Fatalf("code: %v", errObj["code"])
	}
}

// ---------------------------------------------------------------- live GUI (self-skipping when headless)

func TestListAppsLive(t *testing.T) {
	if !guiAvailable() {
		t.Skip("System Events not reachable")
	}
	fx := startServer(t)
	resp := fx.callTool(t, "list_apps", nil)
	if resp["result"].(map[string]any)["isError"] == true {
		t.Fatalf("list_apps failed: %v", resp)
	}
	payload := payloadOf(t, resp)
	if _, ok := payload["running_apps"]; !ok {
		t.Fatalf("running_apps missing: %v", payload)
	}
	if _, ok := payload["frontmost"]; !ok {
		t.Fatalf("frontmost missing: %v", payload)
	}
}

func TestListWindowsUnknownAppIsStructured(t *testing.T) {
	if !guiAvailable() {
		t.Skip("System Events not reachable")
	}
	fx := startServer(t)
	resp := fx.callTool(t, "list_windows", map[string]any{"app": "definitely not a real app 0xF00D"})
	if resp["result"].(map[string]any)["isError"] != true {
		t.Fatalf("expected isError: %v", resp)
	}
	if code := errCode(t, payloadOf(t, resp)); code != "app_not_found" {
		t.Fatalf("code: %s", code)
	}
}

// ---------------------------------------------------------------- pure-function goldens

func TestParseDumpGolden(t *testing.T) {
	raw := "window Untitled\n" +
		"[1] AXButton \"OK\" @px:10,20:80x24 @@1\n" +
		"[2] AXTextField \"name\" v=\"hel@@lo\" @px:0,0:10x10 @@2\n" +
		"[+3] AXStaticText \"new since last\" @@2.1\n" +
		"[~4] AXGroup @@2.2\n" +
		"junk line without bracket\n" +
		"[5] AXImage @@not a chain\n"
	parsed := parseDump(raw)
	if len(parsed.Lines) != 5 {
		t.Fatalf("lines: %d (%v)", len(parsed.Lines), parsed.Lines)
	}
	if len(parsed.Registry) != 4 {
		t.Fatalf("registry: %v", parsed.Registry)
	}
	// a body containing ' @@' must not forge an address: the LAST separator wins
	if e := parsed.Registry[2]; e.Chain != "2" || !strings.Contains(e.Body, "hel@@lo") {
		t.Fatalf("entry 2: %+v", e)
	}
	// the body is everything after "] " — role included, like Python's
	// ELEMENT_LINE_RE group 3
	if e := parsed.Registry[3]; e.Chain != "2.1" || !strings.HasPrefix(e.Body, `AXStaticText "new since`) {
		t.Fatalf("entry 3: %+v", e)
	}
	// "[5] AXImage @@not a chain" has no valid chain: kept as a plain line,
	// not registered
	if _, ok := parsed.Registry[5]; ok {
		t.Fatalf("invalid chain must not register: %v", parsed.Registry)
	}
}

func TestDiffTreeGolden(t *testing.T) {
	st := &serverState{Elements: map[int]treeEntry{}, PrevBodies: map[string]string{}}
	reg := map[int]treeEntry{
		1: {Chain: "1", Body: "button"},
		2: {Chain: "2", Body: "field"},
	}
	counts, marked := diffTree(st, reg, "TextEdit", 7)
	if counts.FirstCapture != true || counts.New != 2 {
		t.Fatalf("first: %+v", counts)
	}
	if marked[1][0] != "+" {
		t.Fatalf("marker: %v", marked[1])
	}
	// same window: unchanged element stays "", missing chain counts gone
	reg2 := map[int]treeEntry{
		1: {Chain: "1", Body: "button"},
		3: {Chain: "3", Body: "added"},
	}
	counts2, marked2 := diffTree(st, reg2, "TextEdit", 7)
	if counts2.Unchanged != 1 || counts2.New != 1 || counts2.Gone != 1 {
		t.Fatalf("second: %+v", counts2)
	}
	if marked2[1][0] != "" || marked2[3][0] != "+" {
		t.Fatalf("markers: %v %v", marked2[1], marked2[3])
	}
	// a DIFFERENT window is a fresh diff space
	counts3, _ := diffTree(st, reg2, "TextEdit", 9)
	if counts3.FirstCapture != true {
		t.Fatalf("other window: %+v", counts3)
	}
}

func TestAsEscGolden(t *testing.T) {
	cases := map[string]string{
		`plain`:      `plain`,
		`a"b`:        `a\"b`,
		`back\slash`: `back\\slash`,
		"new\nline":  `new\nline`,
		"tab\there":  `tab\there`,
	}
	for in, want := range cases {
		got, terr := asEsc(in)
		if terr != nil || got != want {
			t.Fatalf("asEsc(%q) = %q, %v; want %q", in, got, terr, want)
		}
	}
	if _, terr := asEsc("bad\x01char"); terr == nil || terr.Code != "invalid_args" {
		t.Fatalf("control char must be rejected: %v", terr)
	}
}

func TestPyFloatStrGolden(t *testing.T) {
	cases := map[float64]string{
		2.0: "2.0", 1.5: "1.5", 0.1: "0.1", 50: "50.0", 0.25: "0.25",
	}
	for in, want := range cases {
		if got := pyFloatStr(in); got != want {
			t.Fatalf("pyFloatStr(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestArgCoercionGolden(t *testing.T) {
	decode := func(s string) args {
		var m map[string]any
		dec := json.NewDecoder(strings.NewReader(s))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		return args(m)
	}
	a := decode(`{"i": 3, "f": 1.9, "b": true, "s": "x", "e": 1e2}`)
	if v, terr := argInt(a, "i", nil, false, nil, nil); terr != nil || v != 3 {
		t.Fatalf("int: %v %v", v, terr)
	}
	if _, terr := argInt(a, "f", nil, false, nil, nil); terr == nil || terr.Code != "invalid_args" {
		t.Fatalf("1.9 must not be an int: %v", terr)
	}
	if _, terr := argInt(a, "b", nil, false, nil, nil); terr == nil {
		t.Fatalf("bool must not be an int")
	}
	if _, terr := argInt(a, "e", nil, false, nil, nil); terr == nil {
		t.Fatalf("1e2 must not be an int (Python json would decode it as float)")
	}
	if v, terr := argNumber(a, "f", nil, false); terr != nil || v != 1.9 {
		t.Fatalf("number: %v %v", v, terr)
	}
	if v, terr := argNumber(a, "i", nil, false); terr != nil || v != 3 {
		t.Fatalf("int passes number: %v %v", v, terr)
	}
	if _, terr := argNumber(a, "b", nil, false); terr == nil {
		t.Fatalf("bool must not be a number")
	}
	if v, terr := argBool(a, "b", false); terr != nil || !v {
		t.Fatalf("bool: %v %v", v, terr)
	}
	if _, terr := argBool(a, "s", true); terr == nil {
		t.Fatalf("string must not be a bool")
	}
	if v, terr := argStr(a, "s", true); terr != nil || v != "x" {
		t.Fatalf("str: %v %v", v, terr)
	}
	if _, terr := argStr(a, "missing", true); terr == nil {
		t.Fatalf("missing required str must fail")
	}
	if v, terr := argStr(a, "missing", false); terr != nil || v != "" {
		t.Fatalf("optional str: %v %v", v, terr)
	}
}
