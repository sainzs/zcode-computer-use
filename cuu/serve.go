package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

func execLookPath(name string) (string, error) {
	return exec.LookPath(name)
}

// ---------------------------------------------------------------- MCP plumbing

type jsonRpcError struct {
	Code    int
	Message string
}

func (e *jsonRpcError) Error() string { return e.Message }

// dispatch handles one MCP request. Params arrive pre-decoded with
// UseNumber; a present-but-not-object params raises -32602 (an empty list
// must not pass as {}).
func dispatch(method string, params any, hasParams bool) (any, *jsonRpcError) {
	if hasParams {
		if _, ok := params.(map[string]any); !ok {
			return nil, &jsonRpcError{-32602, "params must be an object"}
		}
	}
	pa, _ := params.(map[string]any)
	switch method {
	case "initialize":
		version := latestProtocol
		if req, ok := pa["protocolVersion"].(string); ok &&
			(req == supportedProtocol1 || req == supportedProtocol2) {
			version = req
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
		}, nil
	case "tools/list":
		var tools any
		if err := json.Unmarshal([]byte(toolsManifest), &tools); err != nil {
			return nil, &jsonRpcError{-32603, "internal: tool manifest corrupt"}
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		name, _ := pa["name"].(string)
		if name == "" {
			return nil, &jsonRpcError{-32602, fmt.Sprintf("unknown tool %s", pyRepr(pa["name"]))}
		}
		if name != "perform_secondary_action" && !handlerExists(name) {
			return nil, &jsonRpcError{-32602, fmt.Sprintf("unknown tool %s", pyRepr(name))}
		}
		arguments, hasArgs := pa["arguments"]
		if !hasArgs {
			arguments = map[string]any{}
		}
		argsMap, ok := arguments.(map[string]any)
		if !ok {
			return nil, &jsonRpcError{-32602, "arguments must be an object"}
		}
		st := loadState()
		payload, terr := runTool(name, st, argsMap)
		if terr == nil {
			content := []map[string]any{
				{"type": "text", "text": dumpJSON(payload)},
			}
			// opt-in inline screenshot: an MCP image content block after the
			// text, so image-rendering clients skip the file-read round trip.
			// Best-effort — the text payload already carries the PNG path.
			if name == "get_app_state" && argsMap["include_screenshot"] == true &&
				st.Screenshot != "" {
				if data := inlineScreenshot(st.Screenshot); data != "" {
					content = append(content, map[string]any{
						"type": "image", "data": data, "mimeType": "image/png",
					})
				}
			}
			return map[string]any{"content": content}, nil
		}
		logEvent("tool_error", map[string]any{"tool": name, "code": terr.Code, "message": terr.Message})
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": dumpJSONCompact(terr.payload())},
			},
			"isError": true,
		}, nil
	case "logging/setLevel":
		return map[string]any{}, nil // accepted; the JSONL file log is always on
	case "ping":
		return map[string]any{}, nil
	case "prompts/list":
		return map[string]any{"prompts": []any{}}, nil
	case "resources/list":
		return map[string]any{"resources": []any{}}, nil
	default:
		return nil, &jsonRpcError{-32601, fmt.Sprintf("method not found: %s", method)}
	}
}

// clientGone unwinds the serve loop when stdout breaks, exiting cleanly no
// matter where we were in the protocol.
type clientGone struct{ err error }

func (e *clientGone) Error() string { return e.err.Error() }

func writeResponse(w *bufio.Writer, response map[string]any) error {
	line, err := json.Marshal(response)
	if err != nil {
		line = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"internal: response marshal failed"}}`)
	}
	if _, werr := w.Write(append(line, '\n')); werr != nil {
		return &clientGone{werr}
	}
	if werr := w.Flush(); werr != nil {
		return &clientGone{werr}
	}
	return nil
}

func serve() int {
	logEvent("server_start", map[string]any{"version": serverVersion, "pid": os.Getpid()})
	// The fixture-stdout lesson from 3.x: writes must flush per line and the
	// final flush must happen before exit, or the last response is lost to a
	// client that closed its read end first.
	w := bufio.NewWriter(os.Stdout)
	defer func() {
		_ = w.Flush()
	}()
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var decoded any
		// UseNumber is load-bearing: JSON ints must stay distinguishable from
		// floats (1.9 must never target element 1), like Python's json.loads
		// decoding ints and floats into different types
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&decoded); err != nil {
			logEvent("bad_json", map[string]any{"line": truncateRunes(line, 120)})
			if werr := writeResponse(w, map[string]any{
				"jsonrpc": "2.0", "id": nil,
				"error": map[string]any{"code": -32700,
					"message": fmt.Sprintf("parse error: %s", err)},
			}); werr != nil {
				return serveExit(werr)
			}
			continue
		}
		msg, isObject := decoded.(map[string]any)
		if !isObject {
			// a top-level array (or scalar) is a batch — unsupported
			if werr := writeResponse(w, map[string]any{
				"jsonrpc": "2.0", "id": nil,
				"error": map[string]any{"code": -32600,
					"message": "invalid request: batches not supported"},
			}); werr != nil {
				return serveExit(werr)
			}
			continue
		}
		if v, ok := msg["jsonrpc"]; ok {
			if s, _ := v.(string); s != "2.0" {
				if werr := writeResponse(w, map[string]any{
					"jsonrpc": "2.0", "id": msg["id"],
					"error": map[string]any{"code": -32600,
						"message": fmt.Sprintf("unsupported jsonrpc version: %s", pyRepr(v))},
				}); werr != nil {
					return serveExit(werr)
				}
				continue
			}
		}
		if _, hasID := msg["id"]; !hasID {
			method, _ := msg["method"].(string)
			if method != "notifications/initialized" && method != "notifications/cancelled" {
				logEvent("notification_ignored", map[string]any{"method": method})
			}
			continue
		}
		method, methodIsStr := msg["method"].(string)
		if !methodIsStr {
			if werr := writeResponse(w, map[string]any{
				"jsonrpc": "2.0", "id": msg["id"],
				"error": map[string]any{"code": -32600,
					"message": "invalid request: missing method"},
			}); werr != nil {
				return serveExit(werr)
			}
			continue
		}
		params, hasParams := msg["params"]
		// absent params -> {}; present-but-not-object -> dispatch raises -32602
		var response map[string]any
		if !hasParams {
			params = map[string]any{}
			hasParams = false
		}
		result, rerr := dispatch(method, params, hasParams)
		if rerr == nil {
			response = map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": result}
		} else {
			response = map[string]any{"jsonrpc": "2.0", "id": msg["id"],
				"error": map[string]any{"code": rerr.Code, "message": rerr.Message}}
		}
		if werr := writeResponse(w, response); werr != nil {
			return serveExit(werr)
		}
	}
	logEvent("server_stop", nil)
	return 0
}

func serveExit(werr error) int {
	if _, gone := werr.(*clientGone); gone {
		logEvent("client_disconnected", nil)
		return 0
	}
	return 0
}

func truncateRunes(s string, n int) string {
	if pyLen(s) <= n {
		return s
	}
	return pySlice(s, 0, n)
}

// ---------------------------------------------------------------- selftest

func selftest() int {
	ok := true
	probes := []struct {
		name  string
		check func() (bool, string)
	}{
		{"osascript", func() (bool, string) {
			v, terr := osascript(`return "1"`, "", 0)
			return terr == nil && v == "1", ""
		}},
		{"cliclick", func() (bool, string) {
			_, err := execLookPath("cliclick")
			return err == nil, ""
		}},
		{"screencapture", func() (bool, string) {
			_, err := execLookPath("screencapture")
			return err == nil, ""
		}},
		{"Accessibility (AX query)", func() (bool, string) {
			return accessibilityProbe(), ""
		}},
		{"CGWindowList (JXA)", func() (bool, string) {
			w, terr := windowList()
			return terr == nil && w != nil, ""
		}},
		{"Quartz scroll (pyobjc)", func() (bool, string) {
			if _, err := execLookPath("/usr/bin/python3"); err != nil {
				return false, ""
			}
			return quartzProbe(), ""
		}},
	}
	for _, p := range probes {
		good := false
		detail := ""
		func() {
			defer func() {
				if r := recover(); r != nil {
					good = false
				}
			}()
			good, detail = p.check()
		}()
		status := "FAIL"
		if good {
			status = "ok"
		}
		fmt.Printf("  %s: %s%s\n", p.name, status, detail)
		ok = ok && good
	}
	fmt.Printf("data dir: %s\n", dataDir())
	fmt.Printf("log: %s\n", logPath())
	fmt.Println("Permissions live on the HOST app (ZCode or your terminal):")
	fmt.Println("  Accessibility    > System Settings > Privacy & Security > Accessibility")
	fmt.Println("  Screen Recording > System Settings > Privacy & Security > Screen Recording")
	fmt.Println("Restart the host app after granting either one.")
	if ok {
		return 0
	}
	return 1
}

// accessibilityProbe is a real AX query — exactly what fails without the
// grant, so the selftest can name the missing permission instead of a
// cryptic osascript error later.
func accessibilityProbe() bool {
	_, terr := osascript(
		"tell application \"System Events\" to return (name of first application process)", "", 0)
	if terr != nil {
		return false
	}
	return true
}

func quartzProbe() bool {
	res := runCmd(15*time.Second, "", "/usr/bin/python3", "-c", "import Quartz")
	return res != nil && res.ExitCode == 0
}
