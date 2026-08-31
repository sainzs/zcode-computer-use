# Changelog

All notable changes to `zcode-computer-use` are documented here.

## 4.1.0 — 2026-08-31

The turn-economy release: the metric behind every change is agent turns +
tokens per completed GUI task. All additions are strictly additive — the
v4.0 tool surface, payload shapes, and golden protocol behavior are
unchanged.

### Added

- `menu` — read or drive the app's menu bar, where most macOS
  functionality lives and which window captures cannot see. Without
  `path`: list every menu with its items (separators skipped). With
  `path` (`"File > Export as PDF…"`, deeper submenus chain with more
  `" > "`): click that item. Menu titles pass through the same
  AppleScript-escaping discipline as element addresses; a missing item is
  a structured `menu_not_found` with the listing as its remedy.
- `wait_for` — poll the captured window's AX tree until a text is
  `present` (default) or `gone`, then return; a structured `wait_timeout`
  otherwise. One call replaces the act → observe → "not ready yet" →
  observe loop. Marks state stale on return: the tree it polled is not
  the tree the indices came from.
- `find` — re-query the current capture's registry by text substring
  and/or exact AX role without touching the GUI; returns indexed lines
  whose indices feed element actions directly.
- `get_app_state` gains `filter` (case-insensitive substring; only
  matching tree lines are shown while the full registry stays targetable)
  and `include_screenshot` (attach the capture to the MCP response as an
  image content block, downscaled to ≤1568px — image-rendering MCP
  clients skip the file-read round trip; the CLI accepts and validates
  both flags, inlining applies to the MCP surface).
- CI on GitHub Actions (macOS runner: build, vet, golden suite) and a
  tag-triggered release workflow that ships a prebuilt universal binary —
  installing no longer requires a Go toolchain. The release job refuses a
  tag that disagrees with `serverVersion`.
- The Go test fixture is hermetic: spawned servers and CLI probes get an
  isolated `ZCODE_CUA_DATA`, so a developer's real `state.json` can never
  flip a `no_state` assertion.

## 4.0.0 — 2026-08-30

The Python server is ported to Go: one static binary (`cuu`) with two
surfaces — the stdio MCP server (`cuu serve`) and a CLI with one
JSON-printing verb per tool. The protocol, tool schemas, error taxonomy,
and state format are unchanged; the golden Python tests still pass
against the protocol (now mirrored as `go test` cases that speak raw
JSON-RPC to a spawned `cuu serve`).

### Added

- `cuu` CLI verbs for every tool (`cuu get_app_state --app TextEdit`,
  `cuu click --element_index 4`, …): one JSON document on stdout, exit
  `0` ok / `1` structured tool error / `2` usage. Flags map 1:1 onto MCP
  tool arguments; only explicitly-passed flags are sent.
- CLI and MCP share the capture/action state on disk (`state.json` in the
  data dir), so shell calls and a live MCP session see the same stale-state
  and diff picture.
- `go test ./...` suite: the v3 golden protocol cases re-expressed against
  the binary (RPC error taxonomy, strict argument validation,
  `perform_secondary_action` alias suppression), pure-function goldens for
  tree parsing/diff and AppleScript escaping, and live-GUI cases that
  self-skip headless.

### Changed

- The server is a compiled binary; the Python runtime is gone (scroll
  still shells to Apple's bundled `/usr/bin/python3` for pyobjc Quartz
  wheel events, as in v3).
- `mcp/server.py` moved to `mcp/_legacy/server.py` and its suites to
  `tests/_legacy/` (paths updated; both still run green against the v3
  file for provenance). `cuu/bin/` is gitignored — build with
  `go build -o bin/cuu .`.

### Fixed

- The v3 test fixture could exit before the server flushed its final
  response; the serve loop now flushes per line and once more on exit, so
  the last reply survives a client that closes its read end first.
- A window whose System Events index could not be matched fell back to
  `window 1` silently; the state header now records the mismatch.
- The activation raise fronted the *previous* state's app (read from
  stale state, empty on a first capture), so it silently no-opped exactly
  when it was needed; it now fronts the app being captured. The window
  list is also re-checked before the raise instead of indexing `[0]`
  unguarded.

## 3.0.1 — 2026-08-29

### Fixed

- The 3.0.1 manifest bump never reached the server: `SERVER_VERSION`, the
  `--version` output, and the protocol tests all still reported 3.0.0, and
  this changelog had no 3.0.1 entry. All surfaces now agree on 3.0.1.

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
