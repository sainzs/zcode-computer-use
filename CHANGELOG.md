# Changelog

All notable changes to `zcode-computer-use` are documented here.

## 3.0.1 — 2026-08-28

- E2E suite extended to 31 checks: `drag` now verified by moving a real window
  (+120,+40 pt via its title bar, asserted through CGWindowList),
  `perform_action` drives a live AXShowMenu discovered via `element_info`, and
  `press_key` makes a deterministic edit. Coverage table on the site updated —
  drag/press_key/perform_action bars go to 100% with receipts.
- Number consistency pass across README, site, banner, and manifests.

## 3.0.0 — 2026-08-27

First release packaged for distribution (plugin manifest, MIT license,
tracked in git, protocol test suite).

### Added

- `list_windows` — enumerate an app's on-screen windows (id, title, bounds,
  frontmost flag).
- Per-window targeting: `get_app_state` accepts `window` (id or title
  substring) instead of always grabbing the frontmost window; the AX tree,
  screenshot, and element addresses all bind to the chosen window.
- Background observe: `get_app_state(activate:false)` captures and reads a
  window without stealing focus. An explicit window raise happens only when
  activation is requested and the target window is not frontmost (AXRaise).
- Tree diffing: element lines carry `+`/`~` markers against the previous
  capture of the same window, keyed by tree chain (survives index shifts),
  with a new/changed/gone summary in the state header.
- `select_text` — select a character range with content-verified results:
  the AXSelectedText readback must equal the requested slice of the element
  value, or the tool falls back to a keyboard walk (cmd+up, shift+right,
  capped at 400 chars) and reports which method won. (TextEdit ignores
  scripted AXSelectedTextRange writes; the fallback covers it.)
- Horizontal scroll (`left`/`right`) and a rebuilt scroll engine: wheel
  events post to `kCGSessionEventTap` in pixel units via Apple's bundled
  python3/Quartz. The v2 JXA route posted to `kCGHIDEventTap`, where wheel
  events are silently dropped — vertical scroll had never actually worked.
- `type_text` gains `method:"keys"` (synthesized keystrokes, clipboard
  untouched, capped at 2000 chars) alongside the default clipboard paste.
- Structured tool errors: every failure returns JSON
  `{error:{code,message,remedy}}` (codes like `stale_state`,
  `permission_accessibility`, `window_gone`) so callers can branch instead
  of parsing prose.
- MCP protocol negotiation: supports 2025-03-26 and 2025-06-18, echoes the
  client's version when supported; handles `logging/setLevel`.
- Permissions-aware `selftest`: a real AX query names the missing
  Accessibility grant; Quartz scroll and JXA window probes included;
  JSONL structured log.
- Test suites: `tests/test_protocol.py` (15 headless-safe golden protocol
  tests) and `tests/e2e_textedit.py` (22-check live drive of TextEdit).

### Changed

- `get_app_state` no longer silently launches apps: a non-running app is an
  `app_not_found` error unless `launch:true` is passed.
- `perform_secondary_action` is now `perform_action` (old name still
  dispatches as an alias).
- Screenshot scale is derived per capture as before; coordinate re-anchoring
  still binds to the captured CGWindowID.

## 0.2.0 — 2026-08-14

Hardening wave after a three-critic review: `element_info` (role, value,
enabled/focused, frame, AX action names), `perform_secondary_action`,
stale-state re-anchor by captured CGWindowID, `\x1f` field joins against
tree forgery, symlink-safe screenshot publication (mkstemp + os.replace,
0600/0700), clipboard save/restore with non-text detection, JSON-RPC
-32700/-32600 handling, BrokenPipe and crash guards.

## 0.1.0 — 2026-08-14

Initial internal release: screenshot + indexed AX tree, element-first
AXPress with screenshot-pixel coordinate fallback (cliclick), forced
re-fetch discipline, clipboard-paste typing, key chords, vertical scroll,
drag, set_value.
