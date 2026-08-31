# zcode-computer-use

macOS computer control for ZCode (or any MCP client) — a single static Go
binary that gives an agent eyes and hands on native apps: per-window
screenshots, an indexed accessibility tree with diff markers, element-first
actions with screenshot-pixel fallback, menu-bar driving, condition waiting,
OCR fallback via Apple Vision, window management, explicit clipboard access,
background observe, verified text selection, and four-way scrolling.

The design metric is **agent turns and tokens per completed task**: trees
carry diff markers so one re-observe tells the agent what changed, `filter`
and `find` keep 350-element trees out of the context window, `wait_for`
replaces observe-poll loops with one call, and `include_screenshot` returns
the capture as MCP image content so image-rendering clients skip a file-read
round trip.

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
| Menu bar | list + click by path (`"File > Export as PDF…"`) | usually unreachable |
| Waiting | `wait_for` text present/gone, structured timeout | re-observe loops |
| No AX tree | `ocr` — Vision text regions in click-ready screenshot pixels | stuck |
| Text selection | content-verified with keyboard fallback | rare |
| Auditability | JSONL log, permission-aware selftest, golden protocol tests | — |

## Install

Prebuilt: every tagged release ships a universal macOS binary — download
`cuu-vX.Y.Z-macos-universal.tar.gz` from the releases page, unpack, and
point `mcpServers` at it (no Go toolchain needed).

From source (the plugin manifest points at the built binary, which is not
tracked in git):

```bash
cd cuu
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
      "command": "/absolute/path/to/zcode-computer-use/cuu/bin/cuu",
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
cuu/bin/cuu selftest
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

Four tools cut turns and tokens around that loop:

- `menu(app)` lists the menu bar (where most macOS functionality lives,
  invisible to window captures); `menu(app, path: "File > Export as
  PDF…")` clicks an item — deeper submenus chain with more `" > "`, and a
  missing item is a structured `menu_not_found`.
- `wait_for(text, until: present|gone, timeout_s)` polls the captured
  window's tree until the condition holds — one call instead of an
  observe/not-ready/observe loop; times out as `wait_timeout`.
- `get_app_state(filter: "Save")` shows only matching tree lines while the
  full index registry stays targetable; `find(text?, role?)` re-queries the
  current capture with a different lens without touching the GUI.
- `get_app_state(include_screenshot: true)` attaches the capture to the MCP
  response as image content (downscaled to ≤1568px) — clients that render
  images skip the separate file read.

And three capability tools round out the surface:

- `ocr(filter?)` reads text out of the current screenshot via Apple Vision
  (same osascript stack, no new dependencies) — the fallback when an app
  exposes no usable AX tree. Boxes come back in screenshot pixels, so
  `click x/y` works on them directly.
- `window(app, action, …)` moves/resizes/minimizes/unminimizes/zooms/closes
  a window in screen points; an ambiguous window address is a structured
  refusal, never a silent fallback.
- `clipboard(text?)` reads or sets the pasteboard explicitly (`""` clears);
  non-text content is reported honestly instead of as an empty string.

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
cd cuu && go test ./...   # golden protocol tests
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
- `select_text` keyboard fallback caps at 400 characters; `wait_for` polls
  text in the AX tree, not pixels.
- Without Screen Recording you get a full-screen capture where
  multi-display coordinates are approximate.
- Scroll needs Apple's `/usr/bin/python3` (pyobjc); Homebrew-only setups
  get a structured `unsupported` error.

## License

MIT — see [LICENSE](LICENSE).
