package main

import (
	"fmt"
	"strings"
	"time"
)

// `clipboard` reads or sets the pasteboard explicitly. Until now it was only
// touched implicitly by type_text's paste path; agents need to seed the
// clipboard (or inspect what the user copied) without typing anything.
// The clipboard is global state owned by the OS, not this server: no
// requireState, no markStale.

const clipboardTextMaxChars = 100000

type clipboardReadPayload struct {
	Text string `json:"text"`
	Hint string `json:"hint"`
}

func toolClipboard(a args) (any, *ToolError) {
	text, terr := argStr(a, "text", false)
	if terr != nil {
		return nil, terr
	}
	if text != "" {
		res := runCmd(10*time.Second, text, "pbcopy")
		if res == nil || res.ExitCode != 0 {
			return nil, toolErr("internal", "pbcopy failed — clipboard not set",
				"retry; if it persists check disk space and "+
					"/usr/bin/pbcopy permissions")
		}
		return messagePayload{
			Result: fmt.Sprintf("Clipboard set to %d character(s).", len([]rune(text))),
		}, nil
	}

	probe := runCmd(10*time.Second, "", "pbpaste", "-Prefer", "txt")
	if probe == nil || probe.ExitCode != 0 {
		return nil, toolErr("internal", "pbpaste failed — clipboard not read",
			"retry; if it persists check /usr/bin/pbpaste permissions")
	}
	hint := ""
	if probe.Stdout == "" {
		// an empty text read may mean an empty clipboard or non-text content
		// (image/files) pbpaste cannot give back — tell them apart the same
		// way typeViaClipboard does
		types := runCmd(10*time.Second, "", "osascript", "-e", "return (clipboard info) as text")
		if types != nil {
			t := strings.TrimSpace(types.Stdout)
			if t != "" && t != "0" {
				hint = "clipboard holds non-text content (image or file list); pbpaste returned nothing"
			}
		}
	}
	text = probe.Stdout
	if r := []rune(text); len(r) > clipboardTextMaxChars {
		text = string(r[:clipboardTextMaxChars])
		hint = fmt.Sprintf("text truncated at %d of %d character(s)",
			clipboardTextMaxChars, len(r))
	}
	return clipboardReadPayload{Text: text, Hint: hint}, nil
}
