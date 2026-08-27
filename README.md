# zcode-computer-use

macOS computer control for ZCode — a single-file, stdlib-only MCP server
that gives an agent eyes and hands on native apps: per-window screenshots,
an indexed accessibility tree with diff markers, element-first actions with
screenshot-pixel fallback, background observe, verified text selection, and
four-way scrolling.

Everything ships in one readable Python file (`mcp/server.py`, ~1300 lines,
zero pip dependencies) with its protocol tests and a live end-to-end suite.
If you want to learn how computer control actually works under the hood —
or hack on your own driver — this is the codebase to read.

## Why this over a built-in

| | zcode-computer-use | typical built-in |
|---|---|---|
| Install surface | one Python file, stdlib only | bundled, opaque |
| Tree hygiene | forced re-fetch, anti-forgery address chains, diff markers | varies |
| Errors | structured `{code, message, remedy}` JSON | prose |
| Focus | background observe (`activate:false`) never steals focus | usually activates |
| Launching | never auto-launches without `launch:true` | often does |
| Scroll | verified 4-way wheel events (session-tap + pixel units) | often vertical-only, unverified |
| Text selection | content-verified with keyboard fallback | rare |
| Auditability | JSONL log, permission-aware selftest, golden protocol tests | — |

## Install

From a marketplace that lists this plugin (the author's `zcode-local`
marketplace does), install `zcode-computer-use` and restart ZCode. The
manifest registers the MCP server as
`python3 ${ZCODE_PLUGIN_ROOT}/mcp/server.py`.

Manual fallback (any ZCode build): add to your config's `mcpServers`:

```json
{
  "mcpServers": {
    "zcode-computer-use": {
      "command": "python3",
      "args": ["/absolute/path/to/plugins/zcode-computer-use/mcp/server.py"],
      "env": {"PYTHONUNBUFFERED": "1"}
    }
  }
}
```

Requirements: macOS, Python 3 with the standard library, `cliclick`
(`brew install cliclick`) for coordinate mouse events, and Apple's bundled
`/usr/bin/python3` (Xcode Command Line Tools — ships with pyobjc) for
scroll events.

## Permissions (the one hurdle)

The **host app** (ZCode, or your terminal if you run the server by hand)
needs two grants, then a restart:

- **Accessibility** — System Settings → Privacy & Security → Accessibility
- **Screen Recording** — System Settings → Privacy & Security → Screen Recording

Preflight, which names the exact missing grant:

```bash
python3 plugins/zcode-computer-use/mcp/server.py selftest
```

## The operating loop

1. `list_apps` if you don't know the app name; `list_windows` to pick a
   window.
2. `get_app_state(app, window?, depth?, activate?)` — returns a screenshot
   PNG path, the pixel scale, and an indexed AX tree whose lines carry diff
   markers (`[+n]` new, `[~n]` changed) against the previous capture of the
   same window. Read the PNG. Observe once; don't re-call without acting.
3. Prefer element targets (`click element_index`, `set_value`,
   `perform_action` — discover names with `element_info`). Fall back to
   `x`,`y` in **screenshot pixels** (the server rescales via the capture's
   backing factor and re-anchors to the captured window after actions).
4. After every action the state goes stale: `get_app_state` again before
   the next element action. The diff markers tell you exactly what your
   action changed.

`select_text` returns the selected text plus the method that worked
(`ax_range` or `keyboard`) — the selection is verified against element
content, not trusted. `type_text` defaults to a clipboard paste (saved and
restored); `method:"keys"` never touches the clipboard.

## Safety design

- Element addresses are rebuilt server-side from regex-validated index
  chains with a freshly escaped process name — AX text can never forge an
  address, and index maps can never outlive the tree they came from.
- Failed captures never leave live-looking state; coordinate actions
  re-anchor by the captured CGWindowID, never by app name.
- Screenshots are published atomically (mkstemp + rename) with `0600` files
  in a `0700` directory; the clipboard is saved/restored around pastes with
  non-text detection.
- No silent launches, no silent focus steals, every error carries a remedy.

## Verification

```bash
python3 tests/test_protocol.py    # 15 golden protocol tests, headless-safe
python3 tests/e2e_textedit.py     # 22-check live drive of TextEdit
python3 mcp/server.py selftest    # deps + permissions preflight
```

The scroll engine and selection fallback are verified against real apps
(Safari, TextEdit) — see CHANGELOG 3.0.0 for what was measured.

## Known limits

- macOS only (System Events / CGWindowList / cliclick stack).
- The frontmost window is the default target; apps exposing huge AX trees
  are bounded by `depth` (0–12) and a 350-element cap.
- `select_text` keyboard fallback caps at 400 characters.
- Without Screen Recording you get a full-screen capture where
  multi-display coordinates are approximate.
- Scroll needs Apple's `/usr/bin/python3` (pyobjc); Homebrew-only setups
  get a structured `unsupported` error.

## License

MIT — see [LICENSE](LICENSE).
