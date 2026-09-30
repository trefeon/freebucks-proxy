# Unified runtime store: mem-authoritative, DB-persisted, .env seed-only

Status: accepted · 2026-09-25 · user order; finishes the half-done
env-to-DB migration (`settings_migrate.go`, `data-architecture.md`).

## Context

2026-09-25 pool-only outage: the dashboard wrote `API_KEYS` to the `.env`
file while a stale DB overlay row shadowed it (precedence
`defaults < JSON < .env < DB overlay < process env`,
`settings_overlay.go:1-17`). All client keys 401'd; reload fan-out +
`changed_keys` never surfaced it. Two runtime sources = split-brain class.
Full surface map: StateMapper sweep 2026-09-25 (session transcript) —
settings overlay, `migrated_env_v1` marker, `sessions_persist`, legacy
session JSON, `tokens` maturity, `pool_state` (ledger/admissions/cooldown
hint), `pages_state`, `quota_snapshots`, `maturity_events`,
`request_records`, `log_entries`, `.env` file, JSON `-config`, env-only
blocked keys, DB lifecycle.

## Decision

1. **One runtime source per plane**: an in-memory snapshot, loaded once at
   boot. Reads serve from mem; the hot request path never blocks on disk.
2. **`.env` (and JSON `-config`) are boot seed only**: consulted when the DB
   is empty / first boot (existing one-shot `migrateEnvToDB`, marker-gated),
   never re-read, never written by the dashboard. Direct `.env` edits
   require restart — documented, and `SettingSources` must report the file
   tier as seed-only.
3. **Mutations apply synchronously**: every dashboard mutation writes the
   overlay (or plane store) AND swaps the live snapshot in the same handler
   (extend `applyReloadedConfig`, `admin.go:294` — as a sync applier, never
   a reloader). No `loadConfig`-per-mutation, no reload fan-out, no
   `changed_keys` diffing. The handler returns the applied receipt.
4. **One persist pattern**: mem-swap first (instant, in-request), WAL write
   behind via the recorded spill pattern (`spillCh 1024, batch 100/1s`,
   drop counter — `dashboard_history.go:19-67`). No sync disk on any
   request path. Existing mem-only rules STAND (slot counters, cooldowns,
   single-flight, `ip_capped`, `UsageRecord` ring — parked-goroutine /
   derive physics unchanged); the redesign unifies the persist side, not
   what persists.
5. **Delete the shadow class**: the `.env` write leg of dual-write
   (`envfile.go` via `admin_env.go`), `overlayShadows`, per-mutation
   reloads — all removed. The `.env` editor becomes export-only
   (break-glass copy) or is removed (Lane D decides; export-only default).
6. **Boot order** (unchanged shape): open DB → legacy carry (existing
   empty-gates) → seed from `.env` only if settings empty → snapshot to
   mem → serve. Corrupt/missing → live-only degrade (unchanged). Live DBs
   carry forward untouched; no schema migration required unless a lane
   proves it needs one (new `goose` file, empty-gate, idempotent).
7. **Secrets**: overlay stays the home (`0600` preserved); raw values never
   in logs/rings (existing `sha256[:16]` rule).

## Behavioral invariants (every lane proves all four)

- I1: a mutation is visible to the next read synchronously (test).
- I2: no disk I/O on the request path (test via write-call counting, not timing).
- I3: restart recovers (existing carry/restore tests stay green).
- I4: `.env` is never written and never re-read after boot (test: mutation
  leaves `.env` bytes identical; reads work with `.env` removed post-boot).

## Consequences

- `POST /admin/api/settings` + dedicated endpoints are the only writers;
  the settings UI keeps showing source tiers (db vs file-seed vs env).
- `docs/decisions/data-architecture.md` knob-chain section becomes
  historical on the `.env` runtime tier; this ADR governs.
- Follow-up hardening (not this program): code fix syncing dashboard
  key-saves to the overlay is subsumed — the `.env` write leg is gone.

## Lanes (disjoint ownership; behavior contracts, no shared new API)

- A config plane: `config/*`, `server/admin_settings.go`,
  `server/admin_env.go`, `server/admin_dualwrite.go`, `server/admin.go`
  (applier only).
- B runtime plane: `pool/pool_persist.go`, `session/store.go`,
  `store/sessions.go`, `store/pool_persist.go`.
- C history plane: `store/history*.go`, `dashboard/dashboard_history.go`,
  `dashboard/dashboard_logs.go`, `session/quota_seed.go`.
- D dashboard+API: `frontend/src/**`, `server/admin_pages.go`, e2e,
  openapi regen. No `dist` commit (integrator rebuilds once).
- Shared `store/store.go` (spill core/DNS): Lane A owns; other lanes follow
  the recorded spill pattern inside their plane and MUST NOT touch it.

## Amendment 2026-09-30: pool ledger flush cadence + crash window

Lane B follow-up (post-roll zeros: `Messages24h`/`Requests`/`SpendDay` all
zero after restart). Root cause: the write path worked (dirty flag → spill
→ `pool_state` rows) but `Pool.Start` never called `RestorePoolPersist` —
three comments claimed it did, hiding the bug. Fixed: `Start` restores
first, then starts the spill, so the first pass can only persist restored
state.

- Flush cadence: dirty-gated only. Coalesced wakeup per mutation batch plus
  a 1s spill tick (`poolSpillFlushEvery`), the maintain tick, and a
  best-effort `Shutdown` flush. No new tables: the per-token blob
  (`pool/ledger/<sha256>`) gained one additive field, `requests_total`
  (runs mint counter = healthz `Requests`); pre-persist rows unmarshal it
  as 0 and the restore skips them (max-guarded, never drags a live count
  back). Migration `00004_v4_pool_state` already covers pre-table DBs.
- Crash window: an unclean kill (SIGKILL, power loss) loses at most ~1s of
  ledger deltas (the dirty-gated interval); a clean `Shutdown` flushes.
  Documented, not plumbed further: no new graceful-shutdown machinery was
  added. Restore revalidates on the way in (out-of-window usage dropped,
  stale spend buckets rolled, hints expiry-checked), so a restart never
  resurrects expired windows.
- Deliberately still volatile (no complaint possible): slot/live-turn
  counters, cooldowns, single-flight, `ip_capped`/burst windows, upstream
  client counters (`TransientRetries`/`FingerprintRotations`/
  `RateLimitEvents`), pool `requestsServed`, per-entry served history, and
  the dashboard usage ring (in-memory by design, no table).
