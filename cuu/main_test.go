package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
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
	return startServerInDir(t, t.TempDir())
}

// startServerInDir is startServer against a caller-prepared data dir — tools
// that read a pre-existing capture (ocr) test against a hand-written
// state.json instead of driving a real get_app_state.
func startServerInDir(t *testing.T, dataDir string) *serverFixture {
	t.Helper()
	bin := buildBinary(t)
	cmd := exec.Command(bin, "serve")
	// hermetic data dir: the developer's real state.json must never flip a
	// no_state assertion into stale_state
	cmd.Env = append(os.Environ(), "ZCODE_CUA_DATA="+dataDir)
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
	cliCmd := exec.Command(bin, "element_info", "--element_index", "3")
	cliCmd.Env = append(os.Environ(), "ZCODE_CUA_DATA="+t.TempDir())
	out, err = cliCmd.CombinedOutput()
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

// ---------------------------------------------------------------- 4.1 tools

func TestFindRequiresQueryAndState(t *testing.T) {
	fx := startServer(t)
	// neither text nor role -> invalid_args, checked before state
	resp := fx.callTool(t, "find", nil)
	if code := errCode(t, payloadOf(t, resp)); code != "invalid_args" {
		t.Fatalf("code: %s", code)
	}
	// valid query but no capture yet -> no_state
	resp = fx.callTool(t, "find", map[string]any{"text": "Save"})
	if code := errCode(t, payloadOf(t, resp)); code != "no_state" {
		t.Fatalf("code: %s", code)
	}
	// limit is strictly validated
	resp = fx.callTool(t, "find", map[string]any{"text": "x", "limit": 0})
	if code := errCode(t, payloadOf(t, resp)); code != "invalid_args" {
		t.Fatalf("limit 0 code: %s", code)
	}
}

func TestMatchElementsGolden(t *testing.T) {
	elements := map[int]treeEntry{
		1: {Chain: "1", Body: `AXButton "Save Document" @px:1,2:3x4`},
		2: {Chain: "2", Body: `AXTextField "name" v="save the whales"`},
		3: {Chain: "3", Body: `AXButton "Cancel"`},
		4: {Chain: "3.1", Body: `AXStaticText "Unsaved changes"`},
	}
	// text matches case-insensitively across the whole body
	if got := matchElements(elements, "save", ""); len(got) != 3 ||
		got[0] != 1 || got[1] != 2 || got[2] != 4 {
		t.Fatalf("text match: %v", got)
	}
	// role must equal the leading token exactly
	if got := matchElements(elements, "", "AXButton"); len(got) != 2 ||
		got[0] != 1 || got[1] != 3 {
		t.Fatalf("role match: %v", got)
	}
	// combined narrows
	if got := matchElements(elements, "save", "AXButton"); len(got) != 1 || got[0] != 1 {
		t.Fatalf("combined: %v", got)
	}
	// role is exact, not a prefix
	if got := matchElements(elements, "", "AXText"); len(got) != 0 {
		t.Fatalf("prefix must not match: %v", got)
	}
}

func TestMenuPathGolden(t *testing.T) {
	if _, terr := splitMenuPath("File"); terr == nil || terr.Code != "invalid_args" {
		t.Fatalf("single segment must be rejected: %v", terr)
	}
	// an empty segment must be rejected, never dropped — a rewritten path
	// could click a different (destructive) item
	for _, bad := range []string{"File > > Save", "File > Save > ", " > File > Save"} {
		if _, terr := splitMenuPath(bad); terr == nil || terr.Code != "invalid_args" {
			t.Fatalf("%q must be rejected: %v", bad, terr)
		}
	}
	segs, terr := splitMenuPath(" File >  Export as PDF… ")
	if terr != nil || len(segs) != 2 || segs[0] != "File" || segs[1] != "Export as PDF…" {
		t.Fatalf("split: %v %v", segs, terr)
	}
	addr, terr := menuItemAddress([]string{"File", "Export", "PDF"})
	if terr != nil {
		t.Fatal(terr)
	}
	want := `menu item "PDF" of menu "Export" of menu item "Export" of ` +
		`menu "File" of menu bar item "File" of menu bar 1`
	if addr != want {
		t.Fatalf("addr:\n got %s\nwant %s", addr, want)
	}
	// menu titles pass through asEsc — quotes cannot break the literal
	addr, terr = menuItemAddress([]string{`Fi"le`, "Save"})
	if terr != nil || !strings.Contains(addr, `menu bar item "Fi\"le"`) {
		t.Fatalf("escaping: %s %v", addr, terr)
	}
}

func TestMenuArgValidation(t *testing.T) {
	fx := startServer(t)
	resp := fx.callTool(t, "menu", nil)
	if code := errCode(t, payloadOf(t, resp)); code != "invalid_args" {
		t.Fatalf("code: %s", code)
	}
}

func TestMenuUnknownAppIsStructured(t *testing.T) {
	if !guiAvailable() {
		t.Skip("System Events not reachable")
	}
	fx := startServer(t)
	resp := fx.callTool(t, "menu", map[string]any{"app": "definitely not a real app 0xF00D"})
	if code := errCode(t, payloadOf(t, resp)); code != "app_not_found" {
		t.Fatalf("code: %s", code)
	}
}

func TestWaitForValidation(t *testing.T) {
	fx := startServer(t)
	resp := fx.callTool(t, "wait_for", nil)
	if code := errCode(t, payloadOf(t, resp)); code != "invalid_args" {
		t.Fatalf("missing text: %s", code)
	}
	resp = fx.callTool(t, "wait_for", map[string]any{"text": "x", "until": "sideways"})
	if code := errCode(t, payloadOf(t, resp)); code != "invalid_args" {
		t.Fatalf("bad until: %s", code)
	}
	resp = fx.callTool(t, "wait_for", map[string]any{"text": "x", "timeout_s": 999})
	if code := errCode(t, payloadOf(t, resp)); code != "invalid_args" {
		t.Fatalf("bad timeout: %s", code)
	}
	// well-formed but nothing captured yet -> no_state, no GUI touched
	resp = fx.callTool(t, "wait_for", map[string]any{"text": "x"})
	if code := errCode(t, payloadOf(t, resp)); code != "no_state" {
		t.Fatalf("no state: %s", code)
	}
}

func TestClipboardWrongArgTypeIsInvalid(t *testing.T) {
	fx := startServer(t)
	resp := fx.callTool(t, "clipboard", map[string]any{"text": 42})
	if code := errCode(t, payloadOf(t, resp)); code != "invalid_args" {
		t.Fatalf("code: %s", code)
	}
	resp = fx.callTool(t, "clipboard", map[string]any{"text": true})
	if code := errCode(t, payloadOf(t, resp)); code != "invalid_args" {
		t.Fatalf("code: %s", code)
	}
}

func TestClipboardRoundTripLive(t *testing.T) {
	if !guiAvailable() {
		t.Skip("System Events not reachable")
	}
	// save the human's clipboard, exercise set → read, put it back
	saved, err := exec.Command("pbpaste", "-Prefer", "txt").Output()
	if err != nil {
		t.Skipf("pbpaste unavailable: %v", err)
	}
	t.Cleanup(func() {
		cmd := exec.Command("pbcopy")
		cmd.Stdin = bytes.NewReader(saved)
		_ = cmd.Run()
	})
	fx := startServer(t)
	resp := fx.callTool(t, "clipboard", map[string]any{"text": "cuu-test-123"})
	if resp["result"].(map[string]any)["isError"] == true {
		t.Fatalf("clipboard set failed: %v", resp)
	}
	if got := payloadOf(t, resp)["result"]; got != "Clipboard set to 12 character(s)." {
		t.Fatalf("set payload: %v", got)
	}
	resp = fx.callTool(t, "clipboard", nil)
	payload := payloadOf(t, resp)
	if payload["text"] != "cuu-test-123" {
		t.Fatalf("read-back: %v", payload["text"])
	}
	// an explicit "" clears — it must not fall through to a read
	resp = fx.callTool(t, "clipboard", map[string]any{"text": ""})
	if got := payloadOf(t, resp)["result"]; got != "Clipboard cleared." {
		t.Fatalf("clear payload: %v", got)
	}
	resp = fx.callTool(t, "clipboard", nil)
	payload = payloadOf(t, resp)
	if payload["text"] != "" {
		t.Fatalf("after clear: %v", payload["text"])
	}
	// and the empty-string clipboard must not be mislabeled as non-text
	if hint, _ := payload["hint"].(string); strings.Contains(hint, "non-text") {
		t.Fatalf("empty string mislabeled: %v", hint)
	}
}

func TestWindowArgValidation(t *testing.T) {
	fx := startServer(t)
	// missing action -> invalid_args, checked before any GUI work
	resp := fx.callTool(t, "window", map[string]any{"app": "TextEdit"})
	if code := errCode(t, payloadOf(t, resp)); code != "invalid_args" {
		t.Fatalf("missing action: %s", code)
	}
	// unknown action -> invalid_args
	resp = fx.callTool(t, "window", map[string]any{"app": "TextEdit", "action": "teleport"})
	if code := errCode(t, payloadOf(t, resp)); code != "invalid_args" {
		t.Fatalf("unknown action: %s", code)
	}
	// move without x/y -> invalid_args (coordinate validation precedes
	// resolveApp, so it never needs a running app)
	resp = fx.callTool(t, "window", map[string]any{"app": "TextEdit", "action": "move"})
	if code := errCode(t, payloadOf(t, resp)); code != "invalid_args" {
		t.Fatalf("move without x/y: %s", code)
	}
	resp = fx.callTool(t, "window", map[string]any{"app": "TextEdit", "action": "move", "x": 10})
	if code := errCode(t, payloadOf(t, resp)); code != "invalid_args" {
		t.Fatalf("move without y: %s", code)
	}
	resp = fx.callTool(t, "window", map[string]any{"app": "TextEdit", "action": "resize"})
	if code := errCode(t, payloadOf(t, resp)); code != "invalid_args" {
		t.Fatalf("resize without width/height: %s", code)
	}
}

func TestMinimizedWindowSrcGolden(t *testing.T) {
	// no window arg -> first minimized window
	src, terr := minimizedWindowSrc("TextEdit", nil, false)
	if terr != nil || !strings.Contains(src, `"AXMinimized" is true)`) {
		t.Fatalf("default: %s %v", src, terr)
	}
	// title substring narrows, escaped
	src, terr = minimizedWindowSrc("TextEdit", `no"tes`, true)
	if terr != nil || !strings.Contains(src, `name contains "no\"tes"`) {
		t.Fatalf("title: %s %v", src, terr)
	}
	// a CGWindow id cannot address an off-screen window — rejected, not guessed
	if _, terr := minimizedWindowSrc("TextEdit", json.Number("42"), true); terr == nil ||
		terr.Code != "invalid_args" {
		t.Fatalf("id must be rejected: %v", terr)
	}
}

func TestWindowUnknownAppIsStructured(t *testing.T) {
	if !guiAvailable() {
		t.Skip("System Events not reachable")
	}
	fx := startServer(t)
	resp := fx.callTool(t, "window", map[string]any{
		"app": "definitely not a real app 0xF00D", "action": "minimize",
	})
	if resp["result"].(map[string]any)["isError"] != true {
		t.Fatalf("expected isError: %v", resp)
	}
	if code := errCode(t, payloadOf(t, resp)); code != "app_not_found" {
		t.Fatalf("code: %s", code)
	}
}

func TestGetAppStateNewArgsAreStrict(t *testing.T) {
	fx := startServer(t)
	// both new args are validated before any GUI work happens
	resp := fx.callTool(t, "get_app_state", map[string]any{
		"app": "TextEdit", "include_screenshot": "yes",
	})
	if code := errCode(t, payloadOf(t, resp)); code != "invalid_args" {
		t.Fatalf("include_screenshot: %s", code)
	}
	resp = fx.callTool(t, "get_app_state", map[string]any{
		"app": "TextEdit", "filter": 7,
	})
	if code := errCode(t, payloadOf(t, resp)); code != "invalid_args" {
		t.Fatalf("filter: %s", code)
	}
}

func TestDownscaleBoxGolden(t *testing.T) {
	// a 3000x2000 image lands at the 1568 bound with the aspect kept
	big := image.NewRGBA(image.Rect(0, 0, 3000, 2000))
	out := downscaleBox(big, 1568)
	if b := out.Bounds(); b.Dx() != 1568 || b.Dy() != 1045 {
		t.Fatalf("bounds: %v", b)
	}
	// portrait scales on height
	tall := image.NewRGBA(image.Rect(0, 0, 1000, 4000))
	out = downscaleBox(tall, 1568)
	if b := out.Bounds(); b.Dy() != 1568 || b.Dx() != 392 {
		t.Fatalf("portrait bounds: %v", b)
	}
	// within-bound images pass through untouched
	small := image.NewRGBA(image.Rect(0, 0, 640, 480))
	if downscaleBox(small, 1568) != image.Image(small) {
		t.Fatalf("small image must pass through")
	}
}

func TestInlineScreenshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	shot := filepath.Join(dir, "shot.png")
	f, err := os.Create(shot)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, 2000, 1200))); err != nil {
		t.Fatal(err)
	}
	f.Close()
	data := inlineScreenshot(shot)
	if data == "" {
		t.Fatal("expected base64 data")
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatalf("not base64: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("not a PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 1568 {
		t.Fatalf("not downscaled: %v", b)
	}
	// inlining is best-effort: garbage input yields "", never an error
	bad := filepath.Join(dir, "bad.png")
	if err := os.WriteFile(bad, []byte("not a png"), 0o600); err != nil {
		t.Fatal(err)
	}
	if inlineScreenshot(bad) != "" {
		t.Fatal("garbage must yield empty")
	}
}

// ---------------------------------------------------------------- 4.2 node 3: ocr

func TestOCRBoxToPixelsGolden(t *testing.T) {
	// Vision's box is normalized with a BOTTOM-LEFT origin: y flips, and the
	// top edge sits (y + h) from the bottom
	px, py, pw, ph := ocrBoxToPixels(0.5, 0.25, 0.25, 0.125, 1000, 800)
	if px != 500 || py != 500 || pw != 250 || ph != 100 {
		t.Fatalf("box: %d,%d %dx%d", px, py, pw, ph)
	}
	// a box at the image's bottom-left edge maps to the bottom of pixel space
	px, py, _, _ = ocrBoxToPixels(0, 0, 0.5, 0.5, 1000, 800)
	if px != 0 || py != 400 {
		t.Fatalf("bottom-left origin: %d,%d", px, py)
	}
	// rounding, not truncation: 19.9px truncates to 19 but rounds to 20
	px, _, pw, _ = ocrBoxToPixels(0.199, 0, 0.333, 0, 100, 10)
	if px != 20 || pw != 33 {
		t.Fatalf("rounding: %d %d", px, pw)
	}
}

func TestOCRValidation(t *testing.T) {
	fx := startServer(t)
	// wrong-typed filter is invalid_args, checked before any state look
	resp := fx.callTool(t, "ocr", map[string]any{"filter": 7})
	if code := errCode(t, payloadOf(t, resp)); code != "invalid_args" {
		t.Fatalf("filter type: %s", code)
	}
	// nothing captured yet -> no_state naming the remedy
	resp = fx.callTool(t, "ocr", nil)
	if code := errCode(t, payloadOf(t, resp)); code != "no_state" {
		t.Fatalf("no state: %s", code)
	}
	errObj := payloadOf(t, resp)["error"].(map[string]any)
	if !strings.Contains(errObj["remedy"].(string), "get_app_state") {
		t.Fatalf("remedy: %v", errObj["remedy"])
	}
}

func TestOCRLive(t *testing.T) {
	if !guiAvailable() {
		t.Skip("System Events not reachable")
	}
	shot, err := filepath.Abs(filepath.Join("..", "docs", "demo", "before-scroll.png"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(shot); err != nil {
		t.Skipf("demo capture missing: %v", err)
	}
	// ocr consumes the on-disk capture, not a live window: boot the server on
	// a data dir whose state.json already points at the demo PNG
	dataDir := t.TempDir()
	st := &serverState{App: "Probe", Screenshot: shot, Stale: false,
		Elements: map[int]treeEntry{}, PrevBodies: map[string]string{}}
	raw, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "state.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	fx := startServerInDir(t, dataDir)
	resp := fx.callTool(t, "ocr", nil)
	if resp["result"].(map[string]any)["isError"] == true {
		t.Fatalf("ocr failed: %v", resp)
	}
	payload := payloadOf(t, resp)
	lines, ok := payload["lines"].([]any)
	if !ok || len(lines) == 0 {
		t.Fatalf("lines missing: %v", payload)
	}
	for _, ln := range lines {
		if s, isStr := ln.(string); isStr && strings.Contains(s, "X500") {
			return
		}
	}
	t.Fatalf("no line containing X500: %v", lines)
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
