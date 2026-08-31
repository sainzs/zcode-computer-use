package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Ordered payload structs — JSON object key order follows the Python dicts,
// and agents read these documents.

type listAppsPayload struct {
	Frontmost   string   `json:"frontmost"`
	RunningApps []string `json:"running_apps"`
	Hint        string   `json:"hint"`
}

type windowEntry struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	BoundsPt  string `json:"bounds_pt"`
	Frontmost bool   `json:"frontmost"`
}

type listWindowsPayload struct {
	App     string        `json:"app"`
	Windows []windowEntry `json:"windows"`
	Hint    string        `json:"hint"`
}

type appStatePayload struct {
	State string `json:"state"`
	Tree  string `json:"tree"`
}

type messagePayload struct {
	Result string `json:"result"`
}

type selectTextPayload struct {
	Result       string `json:"result"`
	SelectedText string `json:"selected_text"`
	Method       string `json:"method"`
	Hint         string `json:"hint"`
}

func toolListApps() (any, *ToolError) {
	appsA, front, terr := runningApps()
	if terr != nil {
		return nil, terr
	}
	if appsA == nil {
		appsA = []string{}
	}
	return listAppsPayload{
		Frontmost:   front,
		RunningApps: appsA,
		Hint:        "call get_app_state with one of these names (partial match ok)",
	}, nil
}

func toolListWindows(appRaw string) (any, *ToolError) {
	if appRaw == "" {
		return nil, toolErr("invalid_args", "app is required", "")
	}
	name := resolveApp(appRaw)
	if name == "" {
		return nil, toolErr("app_not_found",
			fmt.Sprintf("app %q is not running", appRaw),
			"call list_apps for running names")
	}
	wins, terr := windowsOf(name)
	if terr != nil {
		return nil, terr
	}
	entries := []windowEntry{}
	for i, w := range wins {
		entries = append(entries, windowEntry{
			ID:        w.ID,
			Title:     w.Name,
			BoundsPt:  fmt.Sprintf("%d,%d %dx%d", int(w.X), int(w.Y), int(w.W), int(w.H)),
			Frontmost: i == 0,
		})
	}
	return listWindowsPayload{
		App:     name,
		Windows: entries,
		Hint: "pass one of these ids (or a title substring) as `window` " +
			"to get_app_state",
	}, nil
}

func toolGetAppState(st *serverState, a args) (any, *ToolError) {
	// any failure below must leave the state unusable, never live-looking
	markStale(st)
	app, terr := argStr(a, "app", true)
	if terr != nil {
		return nil, terr
	}
	minZero, maxHard := 0, maxDepthHard
	depth, terr := argInt(a, "depth", maxDepthDefault, true, &minZero, &maxHard)
	if terr != nil {
		return nil, terr
	}
	window, hasWindow := a.raw("window")
	if hasWindow && window != nil {
		if _, isBool := window.(bool); isBool {
			return nil, toolErr("invalid_args",
				"window must be an id (integer) or a title substring",
				"call list_windows to see ids and titles")
		}
		switch v := window.(type) {
		case string:
			_ = v
		case json.Number:
			if !jsonNumberIsInt(string(v)) {
				return nil, toolErr("invalid_args",
					"window must be an id (integer) or a title substring",
					"call list_windows to see ids and titles")
			}
		default:
			return nil, toolErr("invalid_args",
				"window must be an id (integer) or a title substring",
				"call list_windows to see ids and titles")
		}
	}
	activate, terr := argBool(a, "activate", true)
	if terr != nil {
		return nil, terr
	}
	launch, terr := argBool(a, "launch", false)
	if terr != nil {
		return nil, terr
	}
	filter, terr := argStr(a, "filter", false)
	if terr != nil {
		return nil, terr
	}
	// validated here so both surfaces reject bad types; the MCP layer is
	// what actually attaches the image content block
	if _, terr := argBool(a, "include_screenshot", false); terr != nil {
		return nil, terr
	}

	name := resolveApp(app)
	if name == "" {
		if launch {
			res := runCmd(15*time.Second, "", "open", "-a", app)
			if res == nil || res.ExitCode != 0 {
				stderr := ""
				if res != nil {
					stderr = strings.TrimSpace(res.Stderr)
					if len(stderr) > 200 {
						stderr = stderr[:200]
					}
				}
				return nil, toolErr(
					"app_not_found",
					fmt.Sprintf("could not launch app %q", app),
					stderr)
			}
			// `open -a` accepts bundle paths, but the process will be
			// known by its basename ("TextEdit"), so poll on that
			pollName := app
			if strings.Contains(app, "/") {
				pollName = strings.TrimSuffix(filepath.Base(app), filepath.Ext(app))
			}
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) && name == "" {
				name = orEmpty(resolveApp(pollName))
				if name == "" {
					name = orEmpty(resolveApp(app))
				}
				if name == "" {
					time.Sleep(500 * time.Millisecond)
				}
			}
		}
		if name == "" {
			return nil, toolErr(
				"app_not_found", fmt.Sprintf("app %q is not running", app),
				"pass launch:true to open it, or pick a running app from "+
					"list_apps")
		}
	}
	if activate {
		if terr := activateApp(name); terr != nil {
			return nil, terr
		}
	}

	cgWin, terr := pickWindow(name, window, hasWindow)
	if terr != nil {
		return nil, terr
	}
	// AXRaise the requested window when it is not the app's frontmost one.
	// Guarded: the window list can empty between pickWindow and this check
	// (the Python indexed [0] unguarded and could crash here).
	wins, terr := windowsOf(name)
	if terr != nil {
		return nil, terr
	}
	if activate && len(wins) > 0 && wins[0].ID != cgWin.ID {
		src, _, terr := seWindowSrc(name, cgWin)
		if terr == nil {
			// 4.0 fix: frontmost the app being captured, not the previous
			// state's app — the Python read STATE["app"] here, which is the
			// PREVIOUS app (empty on a first capture), so the raise attempt
			// silently no-opped exactly when it was needed most
			escaped, terr2 := asEsc(name)
			if terr2 == nil {
				front := fmt.Sprintf("    set frontmost of process \"%s\" to true\n", escaped)
				_, terr3 := osascript(
					"tell application \"System Events\"\n"+front+
						fmt.Sprintf("    perform action \"AXRaise\" of %s\n", src)+
						"end tell", "", 0)
				if terr3 == nil {
					time.Sleep(keypageDelayMs * time.Millisecond)
				}
				// AXRaise is best-effort; capture proceeds regardless
			}
		}
	}

	// capture to an unpredictable temp name, then rename onto the counter
	// path — a pre-placed symlink at the predictable name is replaced, not
	// followed, and the file is private to this user
	tmp, err := os.CreateTemp(dataDir(), ".shot-*.png")
	if err != nil {
		return nil, toolErr("internal", "could not create temp capture file", "")
	}
	tmpShot := tmp.Name()
	tmp.Close()

	capture := func(argv ...string) bool {
		res := runCmd(20*time.Second, "", argv...)
		if res == nil || res.ExitCode != 0 {
			rc := -1
			stderr := ""
			if res != nil {
				rc = res.ExitCode
				stderr = res.Stderr
				if len(stderr) > 200 {
					stderr = stderr[:200]
				}
			}
			logEvent("screencapture_failed", map[string]any{"rc": rc, "stderr": stderr})
			return false
		}
		info, err := os.Stat(tmpShot)
		return err == nil && info.Size() > 0
	}

	var originX, originY, pointsW float64
	var winID *int64
	var scope string
	ok := false
	if cgWin.W*cgWin.H > 100 {
		ok = capture("screencapture", "-x", "-o", "-l",
			strconv.FormatInt(cgWin.ID, 10), tmpShot)
	}
	if ok {
		originX, originY, pointsW = cgWin.X, cgWin.Y, cgWin.W
		wid := cgWin.ID
		winID = &wid
		scope = fmt.Sprintf("window %d (%s)", cgWin.ID, orDefault(snippet(cgWin.Name, 40), "unnamed"))
	} else {
		if !capture("screencapture", "-x", tmpShot) {
			_ = os.Remove(tmpShot)
			return nil, toolErr(
				"permission_screen_recording",
				"screencapture produced no image",
				"grant Screen Recording to the host app in System "+
					"Settings > Privacy & Security > Screen Recording, then "+
					"restart it")
		}
		disp, terr := mainDisplay()
		if terr != nil {
			_ = os.Remove(tmpShot)
			return nil, terr
		}
		originX, originY, pointsW = disp.X, disp.Y, disp.W
		winID = nil
		scope = "full-screen (window capture failed; multi-display " +
			"coordinate accuracy is not guaranteed — prefer window capture)"
	}

	st.Counter++
	shot := shotPath(st.Counter)
	_ = os.Rename(tmpShot, shot)
	_ = os.Chmod(shot, 0o600)
	pruneScreenshots(shot)

	pxW, _, err := pngSize(shot)
	if err != nil {
		// the capture is not a decodable PNG; leave state unusable. This
		// surfaced as a RuntimeError in the Python, so it rides the crash
		// payload (code internal + the crash remedy), not a tool error.
		markStale(st)
		return nil, toolErr("internal", err.Error(),
			"retry once; if it persists, check the server log and "+
				"re-call get_app_state")
	}
	scale := 2.0
	if pointsW != 0 {
		scale = pyRound(float64(pxW)/pointsW, 3)
	}

	winSrc, srcMatched, terr := seWindowSrc(name, cgWin)
	if terr != nil {
		markStale(st)
		return nil, terr
	}
	treeLines := []string{}
	registry := map[int]treeEntry{}
	treeNote := "disabled (depth 0)"
	if depth > 0 {
		parsed, terr := dumpTree(winSrc, depth, maxElements)
		if terr != nil {
			if terr.Code == "permission_accessibility" {
				treeNote = "tree unavailable: Accessibility grant missing " +
					"(screenshot is still valid)"
			} else {
				treeNote = fmt.Sprintf("tree unavailable: %s", terr.Message)
			}
		} else {
			registry = parsed.Registry
			counts, marked := diffTree(st, registry, name, i64_or_zero(winID))
			filterLow := strings.ToLower(filter)
			for _, idx := range sortedIndices(registry) {
				entry := registry[idx]
				m := marked[idx]
				if filterLow != "" && !strings.Contains(strings.ToLower(m[1]), filterLow) {
					continue
				}
				treeLines = append(treeLines,
					fmt.Sprintf("[%s%d] %s @@%s", m[0], idx, m[1], entry.Chain))
			}
			treeNote = fmt.Sprintf("%d elements (depth %d); diff vs "+
				"previous capture of this window: +%d new, ~%d changed, "+
				"%d gone", len(registry), depth, counts.New, counts.Changed, counts.Gone)
			if counts.FirstCapture {
				treeNote += " (first capture)"
			}
			if filter != "" {
				// the full registry is still live — filter trims what is
				// SHOWN, never what is targetable; find widens the view later
				treeNote += fmt.Sprintf("; filter %q shows %d line(s) "+
					"(all %d indices stay targetable — use find to re-query)",
					filter, len(treeLines), len(registry))
			}
		}
	}

	st.App = name
	st.Screenshot = shot
	st.Scale = &scale
	st.OriginX = &originX
	st.OriginY = &originY
	st.WinID = winID
	st.WinSrc = winSrc
	st.Elements = registry
	st.Stale = false
	saveState(st)

	head := fmt.Sprintf("app: %s", name)
	if activate {
		head += " (frontmost)"
	} else {
		head += " (background, not activated)"
	}
	head += "\n" + fmt.Sprintf("capture: %s, %dpx wide, pixel_scale %s\n", scope, pxW, pyFloatStr(scale))
	head += fmt.Sprintf("screenshot: %s\n", shot)
	head += fmt.Sprintf("tree: %s\n", treeNote)
	if !srcMatched {
		head += "window-address: System Events match failed; using window 1 " +
			"(element targets may resolve against a different window — " +
			"prefer coordinate actions)\n"
	}
	head += "coordinates in the tree (@px:x,y are screenshot pixels); click " +
		"element_index or x/y in screenshot pixels. Tree diff markers: " +
		"[+n] new, [~n] changed since the previous capture.\n"

	treeText := strings.Join(treeLines, "\n")
	if treeText == "" {
		treeText = "(none)"
	}
	return appStatePayload{State: head, Tree: treeText}, nil
}

func sortedIndices(m map[int]treeEntry) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

func orEmpty(s string) string { return s }

func i64_or_zero(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func toolClick(st *serverState, a args) (any, *ToolError) {
	elementRaw, hasElement := a.raw("element_index")
	element := 0
	hasEl := hasElement && elementRaw != nil
	if hasEl {
		var terr *ToolError
		element, terr = argInt(a, "element_index", nil, false, nil, nil)
		if terr != nil {
			return nil, terr
		}
		if mb, ok := a.raw("mouse_button"); ok && mb != nil && mb != "left" {
			if s, isStr := mb.(string); !isStr || s != "left" {
				return nil, toolErr(
					"invalid_args",
					"element clicks are left-presses (AXPress); mouse_button "+
						"does not apply",
					"for right/middle clicks use screenshot pixel coordinates "+
						"x/y with mouse_button")
			}
		}
	}
	if terr := requireState(st, hasEl); terr != nil {
		return nil, terr
	}
	button := any("left")
	if mb, ok := a.raw("mouse_button"); ok {
		button = mb
	}
	bt, isStr := button.(string)
	if !isStr || (bt != "left" && bt != "right" && bt != "middle") {
		return nil, toolErr("invalid_args",
			fmt.Sprintf("unknown mouse_button %s", pyRepr(button)),
			"use left, right, or middle")
	}
	minOne, maxFive := 1, 5
	count, terr := argInt(a, "click_count", 1, true, &minOne, &maxFive)
	if terr != nil {
		return nil, terr
	}
	var how string
	if hasEl {
		if terr := axPress(st, element, count); terr != nil {
			return nil, terr
		}
		how = fmt.Sprintf("AXPress element %d", element)
	} else {
		_, hasX := a.raw("x")
		_, hasY := a.raw("y")
		if !hasX || !hasY {
			return nil, toolErr("invalid_args", "provide element_index or x/y",
				"element targets are preferred; coordinates are "+
					"screenshot pixels")
		}
		if count > 2 {
			return nil, toolErr(
				"invalid_args",
				fmt.Sprintf("coordinate clicks support click_count 1-2, got %d", count),
				"chain multiple click calls instead")
		}
		if count == 2 && bt != "left" {
			return nil, toolErr(
				"invalid_args",
				fmt.Sprintf("double-click is left-only, got mouse_button=%s", bt),
				"use two single clicks instead")
		}
		x, terr := argNumber(a, "x", nil, false)
		if terr != nil {
			return nil, terr
		}
		y, terr := argNumber(a, "y", nil, false)
		if terr != nil {
			return nil, terr
		}
		px, py, terr := pixelToPoint(st, x, y, true)
		if terr != nil {
			return nil, terr
		}
		ipx, ipy := roundPt(px), roundPt(py)
		cmd := map[string]string{"left": "c", "right": "rc", "middle": "mc"}[bt]
		if count == 2 && bt == "left" {
			cmd = "dc"
		}
		if terr := cliclick(12, fmt.Sprintf("%s:%d,%d", cmd, ipx, ipy)); terr != nil {
			return nil, terr
		}
		how = fmt.Sprintf("%s click at (%s,%s)px -> (%d,%d)pt",
			bt, rawNumberText(a, "x"), rawNumberText(a, "y"), ipx, ipy)
	}
	markStale(st)
	return messagePayload{
		Result: how + ". Action completed. Call get_app_state to see " +
			"the updated UI.",
	}, nil
}

func roundPt(f float64) int {
	return int(math.Round(f))
}

// rawNumberText echoes the caller's literal ("100" stays "100", "1.5" stays
// "1.5") the way Python's str(args["x"]) echoed the decoded value.
func rawNumberText(a args, key string) string {
	if v, ok := a[key]; ok {
		if n, isNum := v.(json.Number); isNum {
			return string(n)
		}
	}
	return "?"
}

func toolTypeText(st *serverState, a args) (any, *ToolError) {
	method := "paste"
	if m, ok := a.raw("method"); ok && m != nil {
		s, isStr := m.(string)
		if !isStr {
			return nil, toolErr("invalid_args", "method must be a string", "")
		}
		method = s
	}
	if method != "paste" && method != "keys" {
		return nil, toolErr("invalid_args",
			fmt.Sprintf("unknown method %q", method),
			"method must be 'paste' (default) or 'keys'")
	}
	if terr := requireState(st, false); terr != nil {
		return nil, terr
	}
	text := ""
	if t, ok := a.raw("text"); ok && t != nil {
		s, isStr := t.(string)
		if !isStr {
			return nil, toolErr("invalid_args", "text must be a string", "")
		}
		text = s
	}
	if text == "" {
		return nil, toolErr("invalid_args",
			"text is required and must be non-empty", "")
	}
	if method == "paste" && strings.Contains(text, "\x1f") {
		// U+001F is the server's internal AX field separator; it would
		// corrupt later element_info/selection readbacks
		return nil, toolErr(
			"invalid_args",
			"text contains the control character U+001F",
			"strip it (it is never meaningful in UI text) and retry")
	}
	note := ""
	how := ""
	if method == "paste" {
		restorable, terr := typeViaClipboard(st, text)
		if terr != nil {
			return nil, terr
		}
		if !restorable {
			note = " NOTE: the previous clipboard held non-text content that could " +
				"not be restored — re-copy it if needed."
		}
		how = "Typed via clipboard paste."
	} else {
		if terr := typeViaKeystrokes(st, text); terr != nil {
			return nil, terr
		}
		how = "Typed via synthesized keystrokes (clipboard untouched)."
	}
	markStale(st)
	return messagePayload{
		Result: fmt.Sprintf("%s%s Action completed. Call get_app_state to "+
			"see the updated UI.", how, note),
	}, nil
}

func toolPressKey(st *serverState, a args) (any, *ToolError) {
	if terr := requireState(st, false); terr != nil {
		return nil, terr
	}
	key := ""
	if k, ok := a.raw("key"); ok && k != nil {
		if s, isStr := k.(string); isStr {
			key = s
		}
	}
	if terr := pressKeyChord(st, key); terr != nil {
		return nil, terr
	}
	markStale(st)
	return messagePayload{
		Result: fmt.Sprintf("Pressed %s. Action completed. Call "+
			"get_app_state to see the updated UI.", key),
	}, nil
}

func toolScroll(st *serverState, a args) (any, *ToolError) {
	if terr := requireState(st, false); terr != nil {
		return nil, terr
	}
	direction := "down"
	if d, ok := a.raw("direction"); ok && d != nil {
		s, isStr := d.(string)
		if !isStr {
			return nil, toolErr("invalid_args", "direction must be a string", "")
		}
		direction = s
	}
	if direction != "up" && direction != "down" && direction != "left" && direction != "right" {
		return nil, toolErr("invalid_args",
			fmt.Sprintf("unknown direction %q", direction),
			"direction must be up, down, left, or right")
	}
	pages, terr := argNumber(a, "pages", 1.0, true)
	if terr != nil {
		return nil, terr
	}
	pages = math.Max(0.1, math.Min(pages, 50))
	_, hasX := a.raw("x")
	_, hasY := a.raw("y")
	if hasX && hasY {
		// 4.0 fix: coerce through the strict arg layer like every other
		// coordinate (the Python float()-ed the raw values here, letting a
		// bool "x": true act as 1.0)
		x, terr := argNumber(a, "x", nil, false)
		if terr != nil {
			return nil, terr
		}
		y, terr2 := argNumber(a, "y", nil, false)
		if terr2 != nil {
			return nil, terr2
		}
		px, py, terr := pixelToPoint(st, x, y, true)
		if terr != nil {
			return nil, terr
		}
		if terr := cliclick(12, fmt.Sprintf("m:%d,%d", roundPt(px), roundPt(py))); terr != nil {
			return nil, terr
		}
	}
	if terr := scrollWheel(direction, pages); terr != nil {
		return nil, terr
	}
	markStale(st)
	return messagePayload{
		Result: fmt.Sprintf("Scrolled %s %s page(s). Action "+
			"completed. Call get_app_state to see the updated UI.",
			direction, pyFloatStr(pages)),
	}, nil
}

func toolDrag(st *serverState, a args) (any, *ToolError) {
	if terr := requireState(st, false); terr != nil {
		return nil, terr
	}
	for _, k := range []string{"from_x", "from_y", "to_x", "to_y"} {
		if _, ok := a.raw(k); !ok {
			return nil, toolErr("invalid_args", fmt.Sprintf("%s is required", k), "")
		}
	}
	// anchor once for both endpoints — two separate lookups could straddle
	// a window move between them
	fx, terr := argNumber(a, "from_x", nil, false)
	if terr != nil {
		return nil, terr
	}
	fy, terr := argNumber(a, "from_y", nil, false)
	if terr != nil {
		return nil, terr
	}
	tx, terr := argNumber(a, "to_x", nil, false)
	if terr != nil {
		return nil, terr
	}
	ty, terr := argNumber(a, "to_y", nil, false)
	if terr != nil {
		return nil, terr
	}
	ffx, ffy, terr := pixelToPoint(st, fx, fy, true)
	if terr != nil {
		return nil, terr
	}
	ttx, tty, terr := pixelToPoint(st, tx, ty, false)
	if terr != nil {
		return nil, terr
	}
	if terr := cliclick(60,
		fmt.Sprintf("dd:%d,%d", roundPt(ffx), roundPt(ffy)),
		fmt.Sprintf("dm:%d,%d", roundPt(ttx), roundPt(tty)),
		fmt.Sprintf("du:%d,%d", roundPt(ttx), roundPt(tty))); terr != nil {
		return nil, terr
	}
	markStale(st)
	return messagePayload{Result: "Drag performed. Action completed. Call get_app_state " +
		"to see the updated UI."}, nil
}

func toolSetValue(st *serverState, a args) (any, *ToolError) {
	element, terr := argInt(a, "element_index", nil, false, nil, nil)
	if terr != nil {
		return nil, terr
	}
	if _, ok := a.raw("value"); !ok {
		return nil, toolErr("invalid_args", "element_index and value are required", "")
	}
	value, terr := argStr(a, "value", true)
	if terr != nil {
		return nil, terr
	}
	if terr := requireState(st, true); terr != nil {
		return nil, terr
	}
	if terr := axSetValue(st, element, value); terr != nil {
		return nil, terr
	}
	markStale(st)
	return messagePayload{Result: fmt.Sprintf("Value set on element %d. Action "+
		"completed. Call get_app_state to see the updated UI.", element)}, nil
}

func toolSelectText(st *serverState, a args) (any, *ToolError) {
	element, terr := argInt(a, "element_index", nil, false, nil, nil)
	if terr != nil {
		return nil, terr
	}
	minZero := 0
	start, terr := argInt(a, "start", 0, true, &minZero, nil)
	if terr != nil {
		return nil, terr
	}
	length, terr := argInt(a, "length", nil, false, nil, nil)
	if terr != nil {
		return nil, terr
	}
	if start+length > 100_000 {
		return nil, toolErr("invalid_args",
			"start+length capped at 100000", "")
	}
	if terr := requireState(st, true); terr != nil {
		return nil, terr
	}
	result, terr := axSelectText(st, element, start, length)
	if terr != nil {
		return nil, terr
	}
	markStale(st)
	return selectTextPayload{
		Result: fmt.Sprintf("Selected [%d:%d) on element "+
			"%d (method: %s).", start, start+length, element, result["method"]),
		SelectedText: result["selected_text"].(string),
		Method:       result["method"].(string),
		Hint:         "selected_text is verified against the element content",
	}, nil
}

func toolElementInfo(st *serverState, a args) (any, *ToolError) {
	element, terr := argInt(a, "element_index", nil, false, nil, nil)
	if terr != nil {
		return nil, terr
	}
	if terr := requireState(st, true); terr != nil {
		return nil, terr
	}
	return axElementInfo(st, element)
}

func toolPerformAction(st *serverState, a args) (any, *ToolError) {
	element, terr := argInt(a, "element_index", nil, false, nil, nil)
	if terr != nil {
		return nil, terr
	}
	action, terr := argStr(a, "action", true)
	if terr != nil {
		return nil, terr
	}
	if terr := requireState(st, true); terr != nil {
		return nil, terr
	}
	if terr := axPerformAction(st, element, action); terr != nil {
		return nil, terr
	}
	markStale(st)
	return messagePayload{Result: fmt.Sprintf("Performed %s on element "+
		"%d. Action completed. Call "+
		"get_app_state to see the updated UI.", action, element)}, nil
}
