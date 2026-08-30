package main

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ---------------------------------------------------------------- keyboard

var keyCodes = map[string]int{
	"return": 36, "enter": 36, "tab": 48, "escape": 53, "esc": 53,
	"space": 49, "delete": 51, "backspace": 51, "forwarddelete": 117,
	"home": 115, "end": 119, "pageup": 116, "page_up": 116,
	"pagedown": 121, "page_down": 121,
	"left": 123, "right": 124, "down": 125, "up": 126,
	"help": 114, "f1": 122, "f2": 120, "f3": 99, "f4": 118, "f5": 96,
	"f6": 97, "f7": 98, "f8": 100, "f9": 101, "f10": 109, "f11": 103,
	"f12": 111,
	// ANSI numeric keypad (code 90 is skipped by macOS)
	"kp_0": 82, "kp_1": 83, "kp_2": 84, "kp_3": 85, "kp_4": 86,
	"kp_5": 87, "kp_6": 88, "kp_7": 89, "kp_8": 91, "kp_9": 92,
	"kp_decimal": 65, "kp_multiply": 67, "kp_add": 69, "kp_clear": 71,
	"kp_divide": 75, "kp_subtract": 78, "kp_equals": 81,
}

var modifiers = map[string]string{
	"super": "command down", "cmd": "command down", "command": "command down",
	"win":  "command down",
	"ctrl": "control down", "control": "control down",
	"alt": "option down", "option": "option down", "meta": "option down",
	"shift": "shift down",
}

var singleKeyRe = regexp.MustCompile(`^[a-z0-9]$`)

// frontmostStmt focuses the recorded app; every synthesized-input script
// starts with it.
func frontmostStmt(st *serverState) (string, *ToolError) {
	escaped, terr := asEsc(st.App)
	if terr != nil {
		return "", terr
	}
	return fmt.Sprintf("    set frontmost of process \"%s\" to true\n", escaped), nil
}

func pressKeyChord(st *serverState, key string) *ToolError {
	var parts []string
	for _, p := range strings.Split(key, "+") {
		if t := strings.TrimSpace(strings.ToLower(p)); t != "" {
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 {
		return toolErr("invalid_args", "empty key chord",
			"use xdotool-style names: return, ctrl+c, super+v, kp_0, f5")
	}
	var mods []string
	var unknown []string
	for _, p := range parts[:len(parts)-1] {
		if m, ok := modifiers[p]; ok {
			mods = append(mods, m)
		} else {
			unknown = append(unknown, p)
		}
	}
	if len(unknown) > 0 {
		names := make([]string, 0, len(modifiers))
		for k := range modifiers {
			names = append(names, k)
		}
		sortStrings(names)
		return toolErr("invalid_args",
			fmt.Sprintf("unknown modifier(s): %s", strings.Join(unknown, ", ")),
			"known modifiers: "+strings.Join(names, ", "))
	}
	stem := parts[len(parts)-1]
	stmt := ""
	if code, ok := keyCodes[stem]; ok {
		stmt = fmt.Sprintf("key code %d", code)
	} else if singleKeyRe.MatchString(stem) {
		stmt = fmt.Sprintf("keystroke \"%s\"", stem)
	} else {
		return toolErr("invalid_args", fmt.Sprintf("unsupported key name: %s", stem),
			"see the press_key tool description for the supported names")
	}
	using := strings.Join(mods, ", ")
	if using != "" {
		stmt += fmt.Sprintf(" using {%s}", using)
	}
	front, terr := frontmostStmt(st)
	if terr != nil {
		return terr
	}
	_, terr = osascript(
		"tell application \"System Events\"\n"+front+
			fmt.Sprintf("    %s\n", stmt)+
			"end tell", "", 0)
	return terr
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ---------------------------------------------------------------- typing

// typeViaClipboard pastes text through the clipboard; returns whether the
// previous clipboard content was restored.
func typeViaClipboard(st *serverState, text string) (bool, *ToolError) {
	var saved string
	restorable := true
	probe := runCmd(10*time.Second, "", "pbpaste", "-Prefer", "txt")
	if probe != nil && probe.ExitCode == 0 {
		saved = probe.Stdout
		// an empty read from a non-empty clipboard means non-text content
		// (image/files) that pbpaste cannot give back — flag it, don't restore ""
		types := runCmd(10*time.Second, "", "osascript", "-e", "return (clipboard info) as text")
		if saved == "" && types != nil {
			t := strings.TrimSpace(types.Stdout)
			if t != "" && t != "0" {
				restorable = false
			}
		}
	} else {
		restorable = false
	}
	copyRes := runCmd(10*time.Second, text, "pbcopy")
	if copyRes == nil || copyRes.ExitCode != 0 {
		if restorable {
			runCmd(10*time.Second, saved, "pbcopy")
		}
		return false, toolErr("osascript_error",
			"pbcopy failed — text not typed, clipboard restored",
			"retry; if it persists check disk space and "+
				"/usr/bin/pbcopy permissions")
	}
	defer func() {
		if restorable {
			if restore := runCmd(10*time.Second, saved, "pbcopy"); restore == nil || restore.ExitCode != 0 {
				restorable = false // couldn't restore; caller reports it
			}
		}
	}()
	time.Sleep(150 * time.Millisecond)
	if terr := pressKeyChord(st, "super+v"); terr != nil {
		return restorable, terr
	}
	time.Sleep(150 * time.Millisecond)
	return restorable, nil
}

// typeViaKeystrokes types literal text with `keystroke` — no clipboard
// involvement. Slow for long strings, and the target must take synthesized
// keyboard input.
func typeViaKeystrokes(st *serverState, text string) *ToolError {
	if pyLen(text) > typeKeysMax {
		return toolErr(
			"invalid_args",
			fmt.Sprintf("keystrokes method is capped at %d characters (got %d)", typeKeysMax, pyLen(text)),
			"use the default paste method for long text")
	}
	var stmts []string
	for i, chunk := range strings.Split(text, "\n") {
		if i > 0 {
			stmts = append(stmts, "keystroke return")
		}
		if chunk != "" {
			escaped, terr := asEsc(chunk)
			if terr != nil {
				return terr
			}
			stmts = append(stmts, fmt.Sprintf("keystroke \"%s\"", escaped))
		}
	}
	front, terr := frontmostStmt(st)
	if terr != nil {
		return terr
	}
	body := ""
	for _, s := range stmts {
		body += "    " + s + "\n"
	}
	_, terr = osascript(
		"tell application \"System Events\"\n"+front+body+"end tell", "", 0)
	return terr
}

// ---------------------------------------------------------------- scroll

const scrollPy = `
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
`

// quartzScroll posts wheel events via Apple's bundled python3 (pyobjc ships
// with the Command Line Tools python, not with Homebrew builds). This is the
// one piece of the driving stack Go cannot replace: posting CGEvents needs
// pyobjc, and JXA's bridge cannot post to the session tap.
func quartzScroll(axis string, signedPixels int) *ToolError {
	candidates := []string{"/usr/bin/python3", lookPathPython3()}
	lastStderr := ""
	for _, py := range candidates {
		if py == "" {
			continue
		}
		res := runCmd(30*time.Second, "", py, "-c", scrollPy, axis, fmt.Sprintf("%d", signedPixels))
		if res != nil && res.ExitCode == 0 {
			return nil
		}
		if res != nil {
			lastStderr = strings.TrimSpace(res.Stderr)
			if len(lastStderr) > 200 {
				lastStderr = lastStderr[len(lastStderr)-200:]
			}
			if strings.Contains(res.Stderr, "No module named") {
				continue // no pyobjc on this interpreter; try the next
			}
		}
	}
	return toolErr(
		"unsupported",
		fmt.Sprintf("could not post scroll events (%s)", orDefault(lastStderr, "no interpreter")),
		"scroll needs Apple's python3 with the bundled pyobjc — present on "+
			"standard dev Macs at /usr/bin/python3 (Xcode Command Line Tools)")
}

func lookPathPython3() string {
	p, err := exec.LookPath("python3")
	if err != nil {
		return ""
	}
	return p
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// scrollWheel: direction is one of up/down/left/right. Sign convention
// (verified in Safari 2026-08-27): axis1 negative scrolls down, axis2
// negative scrolls right — so down/right are the negative directions.
func scrollWheel(direction string, pages float64) *ToolError {
	pixels := int(pages * scrollPixelsPerPage)
	signed := map[string]int{
		"up": +pixels, "down": -pixels,
		"right": -pixels, "left": +pixels,
	}[direction]
	axis := "h"
	if direction == "up" || direction == "down" {
		axis = "v"
	}
	return quartzScroll(axis, signed)
}

// ---------------------------------------------------------------- cliclick

func cliclick(wait int, commands ...string) *ToolError {
	argv := []string{"cliclick"}
	if wait != 0 {
		argv = append(argv, "-w", fmt.Sprintf("%d", wait))
	}
	argv = append(argv, commands...)
	res := runCmd(30*time.Second, "", argv...)
	if res == nil || res.ExitCode != 0 {
		stderr := ""
		if res != nil {
			stderr = strings.TrimSpace(res.Stderr)
		}
		return toolErr(
			"cliclick_error",
			fmt.Sprintf("cliclick failed: %s", stderr),
			"install it with: brew install cliclick")
	}
	return nil
}

// ---------------------------------------------------------------- pixel mapping

// pixelToPoint rescales screenshot pixels -> screen points using the
// capture's backing-store factor, re-anchoring by the CAPTURED window id
// (not by owner — another window of the same app must not silently steal
// the coordinate space).
func pixelToPoint(st *serverState, x, y float64, reanchor bool) (float64, float64, *ToolError) {
	if st.Scale == nil || st.OriginX == nil || st.OriginY == nil {
		return 0, 0, toolErr("no_state", "no screenshot scale on record",
			"call get_app_state first")
	}
	if reanchor && st.Stale {
		win := cgWindow{}
		found := false
		if st.WinID != nil {
			if w, terr := findWindowByID(*st.WinID); terr != nil {
				return 0, 0, terr
			} else if w != nil {
				win, found = *w, true
			}
		}
		if found {
			st.OriginX = &win.X
			st.OriginY = &win.Y
		} else {
			return 0, 0, toolErr(
				"window_gone",
				"the captured window is gone (closed/minimized)",
				"call get_app_state before further coordinate actions")
		}
	}
	return *st.OriginX + x / *st.Scale, *st.OriginY + y / *st.Scale, nil
}

func findWindowByID(winID int64) (*cgWindow, *ToolError) {
	wins, terr := windowList()
	if terr != nil {
		return nil, terr
	}
	for i := range wins {
		if wins[i].ID == winID {
			return &wins[i], nil
		}
	}
	return nil, nil
}

// ---------------------------------------------------------------- element actions

func axPress(st *serverState, index, times int) *ToolError {
	addr, terr := elementSource(st, index)
	if terr != nil {
		return terr
	}
	front, terr := frontmostStmt(st)
	if terr != nil {
		return terr
	}
	src := "tell application \"System Events\"\n" + front +
		fmt.Sprintf("    repeat %d times\n", times) +
		fmt.Sprintf("        perform action \"AXPress\" of %s\n", addr) +
		"    end repeat\n" +
		"end tell"
	if _, terr := osascript(src, "", 0); terr != nil {
		if terr.Code == "osascript_error" && strings.Contains(terr.Message, "AXPress") {
			return toolErr(
				"unsupported",
				fmt.Sprintf("element %d does not expose AXPress", index),
				"inspect it with element_info for available actions, use "+
					"perform_action, or click by screenshot coordinates")
		}
		return terr
	}
	return nil
}

func axSetValue(st *serverState, index int, value string) *ToolError {
	addr, terr := elementSource(st, index)
	if terr != nil {
		return terr
	}
	escaped, terr := asEsc(value)
	if terr != nil {
		return terr
	}
	front, terr := frontmostStmt(st)
	if terr != nil {
		return terr
	}
	src := "tell application \"System Events\"\n" + front +
		fmt.Sprintf("    set value of %s to \"%s\"\n", addr, escaped) +
		"end tell"
	_, terr = osascript(src, "", 0)
	return terr
}

var axActionRe = regexp.MustCompile(`^AX[A-Za-z]+$`)

func axPerformAction(st *serverState, index int, action string) *ToolError {
	if !axActionRe.MatchString(action) {
		return toolErr(
			"invalid_args",
			fmt.Sprintf("invalid action name %q", action),
			"must look like AXPick, AXConfirm, AXIncrement (letters only, "+
				"AX prefix); discover names with element_info")
	}
	addr, terr := elementSource(st, index)
	if terr != nil {
		return terr
	}
	front, terr := frontmostStmt(st)
	if terr != nil {
		return terr
	}
	src := "tell application \"System Events\"\n" + front +
		fmt.Sprintf("    perform action \"%s\" of %s\n", action, addr) +
		"end tell"
	if _, terr := osascript(src, "", 0); terr != nil {
		if terr.Code == "osascript_error" && strings.Contains(terr.Message, action) {
			return toolErr(
				"unsupported",
				fmt.Sprintf("element %d does not expose action %s", index, action),
				"list what it does expose with element_info")
		}
		return terr
	}
	return nil
}

// axReadSelAndValue returns (selected_text, value, have_value) of one
// element via a single osascript round-trip. Missing/unreadable values come
// back as "".
func axReadSelAndValue(addr string) (string, string, bool, *ToolError) {
	src := "on txt(v)\n" +
		"    if v is missing value then return \"\"\n" +
		"    try\n" +
		"        return (v as text)\n" +
		"    on error\n" +
		"        return \"\"\n" +
		"    end try\n" +
		"end txt\n" +
		"tell application \"System Events\"\n" +
		fmt.Sprintf("    set el to %s\n", addr) +
		"    set sep to character id 31\n" +
		"    set sel to \"\"\n" +
		"    try\n" +
		"        set sel to (value of attribute \"AXSelectedText\" of el) as text\n" +
		"    end try\n" +
		"    set val to \"\"\n" +
		"    set haveVal to \"0\"\n" +
		"    try\n" +
		"        set val to (value of el) as text\n" +
		"        set haveVal to \"1\"\n" +
		"    end try\n" +
		"    return (my txt(sel)) & sep & (my txt(val)) & sep & haveVal\n" +
		"end tell"
	raw, terr := osascript(src, "", 0)
	if terr != nil {
		return "", "", false, terr
	}
	parts := strings.Split(raw, "\x1f")
	if len(parts) != 3 {
		return "", "", false, toolErr(
			"internal", "selection readback had the wrong field count",
			"call get_app_state again and retry")
	}
	return parts[0], parts[1], parts[2] == "1", nil
}

// axSelectViaKeys keyboard-walks a selection: focus, jump to document start
// (the Cocoa cmd+up binding), then arrow right. Slow but honored far more
// widely than scripted AXSelectedTextRange writes.
func axSelectViaKeys(st *serverState, addr string, start, length int) *ToolError {
	total := start + length
	front, terr := frontmostStmt(st)
	if terr != nil {
		return terr
	}
	src := "tell application \"System Events\"\n" + front +
		fmt.Sprintf("    set el to %s\n", addr) +
		"    set focused of el to true\n" +
		"    delay 0.1\n" +
		"    key code 126 using command down\n"
	if start > 0 {
		src += fmt.Sprintf("    repeat %d times\n        key code 124\n    end repeat\n", start)
	}
	src += fmt.Sprintf("    repeat %d times\n", length) +
		"        key code 124 using shift down\n" +
		"    end repeat\n" +
		"end tell"
	timeout := osaTimeoutSec + time.Duration(total*100)*time.Millisecond
	_, terr = osascript(src, "", timeout)
	return terr
}

// axSelectText selects [start, start+length) and VERIFIES by content: the
// AXSelectedText readback must equal that slice of the element's value.
// Apps that ignore the attribute write (TextEdit does) get the keyboard
// fallback. Indices are code points, like Python string slicing.
func axSelectText(st *serverState, index, start, length int) (map[string]any, *ToolError) {
	addr, terr := elementSource(st, index)
	if terr != nil {
		return nil, terr
	}
	sel, value, haveValue, terr := axReadSelAndValue(addr)
	if terr != nil {
		return nil, terr
	}
	expected := ""
	haveExpected := false
	if haveValue {
		if start+length > pyLen(value) {
			return nil, toolErr(
				"invalid_args",
				fmt.Sprintf("range [%d:%d) is beyond the element text (%d chars)", start, start+length, pyLen(value)),
				"shrink the range or set the element value first")
		}
		expected = pySlice(value, start, length)
		haveExpected = true
	}

	verified := func() bool {
		if haveExpected {
			return sel == expected
		}
		return sel != ""
	}

	method := ""
	// AXSelectedTextRange write attempt
	writeSrc := "tell application \"System Events\"\n" +
		mustFrontmost(st) +
		fmt.Sprintf("    set el to %s\n", addr) +
		fmt.Sprintf("    set value of attribute \"AXSelectedTextRange\" of el to {%d, %d}\n", start, length) +
		"end tell"
	if _, terr := osascript(writeSrc, "", 0); terr == nil {
		if sel2, _, _, terr2 := axReadSelAndValue(addr); terr2 == nil {
			sel = sel2
			if verified() {
				method = "ax_range"
			}
		}
		// attribute write unsupported / unverified; keyboard fallback follows
	}

	if method == "" && start+length <= selectKeysMax {
		if terr := axSelectViaKeys(st, addr, start, length); terr != nil {
			return nil, terr
		}
		// the keyboard-path readback is not error-swallowed (matches the
		// Python: only the ax_range attempt is wrapped)
		sel3, _, _, terr3 := axReadSelAndValue(addr)
		if terr3 != nil {
			return nil, terr3
		}
		sel = sel3
		if verified() {
			method = "keyboard"
		}
	}

	if method == "" {
		return nil, toolErr(
			"unsupported",
			fmt.Sprintf("could not verify selection [%d:%d) on element %d", start, start+length, index),
			"this app may not honor scripted selection; click to place the "+
				"caret and use press_key with shift+arrows instead")
	}
	return map[string]any{"selected_text": sel, "method": method}, nil
}

func mustFrontmost(st *serverState) string {
	front, terr := frontmostStmt(st)
	if terr != nil {
		return ""
	}
	return front
}

// axElementInfo inspects one element via a single osascript round-trip.
func axElementInfo(st *serverState, index int) (map[string]any, *ToolError) {
	addr, terr := elementSource(st, index)
	if terr != nil {
		return nil, terr
	}
	src := "on txt(v)\n" +
		"    if v is missing value then return \"\"\n" +
		"    try\n" +
		"        return (v as text)\n" +
		"    on error\n" +
		"        return \"\"\n" +
		"    end try\n" +
		"end txt\n" +
		"on joinField(v)\n" +
		"    set sep to character id 31\n" +
		"    if (count of v) is 0 then return sep\n" +
		"    set out to item 1 of v\n" +
		"    repeat with j from 2 to (count of v)\n" +
		"        set out to out & sep & (item j of v)\n" +
		"    end repeat\n" +
		"    return out\n" +
		"end joinField\n" +
		"tell application \"System Events\"\n" +
		fmt.Sprintf("    set el to %s\n", addr) +
		"    set props to properties of el\n" +
		"    set actNames to {}\n" +
		"    try\n" +
		"        set actNames to name of every action of el\n" +
		"    end try\n" +
		"    set p to position of el\n" +
		"    set s to size of el\n" +
		"    set sep to character id 31\n" +
		"    return (my txt(role of props)) & sep & (my txt(name of props)) & sep & (my txt(value of props)) & sep & ((enabled of props) as text) & sep & ((focused of props) as text) & sep & ((item 1 of p) as text) & \",\" & ((item 2 of p) as text) & sep & ((item 1 of s) as text) & \"x\" & ((item 2 of s) as text) & sep & (my joinField(actNames))\n" +
		"end tell"
	raw, terr := osascript(src, "", 0)
	if terr != nil {
		return nil, terr
	}
	parts := strings.Split(raw, "\x1f")
	if len(parts) != 8 {
		// a field value smuggled a U+001F past us — refuse rather than
		// mis-bind role/name/value
		return nil, toolErr(
			"internal",
			"element_info readback had the wrong field count",
			"call get_app_state again; if it persists, the element text "+
				"contains control characters")
	}
	role, name, value, enabled, focused, pos, size, actions := parts[0], parts[1], parts[2], parts[3], parts[4], parts[5], parts[6], parts[7]
	var actionList []string
	for _, a := range strings.Split(actions, ",") {
		if t := strings.TrimSpace(a); t != "" {
			actionList = append(actionList, t)
		}
	}
	if actionList == nil {
		actionList = []string{}
	}
	return map[string]any{
		"index": index, "role": role, "name": name,
		"value":   pySlice(value, 0, 200),
		"enabled": enabled, "focused": focused, "position_pt": pos,
		"size_pt": size,
		"actions": actionList,
	}, nil
}
