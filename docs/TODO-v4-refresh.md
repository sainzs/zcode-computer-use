# docs/ v4 refresh TODO

`docs/` describes the published v3 site (Python era). Refresh status below.

## Done 2026-08-31

- `index.html`: meta/og descriptions, hero sub, meta rail (`static binary 1`,
  `tests green 27 + 21`), stat row (27 go golden tests / 21 legacy protocol /
  0 pip / 3,270px scroll), v3-trace provenance labels (ribbon + figcap +
  JS comment point at `tests/_legacy/`), coverage micro-note (drops the stale
  "drag not re-driven" caveat — 3.0.1 drove it), install block
  (`cuu/bin/cuu serve` + build line + `selftest`), why rows (static Go binary,
  ~3,400 lines, test counts), refuses ("one static binary"), FAQ ("one
  auditable binary"). Scroll/pyobjc copy kept — still true in v4.
- `llms.txt`: rewritten for v4 (build with Go 1.25+, `go test`, cuu paths).
- Verified: HTML parses; rendered screenshots at 1440 + 390 measured clean
  (page-level horizontal overflow = 0px; tools table and install block scroll
  internally at ≤860px by design, per the v3 QA round).

## Remaining

- `demo/` GIFs are v3-era recordings (they show the Python suites). Regenerate
  with [cinta](https://github.com/sainzs/cinta) against v4 (`go test ./...`,
  `cuu selftest`, CLI verbs); the `.html` sources next to the GIFs are the
  regenerable scripts.
- Optional: 20s screen recording of a live v4 TextEdit drive (cinta can't
  produce real GUI movement).
- Re-verify demo image bytes before pushing if any server rerun overwrote the
  state PNGs (known trap from the v3 publish).
- Publish gate: site goes live when the owner pushes v4 to
  `sainzs/zcode-computer-use` (force-push `main` + push `archive/v3-public`;
  needs `gh auth login`). Only then do these pages serve.
