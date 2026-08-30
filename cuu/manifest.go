package main

// toolsManifest is the MCP tools/list payload — the tool surface is
// unchanged from 3.0.1 so callers need no re-education. Kept as a raw JSON
// constant so object key order is stable.

const toolsManifest = `[
 {
  "name": "list_apps",
  "description": "List running GUI apps and the frontmost one.",
  "inputSchema": {"type": "object", "properties": {}}
 },
 {
  "name": "list_windows",
  "description": "List an app's on-screen windows (id, title, bounds, frontmost flag) so you can target one with get_app_state's ` + "`window`" + ` argument.",
  "inputSchema": {
   "type": "object",
   "properties": {"app": {"type": "string"}},
   "required": ["app"]
  }
 },
 {
  "name": "get_app_state",
  "description": "Return an app window's screenshot (PNG path), pixel scale, and an indexed accessibility tree with diff markers against the previous capture of the same window ([+n] new, [~n] changed). Call ONCE per observation — do not re-call without acting in between. Element targets are preferred; fall back to x/y in screenshot pixels. ` + "`window`" + ` targets a specific window by id or title substring; ` + "`activate:false`" + ` observes without stealing focus; ` + "`launch:true`" + ` is required to open a non-running app.",
  "inputSchema": {
   "type": "object",
   "properties": {
    "app": {"type": "string", "description": "app name, path, or partial match"},
    "depth": {"type": "integer", "minimum": 0, "maximum": 12, "default": 8},
    "window": {"description": "window id (integer) or title substring; default frontmost"},
    "activate": {"type": "boolean", "default": true, "description": "false = background observe, no focus steal"},
    "launch": {"type": "boolean", "default": false, "description": "true = open the app if not running"}
   },
   "required": ["app"]
  }
 },
 {
  "name": "click",
  "description": "Click an element by index (preferred; left-press via AXPress) or by screenshot pixel coordinates (any button, click_count 1-2; double-click is left-only).",
  "inputSchema": {
   "type": "object",
   "properties": {
    "element_index": {"type": "integer"},
    "x": {"type": "number"},
    "y": {"type": "number"},
    "mouse_button": {"type": "string", "enum": ["left", "right", "middle"], "default": "left", "description": "coordinate clicks only"},
    "click_count": {"type": "integer", "minimum": 1, "maximum": 5, "default": 1}
   }
  }
 },
 {
  "name": "type_text",
  "description": "Type literal text into the frontmost app. method=paste (default) is fast and unicode-safe but briefly uses the clipboard (restored as plain text — rich formatting can be lost; method=keys never touches the clipboard but is slow and capped at 2000 characters).",
  "inputSchema": {
   "type": "object",
   "properties": {
    "text": {"type": "string"},
    "method": {"type": "string", "enum": ["paste", "keys"], "default": "paste"}
   },
   "required": ["text"]
  }
 },
 {
  "name": "press_key",
  "description": "Press a key or chord in xdotool syntax (super+c, Return, KP_0, ctrl+shift+t).",
  "inputSchema": {
   "type": "object",
   "properties": {"key": {"type": "string"}},
   "required": ["key"]
  }
 },
 {
  "name": "scroll",
  "description": "Scroll up/down/left/right by pages, optionally at given screenshot pixel coordinates (recommended: wheel events land under the cursor, so pass x/y or position first).",
  "inputSchema": {
   "type": "object",
   "properties": {
    "direction": {"type": "string", "enum": ["up", "down", "left", "right"]},
    "pages": {"type": "number", "default": 1},
    "x": {"type": "number"},
    "y": {"type": "number"}
   }
  }
 },
 {
  "name": "drag",
  "description": "Drag from one screenshot pixel point to another.",
  "inputSchema": {
   "type": "object",
   "properties": {
    "from_x": {"type": "number"},
    "from_y": {"type": "number"},
    "to_x": {"type": "number"},
    "to_y": {"type": "number"}
   },
   "required": ["from_x", "from_y", "to_x", "to_y"]
  }
 },
 {
  "name": "set_value",
  "description": "Set the value of a settable accessibility element by index (no keystrokes, no clipboard).",
  "inputSchema": {
   "type": "object",
   "properties": {
    "element_index": {"type": "integer"},
    "value": {"type": "string"}
   },
   "required": ["element_index", "value"]
  }
 },
 {
  "name": "select_text",
  "description": "Select a character range [start, start+length) in a text element by index. The selection is verified by content and the chosen method is reported; apps that ignore the AX range write get a keyboard-walk fallback (capped at 400 characters).",
  "inputSchema": {
   "type": "object",
   "properties": {
    "element_index": {"type": "integer"},
    "start": {"type": "integer", "minimum": 0},
    "length": {"type": "integer", "minimum": 1}
   },
   "required": ["element_index", "start", "length"]
  }
 },
 {
  "name": "element_info",
  "description": "Inspect one element by index: role, name, value, enabled/focused, frame, and the AX actions it exposes (for perform_action).",
  "inputSchema": {
   "type": "object",
   "properties": {"element_index": {"type": "integer"}},
   "required": ["element_index"]
  }
 },
 {
  "name": "perform_action",
  "description": "Invoke a named accessibility action on an element (AXPick, AXConfirm, AXCancel, AXIncrement, ...). Discover names with element_info. (The v2 name perform_secondary_action still works as an alias.)",
  "inputSchema": {
   "type": "object",
   "properties": {
    "element_index": {"type": "integer"},
    "action": {"type": "string", "description": "AX action name, e.g. AXPick"}
   },
   "required": ["element_index", "action"]
  }
 }
]`
