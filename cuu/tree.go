package main

import (
	"fmt"
	"regexp"
	"strings"
)

// The AX tree is one big AppleScript: a recursive element dump where each
// line ends with ` @@<chain>`, chain being a dot-path of child indices.
// Values containing ' @@' can never forge an address because the chain is
// regex-validated after the LAST separator.

const treeDump = `
property idxCounter : 0

on txt(v)
    if v is missing value then return ""
    try
        set v to (v as text)
        set v to my replaceText(v, linefeed, " ")
        set v to my replaceText(v, return, " ")
        return my replaceText(v, tab, " ")
    on error
        return ""
    end try
end txt

on dumpEl(addr, chainStr, depth, maxDepth, maxEls)
    set out to ""
    tell application "System Events"
        set kids to UI elements of addr
        repeat with i from 1 to (count of kids)
            if idxCounter >= maxEls then exit repeat
            set idxCounter to idxCounter + 1
            set kid to (item i of kids)
            try
                set props to (properties of kid)
                set ln to "[" & idxCounter & "] " & my txt(role of props)
                try
                    if name of props is not missing value then set ln to ln & " \"" & (my txt(name of props)) & "\""
                end try
                try
                    if title of props is not missing value and (title of props as text) is not "" then set ln to ln & " t=\"" & (my txt(title of props)) & "\""
                end try
                set ln to ln & (my stringOfValue(props)) & (my stringOfFrame(props))
            on error
                set ln to "[" & idxCounter & "] AXUnreadable"
            end try
            if chainStr is "" then
                set kidChain to (i as text)
            else
                set kidChain to chainStr & "." & (i as text)
            end if
            set out to out & ln & " @@" & kidChain & linefeed
            if depth < maxDepth then
                set out to out & (my dumpEl(kid, kidChain, depth + 1, maxDepth, maxEls))
            end if
        end repeat
    end tell
    return out
end dumpEl

on stringOfValue(props)
    try
        set v to value of props
        if v is missing value then return ""
        if class of v is text or class of v is string then
            set t to v
            if (count of t) > 60 then set t to (text 1 thru 60 of t) & "…"
            set t to my replaceText(t, linefeed, " ")
            set t to my replaceText(t, return, " ")
            return " v=\"" & t & "\""
        else if class of v is boolean or class of v is integer or class of v is real then
            return " v=" & (v as text)
        end if
    end try
    return ""
end stringOfValue

on stringOfFrame(props)
    try
        set p to position of props
        set s to size of props
        if p is missing value or s is missing value then return ""
        return " @px:" & ((item 1 of p) as text) & "," & ((item 2 of p) as text) & ":" & ((item 1 of s) as text) & "x" & ((item 2 of s) as text)
    end try
    return ""
end stringOfFrame

on replaceText(t, f, r)
    set d to AppleScript's text item delimiters
    set AppleScript's text item delimiters to f
    set parts to text items of t
    set AppleScript's text item delimiters to r
    set out to parts as text
    set AppleScript's text item delimiters to d
    return out
end replaceText

tell application "System Events"
    set winAddr to %WIN%
    set header to "window " & (my txt(name of winAddr)) & linefeed
end tell
set idxCounter to 0
set body to my dumpEl(winAddr, "", 1, %MAXDEPTH%, %MAXELS%)
return header & body
`

// element lines: optional diff marker (+ new, ~ changed), index, body, chain
var elementLineRe = regexp.MustCompile(`^\[([+~]?)(\d+)\] (.*)$`)
var chainRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)

type parsedTree struct {
	Lines    []string
	Registry map[int]treeEntry
}

// parseDump splits raw osascript output into display lines and the index
// registry. Split out from the osascript call so it can be golden-tested
// without a GUI.
func parseDump(raw string) parsedTree {
	lines := []string{}
	registry := map[int]treeEntry{}
	for _, ln := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(ln, "[") {
			continue
		}
		if idx := strings.LastIndex(ln, " @@"); idx >= 0 {
			head := ln[:idx]
			chain := strings.TrimSpace(ln[idx+3:])
			if chainRe.MatchString(chain) {
				if m := elementLineRe.FindStringSubmatch(head); m != nil {
					i := 0
					fmt.Sscanf(m[2], "%d", &i)
					lines = append(lines, head)
					registry[i] = treeEntry{Chain: chain, Body: m[3]}
				}
				continue
			}
		}
		lines = append(lines, ln)
	}
	return parsedTree{Lines: lines, Registry: registry}
}

func dumpTree(winSrc string, maxDepth, maxEls int) (parsedTree, *ToolError) {
	src := strings.ReplaceAll(treeDump, "%WIN%", winSrc)
	src = strings.ReplaceAll(src, "%MAXDEPTH%", fmt.Sprintf("%d", maxDepth))
	src = strings.ReplaceAll(src, "%MAXELS%", fmt.Sprintf("%d", maxEls))
	raw, terr := osascript(src, "", 0)
	if terr != nil {
		return parsedTree{}, terr
	}
	return parseDump(raw), nil
}

type diffCounts struct {
	New          int  `json:"new"`
	Changed      int  `json:"changed"`
	Unchanged    int  `json:"unchanged"`
	Gone         int  `json:"gone"`
	FirstCapture bool `json:"first_capture"`
}

// diffTree compares registry bodies against the previous capture of the SAME
// app+window. Keying by chain (not index) is what survives index shifts: an
// element that moved keeps its identity. Returns counts and per-index
// markers; st.Prev* is updated in place.
func diffTree(st *serverState, registry map[int]treeEntry, app string, winID int64) (diffCounts, map[int][2]string) {
	key := fmt.Sprintf("%s|%d", app, winID)
	prev := map[string]string{}
	if st.PrevKey == key {
		for k, v := range st.PrevBodies {
			prev[k] = v
		}
	}
	byChain := map[string]string{}
	for _, e := range registry {
		byChain[e.Chain] = e.Body
	}
	marked := map[int][2]string{}
	counts := diffCounts{}
	for idx, e := range registry {
		old, seen := prev[e.Chain]
		switch {
		case !seen:
			marked[idx] = [2]string{"+", e.Body}
			counts.New++
		case old != e.Body:
			marked[idx] = [2]string{"~", e.Body}
			counts.Changed++
		default:
			marked[idx] = [2]string{"", e.Body}
			counts.Unchanged++
		}
	}
	for c := range prev {
		if _, still := byChain[c]; !still {
			counts.Gone++
		}
	}
	counts.FirstCapture = len(prev) == 0
	st.PrevKey = key
	st.PrevBodies = byChain
	return counts, marked
}
