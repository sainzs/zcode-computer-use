package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// find re-queries the CURRENT capture's registry without touching the GUI —
// a filtered view over state, not a new observation. wait_for polls the AX
// tree of the captured window until a text condition holds, collapsing the
// act → observe → "not ready yet" → observe turn loop into one call.

const (
	findLimitDefault = 40
	findLimitMax     = 200
	waitTimeoutDef   = 10.0
	waitTimeoutMax   = 60.0
	waitPollMs       = 600
)

type findPayload struct {
	Result  string   `json:"result"`
	Matches []string `json:"matches"`
	Hint    string   `json:"hint"`
}

// matchElements returns the indices whose body matches: text is a
// case-insensitive substring of the whole body; role (e.g. "AXButton")
// must equal the body's leading role token. Pure function, golden-tested.
func matchElements(elements map[int]treeEntry, text, role string) []int {
	low := strings.ToLower(text)
	var out []int
	for idx, e := range elements {
		if role != "" {
			tok := e.Body
			if i := strings.IndexByte(tok, ' '); i >= 0 {
				tok = tok[:i]
			}
			if tok != role {
				continue
			}
		}
		if low != "" && !strings.Contains(strings.ToLower(e.Body), low) {
			continue
		}
		out = append(out, idx)
	}
	sort.Ints(out)
	return out
}

func toolFind(st *serverState, a args) (any, *ToolError) {
	text, terr := argStr(a, "text", false)
	if terr != nil {
		return nil, terr
	}
	role, terr := argStr(a, "role", false)
	if terr != nil {
		return nil, terr
	}
	if text == "" && role == "" {
		return nil, toolErr("invalid_args", "pass text and/or role",
			"e.g. {\"text\": \"Save\"} or {\"role\": \"AXButton\"}")
	}
	minOne, maxLim := 1, findLimitMax
	limit, terr := argInt(a, "limit", findLimitDefault, true, &minOne, &maxLim)
	if terr != nil {
		return nil, terr
	}
	if terr := requireState(st, true); terr != nil {
		return nil, terr
	}
	indices := matchElements(st.Elements, text, role)
	matches := []string{}
	for _, idx := range indices {
		if len(matches) >= limit {
			break
		}
		matches = append(matches, fmt.Sprintf("[%d] %s", idx, st.Elements[idx].Body))
	}
	result := fmt.Sprintf("%d of %d elements match", len(indices), len(st.Elements))
	if len(indices) > limit {
		result += fmt.Sprintf(" (showing first %d)", limit)
	}
	hint := "indices are valid for element actions until the next action"
	if len(indices) == 0 {
		hint = "no match in the current tree — broaden text, raise depth, " +
			"or get_app_state again"
	}
	return findPayload{Result: result, Matches: matches, Hint: hint}, nil
}

type waitForPayload struct {
	Result  string `json:"result"`
	Elapsed string `json:"elapsed_s"`
	Hint    string `json:"hint"`
}

func toolWaitFor(st *serverState, a args) (any, *ToolError) {
	text, terr := argStr(a, "text", true)
	if terr != nil {
		return nil, terr
	}
	until := "present"
	if u, ok := a.raw("until"); ok && u != nil {
		s, isStr := u.(string)
		if !isStr || (s != "present" && s != "gone") {
			return nil, toolErr("invalid_args",
				fmt.Sprintf("unknown until %s", pyRepr(u)),
				"until must be 'present' (default) or 'gone'")
		}
		until = s
	}
	timeout, terr := argNumber(a, "timeout_s", waitTimeoutDef, true)
	if terr != nil {
		return nil, terr
	}
	if timeout < 1 || timeout > waitTimeoutMax {
		return nil, toolErr("invalid_args",
			fmt.Sprintf("timeout_s must be 1-%d", int(waitTimeoutMax)), "")
	}
	// needs a captured window to poll, but NOT fresh element indices —
	// waiting is exactly what happens between an action and a re-observe
	if terr := requireState(st, false); terr != nil {
		return nil, terr
	}
	if st.WinSrc == "" {
		return nil, toolErr("no_state", "no captured window to poll",
			"call get_app_state first")
	}

	low := strings.ToLower(text)
	started := time.Now()
	deadline := started.Add(time.Duration(timeout * float64(time.Second)))
	for {
		// poll at the hard depth cap: text visible to a deep get_app_state
		// must be visible to the wait too (the 350-element cap still applies)
		parsed, terr := dumpTree(st.WinSrc, maxDepthHard, maxElements)
		if terr != nil {
			// the watched window closing IS the primary "gone" outcome — a
			// dismissed dialog takes its tree (and our address) with it
			if until == "gone" && terr.Code == "osascript_error" {
				elapsed := pyRound(time.Since(started).Seconds(), 1)
				markStale(st)
				return waitForPayload{
					Result: fmt.Sprintf("%q is gone after %s s (the captured "+
						"window itself went away).", text, pyFloatStr(elapsed)),
					Elapsed: pyFloatStr(elapsed),
					Hint:    "call get_app_state to observe and get fresh indices",
				}, nil
			}
			return nil, terr
		}
		found := false
		for _, e := range parsed.Registry {
			if strings.Contains(strings.ToLower(e.Body), low) {
				found = true
				break
			}
		}
		if (until == "present") == found {
			elapsed := pyRound(time.Since(started).Seconds(), 1)
			// the tree we polled is not the tree the indices came from
			markStale(st)
			return waitForPayload{
				Result: fmt.Sprintf("%q is %s after %s s.", text, until,
					pyFloatStr(elapsed)),
				Elapsed: pyFloatStr(elapsed),
				Hint:    "call get_app_state to observe and get fresh indices",
			}, nil
		}
		if !time.Now().Add(waitPollMs * time.Millisecond).Before(deadline) {
			markStale(st)
			return nil, toolErr("wait_timeout",
				fmt.Sprintf("%q still not %s after %s s", text, until,
					pyFloatStr(pyRound(time.Since(started).Seconds(), 1))),
				"get_app_state to see where the UI actually is; raise "+
					"timeout_s if the operation is just slow")
		}
		time.Sleep(waitPollMs * time.Millisecond)
	}
}
