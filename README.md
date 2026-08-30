# zcode-computer-use

macOS computer control for ZCode — a single static Go binary that gives an
agent eyes and hands on native apps: per-window screenshots, an indexed
accessibility tree with diff markers, element-first actions with
screenshot-pixel fallback, background observe, verified text selection, and
four-way scrolling.

One binary, two surfaces, one state file:

- **MCP over stdio** (`cuu serve`) — what ZCode spawns; the full tool schema
  lives in the manifest.
- **CLI verbs** (`cuu click --element_index 3`, `cuu get_app_state --app
  TextEdit`, …) — one JSON document on stdout per verb, exit `0` ok / `1`
  structured tool error / `2` usage. Verbs and MCP calls share the on-disk
  `state.json`, so you can mix shell calls with a live MCP session.

The v3 Python server (`mcp/_legacy/server.py`) is kept for provenance; the
Go port reproduces its protocol byte-for-byte, including the golden tests
(`tests/_legacy/test_protocol.py` is the original suite the Go tests mirror).

## Why this over a built-in

| | zcode-computer-use | typical built-in |
|---|---|---|
| Install surface | one static Go binary, zero runtime deps | bundled, opaque |
| Tree hygiene | forced re-fetch, anti-forgery address chains, diff markers | varies |
| Errors | structured `{code, message, remedy}` JSON | prose |
| Focus | background observe (`activate:false`) never steals focus | usually activates |
| Launching | never auto-launches without `launch:true` | often does |
| Scroll | verified 4-way wheel events (session-tap + pixel units) | often vertical-only, unverified |
| Text selection | content-verified with keyboard fallback | rare |
| Auditability | JSONL log, permission-aware selftest, golden protocol tests | — |

## Install

Build once (the manifest points at the built binary, which is not tracked
in git):

```bash
cd plugins/zcode-computer-use/cuu
go build -o bin/cuu .
```

From a marketplace that lists this plugin (the author's `zcode-local`
marketplace does), install `zcode-computer-use` and restart ZCode. The
manifest registers the MCP server as `${ZCODE_PLUGIN_ROOT}/cuu/bin/cuu
serve`.

Manual fallback (any ZCode build): add to your config's `mcpServers`:

```json
{
  "mcpServers": {
    "zcode-computer-use": {
      "command": "/absolute/path/to/plugins/zcode-computer-use/cuu/bin/cuu",
      "args": ["serve"]
    }
  }
}
```

Requirements: macOS, Go 1.25+ to build, `cliclick` (`brew install
cliclick`) for coordinate mouse events, and Apple's bundled
`/usr/bin/python3` (Xcode Command Line Tools — ships with pyobjc) for
scroll events only. No Python at runtime otherwise.

## Permissions (the one hurdle)

The **host app** (ZCode, or your terminal if you run the binary by hand)
needs two grants, then a restart:

- **Accessibility** — System Settings → Privacy & Security → Accessibility
- **Screen Recording** — System Settings → Privacy & Security → Screen Recording

Preflight, which names the exact missing grant:

```bash
plugins/zcode-computer-use/cuu/bin/cuu selftest
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

## The CLI

`cuu help` lists every verb with its flags. Two conventions:

- Flags map 1:1 onto the MCP tool arguments (`--element_index`,
  `--activate=false`), so the tool schema stays the single source of
  truth; only explicitly-passed flags are sent.
- Every verb prints one JSON document — the same payload the MCP surface
  returns — so shell pipelines and MCP clients see identical shapes.

```bash
cuu get_app_state --app TextEdit | jq .element_count
cuu click --element_index 4
cuu press_key --key cmd+s
```

## Safety design

- Element addresses are rebuilt server-side from regex-validated index
  chains with a freshly escaped process name — AX text can never forge an
  address, and index maps can never outlive the tree they came from.
- Failed captures never leave live-looking state; coordinate actions
  re-anchor by the captured CGWindowID, never by app name.
- Screenshots are published atomically with `0600` files in a `0700`
  directory; the clipboard is saved/restored around pastes with non-text
  detection.
- No silent launches, no silent focus steals, every error carries a remedy.

## Verification

```bash
cd plugins/zcode-computer-use/cuu && go test ./...   # golden protocol tests
cuu/bin/cuu selftest                                 # deps + permissions preflight
python3 tests/_legacy/test_protocol.py               # original v3 suite, against the v3 server
python3 tests/_legacy/e2e_textedit.py                # live v3 drive of TextEdit
```

The Go suite speaks raw JSON-RPC to a spawned `cuu serve` — the same 15
golden cases as the Python suite (RPC error taxonomy, strict argument
validation, alias suppression), plus pure-function goldens for the tree
diff and AppleScript escaping. The two live-GUI cases self-skip when
System Events is unreachable.

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
