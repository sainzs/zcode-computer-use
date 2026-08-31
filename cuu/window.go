package main

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// `window` manages an app window directly — move/resize/minimize/close —
// without pixel-hunting. Same resolveApp + pickWindow + seWindowSrc chain as
// get_app_state, so a window id or title substring targets the exact window
// an element action would. Coordinates are SCREEN POINTS (what System
// Events' `position`/`size` speak), never screenshot pixels: no capture is
// involved, so the tool works even when Screen Recording is denied.

const windowActionsHelp = "action must be one of move, resize, minimize, unminimize, zoom, close"

var windowActions = map[string]bool{
	"move":       true,
	"resize":     true,
	"minimize":   true,
	"unminimize": true,
	"zoom":       true,
	"close":      true,
}

// scriptNum renders a coordinate for an AppleScript literal: decimal
// notation only ("100", "100.5"), never exponent form (%g would emit
// "1e+07" for large values, which is not an AppleScript number).
func scriptNum(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// minimizedWindowSrc addresses a minimized window via System Events: the
// first minimized window by default, or the first whose title contains the
// given substring. CGWindow ids cannot work here — an off-screen window has
// no CGWindowList entry to match an id against.
func minimizedWindowSrc(name string, window any, hasWindow bool) (string, *ToolError) {
	escaped, terr := asEsc(name)
	if terr != nil {
		return "", terr
	}
	if !hasWindow || window == nil || window == "" {
		return fmt.Sprintf("(first window of process \"%s\" whose value "+
			"of attribute \"AXMinimized\" is true)", escaped), nil
	}
	title, isStr := window.(string)
	if !isStr {
		return "", toolErr("invalid_args",
			"a minimized window has no CGWindow id — target it by title substring",
			"pass window as a title substring, or omit it to restore the "+
				"first minimized window")
	}
	escTitle, terr := asEsc(title)
	if terr != nil {
		return "", terr
	}
	return fmt.Sprintf("(first window of process \"%s\" whose value of "+
		"attribute \"AXMinimized\" is true and name contains \"%s\")",
		escaped, escTitle), nil
}

func toolWindow(st *serverState, a args) (any, *ToolError) {
	app, terr := argStr(a, "app", true)
	if terr != nil {
		return nil, terr
	}
	action, terr := argStr(a, "action", true)
	if terr != nil {
		return nil, terr
	}
	if !windowActions[action] {
		return nil, toolErr("invalid_args",
			fmt.Sprintf("unknown action %q", action), windowActionsHelp)
	}
	// `window` is optional; when present it must be an id or a title
	// substring — the same coercion get_app_state applies before pickWindow
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

	// coordinate args are validated before any GUI work so a malformed call
	// never touches System Events (and stays headless-testable)
	var x, y, width, height float64
	switch action {
	case "move":
		if _, ok := a.raw("x"); !ok {
			return nil, toolErr("invalid_args",
				"move requires x and y (screen points)", "")
		}
		if _, ok := a.raw("y"); !ok {
			return nil, toolErr("invalid_args",
				"move requires x and y (screen points)", "")
		}
		if x, terr = argNumber(a, "x", nil, false); terr != nil {
			return nil, terr
		}
		if y, terr = argNumber(a, "y", nil, false); terr != nil {
			return nil, terr
		}
	case "resize":
		if _, ok := a.raw("width"); !ok {
			return nil, toolErr("invalid_args",
				"resize requires width and height (screen points)", "")
		}
		if _, ok := a.raw("height"); !ok {
			return nil, toolErr("invalid_args",
				"resize requires width and height (screen points)", "")
		}
		if width, terr = argNumber(a, "width", nil, false); terr != nil {
			return nil, terr
		}
		if height, terr = argNumber(a, "height", nil, false); terr != nil {
			return nil, terr
		}
	}

	name := resolveApp(app)
	if name == "" {
		return nil, toolErr("app_not_found",
			fmt.Sprintf("app %q is not running", app),
			"call list_apps for running names")
	}
	var winSrc string
	if action == "unminimize" {
		// a minimized window is absent from CGWindowList (on-screen only), so
		// the pickWindow chain would report window_gone for exactly the window
		// being restored — address it through System Events, which still sees it
		winSrc, terr = minimizedWindowSrc(name, window, hasWindow)
		if terr != nil {
			return nil, terr
		}
	} else {
		cgWin, terr := pickWindow(name, window, hasWindow)
		if terr != nil {
			return nil, terr
		}
		winSrc, _, terr = seWindowSrc(name, cgWin)
		if terr != nil {
			return nil, terr
		}
	}

	src := "tell application \"System Events\"\n"
	switch action {
	case "move":
		src += fmt.Sprintf("    set position of %s to {%s, %s}\n",
			winSrc, scriptNum(x), scriptNum(y))
	case "resize":
		src += fmt.Sprintf("    set size of %s to {%s, %s}\n",
			winSrc, scriptNum(width), scriptNum(height))
	case "minimize":
		src += fmt.Sprintf("    set value of attribute \"AXMinimized\" of %s to true\n", winSrc)
	case "unminimize":
		src += fmt.Sprintf("    set value of attribute \"AXMinimized\" of %s to false\n", winSrc)
	case "zoom":
		src += fmt.Sprintf("    set value of attribute \"AXZoomed\" of %s to not (value of attribute \"AXZoomed\" of %s)\n",
			winSrc, winSrc)
	case "close":
		src += fmt.Sprintf("    click (first button of %s whose subrole is \"AXCloseButton\")\n", winSrc)
	}
	src += "end tell"

	if _, terr := osascript(src, "", 0); terr != nil {
		return nil, terr
	}
	markStale(st)
	return messagePayload{
		Result: fmt.Sprintf("Applied %s to a window of %q. Action completed. "+
			"Call get_app_state to see the updated UI.", action, name),
	}, nil
}
