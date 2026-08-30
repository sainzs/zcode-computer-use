---
name: computer-use
description: Control macOS GUI apps from ZCode via the zcode-computer-use MCP server — per-window screenshot plus an indexed accessibility tree with diff markers, then element-first actions with screenshot-pixel fallback. Use when the owner asks to click, type, read, select, or drive a native app that browser automation cannot reach (Finder dialogs, Adobe desktop apps, system panels, installers).
license: MIT
---

# Computer use (macOS, ZCode v4)

Element-first with screenshot-pixel coordinate fallback, forced state
re-fetch after every action, and diff markers so one re-observe tells you
exactly what your last action changed. The server is the `cuu` Go binary;
every tool is also a CLI verb (`cuu click --element_index 4` → one JSON
document, exit 0/1/2) sharing the same state file as the MCP session.

## Prerequisites (once per host app)

The MCP server is spawned by ZCode, so **ZCode** needs both grants in
System Settings → Privacy & Security:

- **Accessibility** (to read and drive the AX tree)
- **Screen Recording** (for `screencapture` window captures)

`cliclick` must be on PATH (`brew install cliclick`); scroll uses Apple's
bundled `/usr/bin/python3` (present with Xcode CLT).
Preflight: `plugins/zcode-computer-use/cuu/bin/cuu selftest` — it names
the exact missing grant. (Build first if the binary is missing:
`cd plugins/zcode-computer-use/cuu && go build -o bin/cuu .`)

## The operating loop

1. `list_apps` if you don't know the app name; `list_windows` to target a
   specific window.
2. `get_app_state(app, window?, depth?, activate?)` — returns `screenshot`
   (PNG path — **Read it**), `pixel_scale`, and an indexed AX tree with
   diff markers: `[+n]` new, `[~n]` changed vs the previous capture of the
   same window. Call ONCE per observation; re-calling without acting
   between calls is the classic waste.
3. Prefer element targets: `click element_index` (AXPress), `set_value`,
   `select_text`, `perform_action` (discover action names with
   `element_info`). Fall back to `x`,`y` in **screenshot pixel
   coordinates** — never raw screen points; the server rescales and
   re-anchors to the captured window.
4. After every action the state is stale: `get_app_state` again before the
   next element action. The diff summary (`+n new, ~n changed, n gone`)
   shows what your action did — scan marked lines first.

v3 specifics worth reaching for:

- `activate:false` observes a background window without stealing focus
  (the header says "background").
- A non-running app is an `app_not_found` error — pass `launch:true` to
  open it deliberately.
- `select_text` verifies the selection against element content and reports
  the method (`ax_range` or `keyboard`); trust `selected_text`, not the
  click.
- `type_text` paste is fast but touches the clipboard (auto-restored);
  `method:"keys"` types real keystrokes when the clipboard must not change
  or the target ignores pastes.
- Errors are structured JSON (`{"error":{"code","message","remedy"}}`) —
  branch on `code` (`stale_state`, `window_gone`,
  `permission_accessibility`, …) and follow `remedy`.

## Confirmation policy (default: confirm-at-action)

ZCode's own permission prompts gate each tool call. Beyond that:

- Announce what you're about to click and why when an action is spendy,
  destructive, publishes, sends messages/mail, or touches account settings.
- The owner watches GUI actions live; keep windows you activate obvious and
  close what you open. `activate:false` exists so pure observation never
  disturbs his focus.
- Never automate a purchase, upload, or send without the owner's explicit
  ask in the conversation.

## Known limits

- Some apps expose no AX tree (or a huge one) — use `depth` to bound
  (0–12, 350 elements), `depth=0` is screenshot-only; fall back to pure
  screenshot reasoning.
- `select_text` keyboard fallback caps at 400 characters; without Screen
  Recording you get a full-screen capture with approximate coordinates.
- Coordinate actions re-anchor from live window bounds, but trust a fresh
  screenshot for anything delicate.
