package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// cuu — CLI verbs over the computer-use core, plus `cuu serve` (the thin
// stdio-MCP shim). Pi philosophy: every verb does one thing, prints one JSON
// document on stdout, and exits 0 (ok) / 1 (structured tool error, the same
// {"error":{code,message,remedy}} payload the MCP surface returns) / 2
// (usage). Verbs share the on-disk state.json with the MCP server, so an
// agent can mix `cuu` shell calls with an MCP session.

// cliTool maps one MCP tool onto CLI flags. Flags are single letters or
// words; each maps to a JSON argument by name, so the MCP schema stays the
// single source of truth for semantics.
type cliTool struct {
	tool  string   // MCP tool name = verb name
	flags []string // accepted flags, in help order
	build func(fs *flag.FlagSet) (args, bool, any)
}

func strFlag(fs *flag.FlagSet, name, def, usage string) *string {
	return fs.String(name, def, usage)
}

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	verb := os.Args[1]
	rest := os.Args[2:]
	switch verb {
	case "serve":
		if len(rest) > 0 {
			fmt.Fprintln(os.Stderr, "usage: cuu serve")
			os.Exit(2)
		}
		os.Exit(serve())
	case "selftest":
		os.Exit(selftest())
	case "version", "--version", "-v":
		fmt.Printf("%s %s (protocol %s, %s)\n", serverName, serverVersion,
			supportedProtocol1, supportedProtocol2)
		os.Exit(0)
	case "help", "--help", "-h":
		usage(os.Stdout)
		os.Exit(0)
	}
	tool, known := cliVerbs[verb]
	if !known {
		fmt.Fprintf(os.Stderr, "cuu: unknown verb %q (try `cuu help`)\n", verb)
		os.Exit(2)
	}
	os.Exit(runVerb(tool, rest))
}

// recorded values: flag.Flag does not expose whether a flag was set, so each
// value records it. Only explicitly-passed flags enter the JSON arguments —
// an absent --activate must stay "absent" (the tool default true applies),
// not "false".
type recValue struct {
	kind  string // "s", "b", "i", "n"
	strP  *string
	boolP *bool
	intP  *int
	numP  *float64
	setP  *bool
}

func (v recValue) String() string {
	switch v.kind {
	case "b":
		return fmt.Sprintf("%v", *v.boolP)
	case "i":
		return fmt.Sprintf("%d", *v.intP)
	case "n":
		return fmt.Sprintf("%v", *v.numP)
	default:
		return *v.strP
	}
}

func (v recValue) Set(s string) error {
	switch v.kind {
	case "b":
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		*v.boolP, *v.setP = b, true
	case "i":
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return err
		}
		*v.intP, *v.setP = int(n), true
	case "n":
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		*v.numP, *v.setP = f, true
	default:
		*v.strP, *v.setP = s, true
	}
	return nil
}

func (v recValue) IsBoolFlag() bool { return v.kind == "b" }

func runVerb(t cliTool, argv []string) int {
	fs := flag.NewFlagSet("cuu "+t.tool, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: cuu %s", t.tool)
		for _, f := range t.flags {
			fmt.Fprintf(os.Stderr, " --%s", f)
		}
		fmt.Fprintln(os.Stderr)
	}
	collect := map[string]any{}
	for _, fname := range t.flags {
		kind, name := fname, fname
		if i := strings.Index(fname, ":"); i > 0 {
			kind, name = fname[:i], fname[i+1:]
		}
		set := new(bool)
		switch kind {
		case "b":
			fs.Var(recValue{kind: "b", boolP: new(bool), setP: set}, name, "")
		case "i":
			fs.Var(recValue{kind: "i", intP: new(int), setP: set}, name, "")
		case "n":
			fs.Var(recValue{kind: "n", numP: new(float64), setP: set}, name, "")
		default:
			fs.Var(recValue{kind: "s", strP: new(string), setP: set}, name, "")
		}
	}
	if err := fs.Parse(argv); err != nil {
		os.Exit(2)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "cuu: unexpected positional argument %q\n", fs.Arg(0))
		os.Exit(2)
	}
	fs.Visit(func(f *flag.Flag) {
		// recover the recorded value from the Value we installed
		if rv, ok := f.Value.(recValue); ok && *rv.setP {
			switch rv.kind {
			case "b":
				collect[f.Name] = *rv.boolP
			case "i":
				collect[f.Name] = json.Number(fmt.Sprintf("%d", *rv.intP))
			case "n":
				collect[f.Name] = json.Number(strconv.FormatFloat(*rv.numP, 'g', -1, 64))
			default:
				collect[f.Name] = *rv.strP
			}
		}
	})
	st := loadState()
	payload, terr := runTool(t.tool, st, collect)
	if terr != nil {
		fmt.Println(dumpJSONCompact(terr.payload()))
		return 1
	}
	fmt.Println(dumpJSON(payload))
	return 0
}

func usage(w *os.File) {
	fmt.Fprintln(w, "cuu — macOS GUI control (zcode-computer-use "+serverVersion+")")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "MCP stdio server:")
	fmt.Fprintln(w, "  cuu serve                       speak MCP over stdio")
	fmt.Fprintln(w, "  cuu selftest                    dependency + permission preflight")
	fmt.Fprintln(w, "  cuu version")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "CLI verbs (one JSON document on stdout; exit 0 ok, 1 tool error, 2 usage):")
	names := make([]string, 0, len(cliVerbs))
	for name := range cliVerbs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t := cliVerbs[name]
		line := "  cuu " + name
		for _, f := range t.flags {
			line += " --" + f
		}
		fmt.Fprintln(w, line)
	}
}

// handlerExists reports whether an MCP tool name has an implementation
// (perform_secondary_action is handled as an alias inside runTool).
func handlerExists(name string) bool {
	if name == "perform_secondary_action" {
		return true
	}
	_, ok := cliVerbs[name]
	return ok
}

// runTool dispatches one tool call by name. CLI verbs and tools/call share it.
func runTool(name string, st *serverState, a args) (any, *ToolError) {
	switch name {
	case "list_apps":
		return toolListApps()
	case "list_windows":
		app, terr := argStr(a, "app", true)
		if terr != nil {
			return nil, terr
		}
		return toolListWindows(app)
	case "get_app_state":
		return toolGetAppState(st, a)
	case "click":
		return toolClick(st, a)
	case "type_text":
		return toolTypeText(st, a)
	case "press_key":
		return toolPressKey(st, a)
	case "scroll":
		return toolScroll(st, a)
	case "drag":
		return toolDrag(st, a)
	case "set_value":
		return toolSetValue(st, a)
	case "select_text":
		return toolSelectText(st, a)
	case "element_info":
		return toolElementInfo(st, a)
	case "perform_action", "perform_secondary_action":
		return toolPerformAction(st, a)
	case "find":
		return toolFind(st, a)
	case "menu":
		return toolMenu(st, a)
	case "wait_for":
		return toolWaitFor(st, a)
	case "clipboard":
		return toolClipboard(a)
	case "window":
		return toolWindow(st, a)
	case "ocr":
		return toolOCR(st, a)
	default:
		return nil, toolErr("internal", "no handler for "+name, "")
	}
}

// cliVerbs: verb name -> MCP tool + accepted flags. "b:" bool, "i:" int,
// "n:" number prefixes select the flag type; bare names are strings.
var cliVerbs = map[string]cliTool{
	"list_apps":      {tool: "list_apps"},
	"list_windows":   {tool: "list_windows", flags: []string{"app"}},
	"get_app_state":  {tool: "get_app_state", flags: []string{"app", "i:depth", "window", "b:activate", "b:launch", "filter", "b:include_screenshot"}},
	"click":          {tool: "click", flags: []string{"i:element_index", "n:x", "n:y", "mouse_button", "i:click_count"}},
	"type_text":      {tool: "type_text", flags: []string{"text", "method"}},
	"press_key":      {tool: "press_key", flags: []string{"key"}},
	"scroll":         {tool: "scroll", flags: []string{"direction", "n:pages", "n:x", "n:y"}},
	"drag":           {tool: "drag", flags: []string{"n:from_x", "n:from_y", "n:to_x", "n:to_y"}},
	"set_value":      {tool: "set_value", flags: []string{"i:element_index", "value"}},
	"select_text":    {tool: "select_text", flags: []string{"i:element_index", "i:start", "i:length"}},
	"element_info":   {tool: "element_info", flags: []string{"i:element_index"}},
	"perform_action": {tool: "perform_action", flags: []string{"i:element_index", "action"}},
	"find":           {tool: "find", flags: []string{"text", "role", "i:limit"}},
	"menu":           {tool: "menu", flags: []string{"app", "path"}},
	"wait_for":       {tool: "wait_for", flags: []string{"text", "until", "n:timeout_s"}},
	"clipboard":      {tool: "clipboard", flags: []string{"text"}},
	"window":         {tool: "window", flags: []string{"app", "window", "action", "n:x", "n:y", "n:width", "n:height"}},
	"ocr":            {tool: "ocr", flags: []string{"filter"}},
}
