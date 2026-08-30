package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------- windows (JXA)

const windowListJXA = `
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
`

type cgWindow struct {
	Owner string  `json:"owner"`
	Name  string  `json:"name"`
	ID    int64   `json:"id"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	W     float64 `json:"w"`
	H     float64 `json:"h"`
}

func windowList() ([]cgWindow, *ToolError) {
	raw, terr := osascript(windowListJXA, "JavaScript", 0)
	if terr != nil {
		return nil, terr
	}
	if raw == "" {
		return []cgWindow{}, nil
	}
	var wins []cgWindow
	if err := json.Unmarshal([]byte(raw), &wins); err != nil {
		return nil, toolErr("internal", "window list parse failed: "+err.Error(), "")
	}
	return wins, nil
}

const mainDisplayJXA = `
ObjC.import('CoreGraphics');
const r = $.CGDisplayBounds($.CGMainDisplayID());
JSON.stringify({x: $.CGRectGetMinX(r), y: $.CGRectGetMinY(r),
                w: $.CGRectGetWidth(r), h: $.CGRectGetHeight(r)});
`

type displayBounds struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

func mainDisplay() (displayBounds, *ToolError) {
	raw, terr := osascript(mainDisplayJXA, "JavaScript", 0)
	if terr != nil {
		return displayBounds{}, terr
	}
	var d displayBounds
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return displayBounds{}, toolErr("internal", "display bounds parse failed: "+err.Error(), "")
	}
	return d, nil
}

// windowsOf returns on-screen layer-0 windows owned by owner, front-to-back
// (the list comes back front-to-back, so the first match is the key window).
func windowsOf(owner string) ([]cgWindow, *ToolError) {
	low := strings.ToLower(owner)
	wins, terr := windowList()
	if terr != nil {
		return nil, terr
	}
	out := []cgWindow{}
	for _, w := range wins {
		if strings.ToLower(w.Owner) == low && w.W*w.H > 100 {
			out = append(out, w)
		}
	}
	return out, nil
}

// pickWindow resolves the `window` argument against the app's on-screen
// windows: id -> CGWindowID match (hard error if gone); string ->
// case-insensitive substring of the title; absent -> frontmost window.
func pickWindow(owner string, window any, hasWindow bool) (cgWindow, *ToolError) {
	wins, terr := windowsOf(owner)
	if terr != nil {
		return cgWindow{}, terr
	}
	if len(wins) == 0 {
		return cgWindow{}, toolErr("window_gone",
			fmt.Sprintf("no on-screen windows for %q", owner),
			"check the app is running with a visible window")
	}
	if !hasWindow || window == nil || window == "" {
		return wins[0], nil
	}
	switch v := window.(type) {
	case bool:
		return cgWindow{}, toolErr("invalid_args",
			"window must be an id (integer) or a title substring",
			"call list_windows to see ids and titles")
	case json.Number:
		if !jsonNumberIsInt(string(v)) {
			return cgWindow{}, toolErr("invalid_args",
				"window must be an id (integer) or a title substring",
				"call list_windows to see ids and titles")
		}
		id, _ := strconv.ParseInt(string(v), 10, 64)
		for _, w := range wins {
			if w.ID == id {
				return w, nil
			}
		}
		return cgWindow{}, toolErr("window_gone",
			fmt.Sprintf("window id %s is gone (closed/minimized)", string(v)),
			"call get_app_state again or list_windows")
	case string:
		needle := strings.ToLower(v)
		for _, w := range wins {
			if strings.Contains(strings.ToLower(w.Name), needle) {
				return w, nil
			}
		}
		return cgWindow{}, toolErr("window_gone",
			fmt.Sprintf("no window title matching %q", v),
			"call list_windows to see current titles")
	default:
		return cgWindow{}, toolErr("invalid_args",
			"window must be an id (integer) or a title substring",
			"call list_windows to see ids and titles")
	}
}

// ---------------------------------------------------------------- png size

func pngSize(path string) (int, int, error) {
	data := make([]byte, 24)
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, fmt.Errorf("not a PNG capture")
	}
	n, err := f.Read(data)
	f.Close()
	if err != nil || n < 24 {
		return 0, 0, fmt.Errorf("not a PNG capture")
	}
	pngMagic := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	if string(data[:8]) != string(pngMagic) || string(data[12:16]) != "IHDR" {
		return 0, 0, fmt.Errorf("not a PNG capture")
	}
	return int(binary.BigEndian.Uint32(data[16:20])), int(binary.BigEndian.Uint32(data[20:24])), nil
}

// ---------------------------------------------------------------- app queries

const runningAppsSrc = `
tell application "System Events"
    set names to name of every process whose background only is false
    set frontName to name of first application process whose frontmost is true
end tell
set d to AppleScript's text item delimiters
set AppleScript's text item delimiters to ","
set joined to names as text
set AppleScript's text item delimiters to d
return frontName & "|" & joined
`

func runningApps() ([]string, string, *ToolError) {
	raw, terr := osascript(runningAppsSrc, "", 0)
	if terr != nil {
		return nil, "", terr
	}
	front := raw
	rest := ""
	if i := strings.Index(raw, "|"); i >= 0 {
		front, rest = raw[:i], raw[i+1:]
	}
	apps := []string{}
	for _, a := range strings.Split(rest, ",") {
		if t := strings.TrimSpace(a); t != "" {
			apps = append(apps, t)
		}
	}
	return apps, strings.TrimSpace(front), nil
}

func resolveApp(app string) string {
	apps, _, _ := runningApps()
	low := strings.ToLower(app)
	for _, name := range apps {
		if strings.ToLower(name) == low {
			return name
		}
	}
	for _, name := range apps {
		if strings.Contains(strings.ToLower(name), low) {
			return name
		}
	}
	return ""
}

func activateApp(name string) *ToolError {
	escaped, terr := asEsc(name)
	if terr != nil {
		return terr
	}
	if _, terr := osascript(fmt.Sprintf("tell application \"%s\" to activate", escaped), "", 0); terr != nil {
		return terr
	}
	src := fmt.Sprintf("tell application \"System Events\" to set frontmost of process \"%s\" to true", escaped)
	if _, terr := osascript(src, "", 0); terr != nil {
		return terr
	}
	time.Sleep(keypageDelayMs * time.Millisecond)
	return nil
}

// ---------------------------------------------------------------- SE window mapping

type seWindow struct {
	Index int
	Name  string
	X, Y  int
	W, H  int
}

func seWindowsDump(processName string) ([]seWindow, *ToolError) {
	escaped, terr := asEsc(processName)
	if terr != nil {
		return nil, terr
	}
	src := "tell application \"System Events\"\n" +
		"    set wins to windows of process \"" + escaped + "\"\n" +
		"    set out to \"\"\n" +
		"    repeat with i from 1 to (count of wins)\n" +
		"        set w to item i of wins\n" +
		"        set p to position of w\n" +
		"        set s to size of w\n" +
		"        set nm to \"\"\n" +
		"        try\n" +
		"            set nm to (name of w) as text\n" +
		"        end try\n" +
		"        set out to out & i & \"|\" & nm & \"|\" & ((item 1 of p) as " +
		"text) & \",\" & ((item 2 of p) as text) & \"|\" & ((item 1 of s) as " +
		"text) & \"x\" & ((item 2 of s) as text) & linefeed\n" +
		"    end repeat\n" +
		"    return out\n" +
		"end tell"
	raw, terr := osascript(src, "", 0)
	if terr != nil {
		return nil, terr
	}
	out := []seWindow{}
	for _, ln := range strings.Split(raw, "\n") {
		parts := strings.Split(ln, "|")
		if len(parts) != 4 || !allDigits(strings.TrimSpace(parts[0])) {
			continue
		}
		xy := strings.Split(parts[2], ",")
		wh := strings.Split(parts[3], "x")
		if len(xy) != 2 || len(wh) != 2 {
			continue
		}
		px, err1 := atoiFloat(xy[0])
		py, err2 := atoiFloat(xy[1])
		sw, err3 := atoiFloat(wh[0])
		sh, err4 := atoiFloat(wh[1])
		if err1 || err2 || err3 || err4 {
			continue
		}
		idx := 0
		fmt.Sscanf(strings.TrimSpace(parts[0]), "%d", &idx)
		out = append(out, seWindow{Index: idx, Name: parts[1], X: px, Y: py, W: sw, H: sh})
	}
	return out, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func atoiFloat(s string) (int, bool) {
	s = strings.TrimSpace(s)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return int(f), true
}

// seWindowSrc returns the System Events address fragment for the window
// captured as cgWin, plus whether the match was real. Match order: title,
// then position (CG and SE both report global top-left-origin points;
// +/-2pt tolerance covers rounding). Falls back to `window 1` with matched
// false — the caller records the mismatch in the state header (the Python
// fell back silently; the fallback note is the 4.0 fix).
func seWindowSrc(processName string, cgWin cgWindow) (string, bool, *ToolError) {
	seWins, terr := seWindowsDump(processName)
	if terr != nil {
		return "", false, terr
	}
	escaped, terr := asEsc(processName)
	if terr != nil {
		return "", false, terr
	}
	title := strings.TrimSpace(cgWin.Name)
	if title != "" {
		for _, w := range seWins {
			if strings.TrimSpace(w.Name) == title {
				return fmt.Sprintf("(window %d of process \"%s\")", w.Index, escaped), true, nil
			}
		}
	}
	for _, w := range seWins {
		if absInt(w.X-int(cgWin.X)) <= 2 && absInt(w.Y-int(cgWin.Y)) <= 2 &&
			absInt(w.W-int(cgWin.W)) <= 2 {
			return fmt.Sprintf("(window %d of process \"%s\")", w.Index, escaped), true, nil
		}
	}
	return fmt.Sprintf("(window 1 of process \"%s\")", escaped), false, nil
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
