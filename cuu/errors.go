package main

import "strings"

// ToolError is a tool failure with a machine-readable code and an
// agent-usable remedy. Rendered into the tool result as structured JSON so
// the caller can branch on `code` instead of parsing prose.
type ToolError struct {
	Code    string
	Message string
	Remedy  string
}

func (e *ToolError) Error() string { return e.Message }

// payload renders the wire form: {"error":{"code","message"[,"remedy"]}}.
func (e *ToolError) payload() map[string]any {
	err := map[string]any{"code": e.Code, "message": e.Message}
	if e.Remedy != "" {
		err["remedy"] = e.Remedy
	}
	return map[string]any{"error": err}
}

func toolErr(code, message, remedy string) *ToolError {
	return &ToolError{Code: code, Message: message, Remedy: remedy}
}

// classifyOsascriptError maps a raw osascript stderr onto the two permission
// errors agents actually need to act on, with osascript_error as the catch-all.
func classifyOsascriptError(excMessage string) *ToolError {
	low := strings.ToLower(excMessage)
	if strings.Contains(low, "assistive access") || strings.Contains(low, "-1719") ||
		strings.Contains(low, "-25211") || strings.Contains(low, "not allowed assistive") {
		return toolErr(
			"permission_accessibility",
			"the host app lacks the Accessibility grant",
			"grant Accessibility to the host app (ZCode or your terminal) in "+
				"System Settings > Privacy & Security > Accessibility, then "+
				"restart the host app")
	}
	if strings.Contains(low, "screencapture") || strings.Contains(low, "screen recording") {
		return toolErr(
			"permission_screen_recording",
			"window capture failed — the host app may lack Screen Recording",
			"grant Screen Recording in System Settings > Privacy & Security "+
				"> Screen Recording, then restart the host app")
	}
	msg := excMessage
	if msg == "" {
		msg = "osascript failed"
	}
	return toolErr("osascript_error", msg,
		"see the server log for the full AppleScript error")
}
