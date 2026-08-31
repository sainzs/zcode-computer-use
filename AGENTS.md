# zcode-computer-use

macOS computer control for ZCode: one static Go binary (`cuu`) — MCP-over-stdio
server (`cuu serve`) plus a CLI with one JSON-printing verb per tool. Per-window
screenshots, indexed AX tree with diff markers, element-first actions with
screenshot-pixel fallback, background observe, verified text selection, 4-way
scroll. Extracted from `liminal-void/plugins/zcode-computer-use` on 2026-08-31
(history preserved via `git subtree split`); the v3 Python server stays under
`mcp/_legacy/` with its golden suites in `tests/_legacy/` for provenance.

## Gotchas

- `cuu/bin/` is gitignored — after a fresh clone, build before ZCode can load
  the plugin (the manifest points at `${ZCODE_PLUGIN_ROOT}/cuu/bin/cuu serve`).
- macOS only. Runtime deps: `cliclick` (mouse events) and Apple's bundled
  `/usr/bin/python3` (pyobjc, scroll only). Grants (Accessibility + Screen
  Recording) live on the HOST app — ZCode or the terminal running the binary.
- CLI and MCP share `state.json` in the data dir (`~/.cache/zcode-computer-use`);
  the server log is `server.log` there (JSONL, event-greppable).
- Scroll trap: JXA `CGEventPost(kCGHIDEventTap)` wheel events are silently
  dropped by macOS — scroll posts pixel-unit events to `kCGSessionEventTap` via
  `/usr/bin/python3` in ≤120px chunks; axis2 negative = right. Don't "simplify"
  this; it is the verified recipe.

## Verify

```bash
cd cuu && go build -o bin/cuu . && go test ./...   # golden protocol suite
cuu/bin/cuu selftest                                # deps + permission preflight
python3 tests/_legacy/test_protocol.py              # v3 provenance suite
python3 tests/_legacy/e2e_textedit.py               # live TextEdit drive (GUI)
```

The Go suite speaks raw JSON-RPC to a spawned `cuu serve`; live-GUI cases
self-skip headless. Last full-green rehearsal: 2026-08-31 (go test ok, 21/21
protocol, selftest 6/6).
