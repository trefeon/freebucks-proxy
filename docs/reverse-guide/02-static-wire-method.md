# 02 — Static-Wire Method

> Guide: [README](README.md) · [01 scope](01-scope-evidence.md) · **02 static-wire** · [03 capture](03-dynamic-capture.md) · [04 JS](04-js-bundle.md) · [05 binary](05-binary-analysis.md)

Vendor clone is the source of truth. Everything else derives from pins.

## Vendor clone

- Clone lives at `upstream/freebuff` (gitignored), overridable via
  `$FREEBUFF_REFERENCE_DIR`. Missing clone → shallow-cloned `--depth 50`;
  present → fetched. Never commit it.
- Compare against `origin/main` by default; always fetch before classifying.
  Full-SHA refs additionally gate on the snapshots manifest pinning that
  same SHA (`scripts/check-upstream.sh` exits 2 otherwise).

## Verbatim pins

- Wire: `backend/internal/wirefacts/testdata/wire/snapshots.json` pins
  `upstream_sha` + 13 files with per-file sha256. Bytes are verbatim —
  never transformed, so drift review reads the same bytes.
- Registry mirror: `backend/internal/registry/testdata/upstream/` holds
  6 files (`free-agents.ts`, `freebuff-model-ids.ts`, `freebuff-models.ts`,
  `gemini.ts`, `model-config.ts`, `freebuff-model-entitlements.ts`).
  Keep in sync with `RegistryFiles` (`backend/internal/wirefacts/wirefacts.go:34`)
  and `REGISTRY_FILES` in `scripts/check-upstream.sh`.
- Per-commit content hashes of the 13 watched wire files:
  `scripts/wire-baseline.tsv`.
- `scripts/vendor-version.txt` pins the npm wrapper version (informational;
  never flips a check exit code). `snapshots.json` also stamps
  `llm_providers_version` and `bun_version` at re-pin time.

## Codegen derivation

- `go generate ./backend/internal/wirefacts/` runs
  `backend/cmd/wiregen -upstream <sha>` (`backend/internal/wirefacts/wirefacts.go:9`).
- Emitters: `backend/internal/wirefacts/emit_wire.go` (known-status subset,
  notices table), `emit_catalog.go` (catalog), `emit_tools.go` (toolmap)
  → `backend/internal/wirefacts/wirefacts_gen.go` (generated, never hand-edit).

## Drift taxonomy — which script when

| Script | Answers | Labels |
|---|---|---|
| `scripts/check-upstream.sh` | *Is there drift?* Per-file SAME/DRIFT/MISSING for registry+wire; `--version-only` probes the version gate; `--group` scopes one group | SAME / DRIFT / MISSING |
| `scripts/drift-exact.sh` | *What changed at export level?* Old ref (default: pinned `upstream_sha`) vs new ref (default: `origin/main`) | SAME / COMMENT_ONLY / NOTICE_ONLY / FUNCTIONAL + MODEL / PRICE labels; `untracked[]` for new files outside the watched lists |
| `scripts/review-wire-drift.sh` | *Older per-file classifier* (baseline-TSV based); notice = 5 `FREEBUFF_*_NOTICE` strings | SAME / COMMENT-ONLY / NOTICE / FUNCTIONAL / UNKNOWN-BASELINE |
| `scripts/drift-impact.sh` | *Who consumes it?* Grep-maps FUNCTIONAL exports to repo consumers | MODEL / PRICE / WIRE / OTHER / NOTICE / UNTRACKED × direct / name-match / unresolved |

- COMMENT_ONLY → refresh baseline, no port. NOTICE_ONLY → snapshots
  re-pin + wiregen regen, no port. FUNCTIONAL → port the Go side first.
- MODEL vs PRICE labels keep bot announcements precise ("price change
  only" ≠ "models drifted"). `drift-tui.sh` is the interactive viewer
  over the same data.

## Serial re-pin chain

One entry point: `scripts/repin-all.sh [--dry-run] <vendor-sha> [clone-dir]`
(steps: classify → refresh 13 snapshots + stamp sha/versions →
update `wirefacts_test` + `go:generate` line → wiregen → verify via
`check-upstream.sh` + hermetic wirefacts +
`TestFallbackParityWithPinnedUpstream` → print 3 PR commands).

Order is wire PR → registry PR (`scripts/sync-upstream.sh`) → dashboard
embed PR (`DRIFT_REPORT=... check-upstream.sh`), **one green merge before
the next opens**. Registry sync verifies `--group registry` so concurrent
wire drift can't fail it. Green-CI-between is the rule, not a suggestion.

## IRON RULES

1. **Classify BEFORE refresh.** Refreshing baselines first stamps
   all-SAME and hides FUNCTIONAL rows. `repin-all.sh` aborts on
   FUNCTIONAL/UNKNOWN-BASELINE before touching anything.
2. **CRLF-normalize.** Refresh writes LF-normalized bytes; readers
   CR-strip every line — byte-clean under Windows shells and CRLF
   checkouts alike.

## Sources

- `devdocs/re-kit/CLI-FLOW.md`, `devdocs/re-kit/PORT-MAP.md`
- `scripts/repin-all.sh`, `scripts/drift-exact.sh`, `scripts/check-upstream.sh`
- `backend/internal/wirefacts/wirefacts.go`
