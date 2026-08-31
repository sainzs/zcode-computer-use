package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	serverName         = "zcode-computer-use"
	serverVersion      = "4.2.0"
	supportedProtocol1 = "2025-03-26"
	supportedProtocol2 = "2025-06-18"
	latestProtocol     = supportedProtocol2

	osaTimeoutSec       = 25  // per AppleScript invocation
	scrollPixelsPerPage = 450 // wheel pixels per "page"
	maxElements         = 350
	maxDepthDefault     = 8
	maxDepthHard        = 12
	keepScreenshots     = 24
	keypageDelayMs      = 250  // settle after activation before capture
	typeKeysMax         = 2000 // keystrokes method is slow; cap it
	selectKeysMax       = 400  // keyboard selection walk is slower still
)

// ---------------------------------------------------------------- data dir + log

func dataDir() string {
	env := os.Getenv("ZCODE_CUA_DATA")
	if env == "" {
		env = os.Getenv("ZCODE_PLUGIN_DATA")
	}
	base := env
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "/tmp/" + serverName
		}
		base = filepath.Join(home, ".cache", serverName)
	}
	_ = os.MkdirAll(base, 0o700)
	_ = os.Chmod(base, 0o700)
	return base
}

func logPath() string { return filepath.Join(dataDir(), "server.log") }

// logEvent writes one grep-able JSON object per line. Failures are swallowed:
// the log must never take the server down.
func logEvent(event string, fields map[string]any) {
	rec := map[string]any{"ts": pyFloatStr(math.Round(timeFloatSeconds()*1000) / 1000), "event": event}
	for k, v := range fields {
		rec[k] = v
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(logPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

func timeFloatSeconds() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

// ---------------------------------------------------------------- subprocesses

type runResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// runCmd executes a subprocess, returning nil on failure or timeout (the
// failure is logged; callers treat nil as "the tool is not usable").
func runCmd(timeout time.Duration, inputText string, argv ...string) *runResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(inputText)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		logEvent("subprocess_failed", map[string]any{
			"cmd":   argv[:min(3, len(argv))],
			"error": err.Error(),
		})
		if ctx.Err() == context.DeadlineExceeded || stdout.Len() == 0 && stderr.Len() == 0 && ctx.Err() != nil {
			return nil
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return &runResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: ee.ExitCode()}
		}
		return nil
	}
	return &runResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: 0}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// osascript runs one AppleScript (or JXA) invocation and returns stdout with
// trailing newlines stripped, or raises a classified ToolError.
func osascript(source string, lang string, timeout time.Duration) (string, *ToolError) {
	if lang == "" {
		lang = "AppleScript"
	}
	if timeout == 0 {
		timeout = osaTimeoutSec * time.Second
	}
	res := runCmd(timeout, "", "osascript", "-l", lang, "-e", source)
	if res == nil || res.ExitCode != 0 {
		errText := ""
		if res != nil {
			errText = strings.TrimSpace(res.Stderr)
			if len(errText) > 300 {
				errText = errText[len(errText)-300:]
			}
		}
		if errText == "" {
			errText = "timeout"
		}
		return "", classifyOsascriptError(errText)
	}
	return strings.TrimRight(res.Stdout, "\n"), nil
}

// ---------------------------------------------------------------- text helpers

// asEsc escapes a string for an AppleScript double-quoted literal.
func asEsc(text string) (string, *ToolError) {
	out := text
	out = strings.ReplaceAll(out, "\\", "\\\\")
	out = strings.ReplaceAll(out, "\"", "\\\"")
	out = strings.ReplaceAll(out, "\n", "\\n")
	out = strings.ReplaceAll(out, "\r", "\\r")
	out = strings.ReplaceAll(out, "\t", "\\t")
	leftover := false
	for _, c := range out {
		if c < 0x20 || c == 0x7F {
			leftover = true
			break
		}
	}
	if leftover {
		return "", toolErr(
			"invalid_args",
			"control characters in the text are not representable in an "+
				"AppleScript literal",
			"strip control characters or use type_text method=paste with "+
				"clean text")
	}
	return out, nil
}

// snippet renders one line of prose, newlines visible, capped with an ellipsis.
func snippet(text string, limit int) string {
	if limit <= 0 {
		limit = 60
	}
	t := strings.ReplaceAll(text, "\n", "\\n")
	t = strings.ReplaceAll(t, "\r", "")
	return pyTruncate(t, limit)
}

// dumpJSON renders a payload the way the Python server did:
// json.dumps(..., ensure_ascii=False, indent=1) — one-space indent.
func dumpJSON(v any) string {
	line, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	var buf bytes.Buffer
	if err := indentJSON(&buf, line, " "); err != nil {
		return string(line)
	}
	return buf.String()
}

// indentJSON re-renders compact JSON with the given indent, matching
// Python's separators (",\n<ind>" and ": ").
func indentJSON(buf *bytes.Buffer, compact []byte, indent string) error {
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", indent); err != nil {
		return err
	}
	buf.Write(out.Bytes())
	return nil
}

// dumpJSONCompact is json.dumps(..., ensure_ascii=False) — used for error payloads.
func dumpJSONCompact(v any) string {
	line, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(line)
}

// sortedIntKeys returns map keys ascending — Go map iteration is random and
// the tree must render in index order like the Python dict did.
func sortedIntKeys(m map[int]struct{}) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}
