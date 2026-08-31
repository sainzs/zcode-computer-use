package main

import (
	"fmt"
	"strings"
	"time"
)

// The menu bar is where most macOS app functionality actually lives, and it
// is invisible to window captures (the AX tree binds to a window, not the
// process). `menu` closes that gap: list the bar, or click one item by a
// " > "-separated path. Same System Events stack, same escaping discipline
// as element actions.

const menuPathMaxDepth = 6

type menuEntry struct {
	Title string   `json:"title"`
	Items []string `json:"items"`
}

type menuListPayload struct {
	App   string      `json:"app"`
	Menus []menuEntry `json:"menus"`
	Hint  string      `json:"hint"`
}

// splitMenuPath splits "File > Export as PDF…" into trimmed segments.
// Segments never contain ">" — no real menu title does, and keeping the
// separator unescapable keeps the address builder simple.
func splitMenuPath(path string) ([]string, *ToolError) {
	var segments []string
	for _, part := range strings.Split(path, ">") {
		if t := strings.TrimSpace(part); t != "" {
			segments = append(segments, t)
		}
	}
	if len(segments) < 2 {
		return nil, toolErr("invalid_args",
			"path needs at least a menu and an item, e.g. \"File > Save\"",
			"call menu without path to list the menu bar first")
	}
	if len(segments) > menuPathMaxDepth {
		return nil, toolErr("invalid_args",
			fmt.Sprintf("path deeper than %d levels", menuPathMaxDepth), "")
	}
	return segments, nil
}

// menuItemAddress builds the System Events address for a menu path, escaped
// segment by segment — menu text can never smuggle AppleScript. For
// ["File","Export","PDF"] it yields
//
//	menu item "PDF" of menu "Export" of menu item "Export" of menu "File" of menu bar item "File" of menu bar 1
func menuItemAddress(segments []string) (string, *ToolError) {
	top, terr := asEsc(segments[0])
	if terr != nil {
		return "", terr
	}
	// asEsc output goes inside plain AppleScript double quotes — %q would
	// re-escape with Go rules and corrupt the literal
	addr := fmt.Sprintf("menu \"%s\" of menu bar item \"%s\" of menu bar 1", top, top)
	for _, seg := range segments[1 : len(segments)-1] {
		esc, terr := asEsc(seg)
		if terr != nil {
			return "", terr
		}
		addr = fmt.Sprintf("menu \"%s\" of menu item \"%s\" of %s", esc, esc, addr)
	}
	last, terr := asEsc(segments[len(segments)-1])
	if terr != nil {
		return "", terr
	}
	return fmt.Sprintf("menu item \"%s\" of %s", last, addr), nil
}

// menuDump lists the menu bar: one line per top-level menu, items joined by
// U+001F (the server's field separator — never legal in UI text, and
// type_text already rejects it on the way in). Separator items have no name
// and are skipped inside the script.
const menuDumpSrc = `
tell application "System Events"
    set out to ""
    repeat with mbi in menu bar items of menu bar 1 of process "%APP%"
        set mn to ""
        try
            set mn to (name of mbi) as text
        end try
        if mn is not "" then
            set out to out & mn
            try
                repeat with mi in menu items of menu 1 of mbi
                    set nm to ""
                    try
                        -- separators have a missing-value name, which AS
                        -- happily coerces to the STRING "missing value"
                        if name of mi is not missing value then set nm to (name of mi) as text
                    end try
                    if nm is not "" then set out to out & (ASCII character 31) & nm
                end repeat
            end try
            set out to out & linefeed
        end if
    end repeat
    return out
end tell`

func toolMenu(st *serverState, a args) (any, *ToolError) {
	appRaw, terr := argStr(a, "app", true)
	if terr != nil {
		return nil, terr
	}
	path, terr := argStr(a, "path", false)
	if terr != nil {
		return nil, terr
	}
	name := resolveApp(appRaw)
	if name == "" {
		return nil, toolErr("app_not_found",
			fmt.Sprintf("app %q is not running", appRaw),
			"call list_apps for running names")
	}
	escaped, terr := asEsc(name)
	if terr != nil {
		return nil, terr
	}

	if path == "" {
		raw, terr := osascript(strings.ReplaceAll(menuDumpSrc, "%APP%", escaped), "", 0)
		if terr != nil {
			return nil, terr
		}
		menus := []menuEntry{}
		for _, ln := range strings.Split(raw, "\n") {
			if ln == "" {
				continue
			}
			fields := strings.Split(ln, "\x1f")
			items := fields[1:]
			if items == nil {
				items = []string{}
			}
			menus = append(menus, menuEntry{Title: fields[0], Items: items})
		}
		return menuListPayload{
			App:   name,
			Menus: menus,
			Hint: "click one with path, e.g. {\"app\": \"" + name +
				"\", \"path\": \"File > Save\"} — deeper submenus chain with more \" > \"",
		}, nil
	}

	segments, terr := splitMenuPath(path)
	if terr != nil {
		return nil, terr
	}
	addr, terr := menuItemAddress(segments)
	if terr != nil {
		return nil, terr
	}
	// menu clicks are user-visible by nature; front the app so the pull-down
	// happens on the window the owner is watching
	src := "tell application \"System Events\"\n" +
		fmt.Sprintf("    set frontmost of process \"%s\" to true\n", escaped) +
		fmt.Sprintf("    click (%s of process \"%s\")\n", addr, escaped) +
		"end tell"
	if _, terr := osascript(src, "", 0); terr != nil {
		return nil, classifyMenuError(terr, path)
	}
	time.Sleep(keypageDelayMs * time.Millisecond)
	markStale(st)
	return messagePayload{
		Result: fmt.Sprintf("Clicked menu item %q. Action completed. Call "+
			"get_app_state to see the updated UI.", strings.Join(segments, " > ")),
	}, nil
}

// classifyMenuError turns System Events' "Can't get menu item …" into a
// branchable code; everything else passes through unchanged.
func classifyMenuError(terr *ToolError, path string) *ToolError {
	if terr.Code != "osascript_error" {
		return terr
	}
	low := strings.ToLower(terr.Message)
	// osascript renders the apostrophe as U+2019 ("can’t get")
	if strings.Contains(low, "can’t get") || strings.Contains(low, "can't get") {
		return toolErr("menu_not_found",
			fmt.Sprintf("no menu item at path %q", path),
			"call menu without path to list the menu bar (titles are "+
				"exact, including … and case)")
	}
	return terr
}
