# docs/ v4 refresh TODO

`docs/` was salvaged verbatim from the v3-era publish bundle
(`liminal-void/build/zcode-computer-use-publish/`) on 2026-08-31. It describes
the **Python server**; v4.0.0 is the Go `cuu` binary. Refresh before enabling
GitHub Pages.

Stale, known:

- `index.html` og:description + hero: "One stdlib-only Python file" → single
  static Go binary (`cuu`), MCP stdio + CLI verbs.
- `index.html` install block: MCP config shows `python3` command +
  `PYTHONUNBUFFERED` → `${ZCODE_PLUGIN_ROOT}/cuu/bin/cuu serve` (or absolute
  path manual fallback, as in README).
- `index.html` permissions section: `python3 mcp/server.py selftest` →
  `cuu/bin/cuu selftest`.
- `index.html` "No pip anywhere" / `/usr/bin/python3` copy — partially still
  true (scroll only); reword so it doesn't read as "the server is Python".
- `llms.txt` requirements line: "Python 3.9+" → Go 1.25+ to build, no Python
  runtime except Apple's bundled `/usr/bin/python3` for scroll.
- `demo/` GIFs are real output but show the v3 Python test runs; regenerate
  with `cinta` against the v4 CLI verbs when publishing (HTML sources are
  alongside the GIFs and are regenerable by design).
- Optional per the original checklist: 20s screen recording of the live
  TextEdit E2E (`tests/_legacy/e2e_textedit.py` is v3; a v4 equivalent would
  drive `cuu` CLI verbs).

Not stale: the two deep-dive traps (HID-tap wheel events silently dropped →
session tap + pixel units; TextEdit ignoring scripted AXSelectedTextRange
writes → content-verified selection with keyboard fallback) are unchanged in
v4 and remain the best teaching content on the page.
