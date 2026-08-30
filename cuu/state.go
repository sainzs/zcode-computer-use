package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// The capture/action state lives ON DISK (state.json in the data dir), not in
// process memory: CLI verbs are separate processes, and the MCP shim must see
// what they see. The Python server held this in module globals; the port moves
// it to the one place both front ends read. Writes are atomic (temp + rename).

type treeEntry struct {
	Chain string `json:"chain"`
	Body  string `json:"body"`
}

type serverState struct {
	App        string   `json:"app"`        // System Events process name
	Screenshot string   `json:"screenshot"` // latest PNG path
	Scale      *float64 `json:"scale"`      // screenshot pixels per screen point
	OriginX    *float64 `json:"origin_x"`   // screen points of the captured
	OriginY    *float64 `json:"origin_y"`   // window origin
	WinID      *int64   `json:"win_id"`     // CGWindowID of the captured window
	WinSrc     string   `json:"win_src"`    // SE address fragment, e.g.
	//                                '(window 1 of process "TextEdit")' — built fresh at
	//                                capture time so element addresses always resolve
	//                                against the captured window
	Elements map[int]treeEntry `json:"elements"` // index -> chain + body
	Stale    bool              `json:"stale"`
	Counter  int               `json:"counter"`
	// diff memory: chain -> body of the previous capture of the SAME app+window
	PrevKey    string            `json:"prev_key"` // "" = none
	PrevBodies map[string]string `json:"prev_bodies"`
}

func statePath() string { return filepath.Join(dataDir(), "state.json") }

func loadState() *serverState {
	st := &serverState{Elements: map[int]treeEntry{}, Stale: true, PrevBodies: map[string]string{}}
	raw, err := os.ReadFile(statePath())
	if err != nil {
		return st
	}
	if err := json.Unmarshal(raw, st); err != nil {
		return &serverState{Elements: map[int]treeEntry{}, Stale: true, PrevBodies: map[string]string{}}
	}
	if st.Elements == nil {
		st.Elements = map[int]treeEntry{}
	}
	if st.PrevBodies == nil {
		st.PrevBodies = map[string]string{}
	}
	return st
}

func saveState(st *serverState) {
	raw, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(dataDir(), ".state-*.tmp")
	if err != nil {
		return
	}
	name := tmp.Name()
	_, werr := tmp.Write(raw)
	cerr := tmp.Close()
	if werr == nil && cerr == nil {
		_ = os.Chmod(name, 0o600)
		_ = os.Rename(name, statePath())
		return
	}
	_ = os.Remove(name)
}

func markStale(st *serverState) {
	st.Stale = true
	st.Elements = map[int]treeEntry{}
	saveState(st)
}

// elementSource rebuilds the System Events address from the stored index
// chain with a freshly built win_src — never trust round-tripped text.
func elementSource(st *serverState, index int) (string, *ToolError) {
	entry, ok := st.Elements[index]
	if !ok {
		return "", toolErr(
			"unknown_element",
			fmt.Sprintf("unknown element_index %d", index),
			"call get_app_state and use an index from the current tree")
	}
	src := st.WinSrc
	if src == "" {
		escaped, terr := asEsc(st.App)
		if terr != nil {
			return "", terr
		}
		src = fmt.Sprintf("(window 1 of process \"%s\")", escaped)
	}
	for _, part := range strings.Split(entry.Chain, ".") {
		n := 0
		fmt.Sscanf(part, "%d", &n)
		src = fmt.Sprintf("(UI element %d of %s)", n, src)
	}
	return src, nil
}

// requireState gates element actions on freshness: index mapping can never
// outlive the tree it came from.
func requireState(st *serverState, forElements bool) *ToolError {
	if st.App == "" {
		return toolErr("no_state", "no app state yet",
			"call get_app_state first")
	}
	if forElements && st.Stale {
		return toolErr(
			"stale_state",
			"state is stale after a previous action",
			"call get_app_state again before using element_index "+
				"(index mapping must never outlive the tree it came from)")
	}
	return nil
}

// pruneScreenshots keeps the newest keepScreenshots captures of THIS process
// (filenames are pid-scoped so a second server instance never prunes ours)
// and best-effort removes captures from pids that no longer exist.
func pruneScreenshots(keep string) {
	dir := dataDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := fmt.Sprintf("state-%d-", os.Getpid())
	var mine []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "state-") || !strings.HasSuffix(name, ".png") {
			continue
		}
		full := filepath.Join(dir, name)
		if keep != "" && full == keep {
			continue
		}
		if strings.HasPrefix(name, prefix) {
			mine = append(mine, full)
			continue
		}
		if pidFromShotName(name) > 0 && !pidAlive(pidFromShotName(name)) {
			_ = os.Remove(full)
		}
	}
	sort.Strings(mine) // zero-padded counter => lexicographic == numeric order
	for _, old := range mine[:max(0, len(mine)-keepScreenshots)] {
		_ = os.Remove(old)
	}
}

// pidFromShotName parses the pid out of "state-<pid>-<counter>.png".
// Pre-4.0 files ("state-<counter>.png") return 0 and are only cleaned when
// already old beyond our own keep window is impossible to know — they are
// left to the user's cache hygiene.
func pidFromShotName(name string) int {
	body := strings.TrimSuffix(strings.TrimPrefix(name, "state-"), ".png")
	parts := strings.SplitN(body, "-", 2)
	if len(parts) != 2 {
		return 0
	}
	pid := 0
	fmt.Sscanf(parts[0], "%d", &pid)
	return pid
}

func pidAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func shotPath(counter int) string {
	return filepath.Join(dataDir(), fmt.Sprintf("state-%d-%06d.png", os.Getpid(), counter))
}
