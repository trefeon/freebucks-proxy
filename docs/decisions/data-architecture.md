# Data architecture: DB vs env vs JSON vs log vs mem

Status: proposed · 2026-09-18 · supersedes hallway discussion on
"why is so much temp, why can't we persist like 9router".

## Context

`reference/routers/9router` was researched as the durability comparison
(`src/lib/db/*`, `src/sse/services/auth.js`, `open-sse/services/combo.js`,
`ARCHITECTURE.md` — note: that doc is stale, still claims `db.json`).
Finding: 9router persists CONFIG + append-only history in SQLite and keeps
hot runtime ephemeral, exactly like us. Its one real divergence is
persisting timestamped cooldown locks (`modelLock_*` in the connection row);
ours keeps cooldowns memory-only by design (MASQ re-probes, cold start is
cheap and correct).

## Our DB stack

Pure-Go SQLite (`modernc.org/sqlite`, no cgo) + `pressly/goose 00001..00005`
(`backend/internal/store/store.go:sqliteDSN`):
`busy_timeout 5000, journal_mode WAL, synchronous NORMAL, _txlock immediate`
on every connection, single-writer `writeMu`, file `DB_PATH` else
`./data/freebuff.db` (`0600`), live store `/app/data/freebuff.db` on the
`db_data` volume. Corrupt/missing file degrades to live-only, never fails boot.

Knob chain (`backend/internal/config/settings_overlay.go`, `config_load.go`):

```text
defaults < JSON -config (explicit path only) < dotenv ./.env (cwd-wins seed)
  < DB overlay config:* rows < process env
```

`POST /admin/api/settings` dual-writes overlay, `/admin/reload` fans out via
`applyReloadedConfig` (`backend/internal/server/admin.go`).

## Decision

Authority + staleness + latency decides the home:

- Dashboard edits it and it must survive restart → `DB` (`settings` overlay).
- Container binding resolves before `DB` loads → `env` only.
- Sockets/goroutines, upstream-owned, or sub-second hot path → `mem`
  (+ timestamped `DB-hint` at most, re-validated, never authoritative).
- Queryable/auditable/retained → `DB` table with `Purge` cutoff.
- Tail/debug bulk → `log` file, never `DB`. Ad-hoc `JSON` forbidden
  outside explicit `-config`.

### Config plane → `DB` overlay is truth, `env` wins, files are seed

| Datum | Home |
|---|---|
| `AUTH_TOKENS`, `ADMIN_TOKEN`, `API_KEYS`, `WEBHOOK_URL` | `DB` rows (`0600`) + dedicated endpoints; `.env` = boot seed only |
| Live knobs (`SAFE_MODE`, `BRIDGE_ENABLED`, `MODELS_ALLOW`, `PIN_MODEL`, `SLOTS_*`, `QUEUE_*`, `COOLDOWN_*`, `SESSION_*`, `MATURITY_*`, `QUOTA_*`, `RATE_LIMIT_*`, `CORS_*`, …) | `DB`, live-apply via reload fan-out |
| Restart-only tunables (`UPSTREAM_BASE_URL`, `REQUEST_TIMEOUT`, `TLS_FINGERPRINT`, `LOG_LEVEL`, …) | `DB` persist + honest `restart_only` response |
| `LISTEN_ADDR`, `DB_PATH` | `env` only (socket/`sqliteDSN` resolve before overlay loads; `DB_PATH` has no catalog entry) |
| `SESSION_STATE_FILE`/`SESSION_PERSIST`, `LOG_FILE`, `HTTP_READ_TIMEOUT`, `AUTO_DISCOVER_TOKEN`, `ADMIN_FORCE_SECURE_COOKIES` | `env` only (proposed; readers consult `env/.env`, never overlay — a saved row is an inert lie) |
| `-config JSON` / `.env` / `.env.example` | `JSON` explicit path only; `.env` seed + break-glass editor; `.example` docs |
| `SSE tokenStateHash` (`dashboard/events.go`) | `mem` (1s view fingerprint, never disk) |

One-shot `config:migrated_env_v1` converges effective config into the overlay;
`env` still wins at runtime (`settings_migrate.go`).

### Runtime/session plane → `DB-hint` vs `mem`

| Datum | Home | Restart |
|---|---|---|
| Session slot (`instanceID/status/model/expiresAt/graceEnd`) | `DB-hint` (`sessions_persist` per token-hash) + `mem` live | Y, re-validated: `pollPersisted` re-`GET`s, drops on `428/410/409/rate\|ip\|spend` (`session/session_poll.go`) |
| Queue view (`position/queueDepth/pollAt`) | `mem`, never adopt | N (seconds-fresh upstream queue) |
| Quota last-seen + referral/freebucks/standing/promo blocks | `DB-hint`, stale-marked, display-only | Y |
| Active run (`runID/agentID/traceSessionID`) | `DB-hint` (runs blob) | Y, adopt-or-`re-START` |
| Slot counters + FIFO queues (`pool/slot_ledger.go`) | `mem`, zero on boot | N — waiters are parked goroutines; counters without waiters park fresh traffic behind dead holders; WAL on hot path rejected |
| Cooldown/ban/rate windows, `ip_capped`/burst timers | `mem` (+ optional timestamped hint, never authoritative) | N — stale rows park healthy tokens; `ip_capped` is a per-IP wall, persisting per-token spreads one IP's limit to all accounts |
| Single-flight `refreshErr`/gates, probe backoff, refund trackers, bridge entries/LRU, spend pacing | `mem` | N — un-rehydratable or derived; persisted error = permanent boot failure |
| Spend/ledger/admissions (`pool/ledger/<hash>`, `pool/admissions`) | `DB-hint`, window-clipped | Y (`installLedger` drops out-of-window) |

Why `instanceId` is temp by necessity: upstream mints + rotates it on every
admission `POST` (`session/session_admission.go`); the repo's own session
windows are a 5s pre-expiry margin and a 30-minute grace drain
(`session/session.go:22-28` — `expiryMargin`, `graceWindow`), with no
heartbeat — a stored id without re-validation buys a `428/410/409` round-trip
at best, a stuck re-poll loop at worst.

### History/observability plane → `DB` tables vs `log` vs `mem`

| Datum | Home | Retention |
|---|---|---|
| `log_entries` (off-path `spillCh 1024`, `100 rows/1s`) | `DB` | `168h` |
| `request_records` (`+client_key_hash`, sync write) | `DB` | `168h` |
| `quota_snapshots` (change-point only) | `DB`, boot seed | `90d` |
| `maturity_events` | `DB` | `90d` |
| `pages_state` | `DB` upsert | none (missing = default) |
| Rollups/dailies (`p50/90/99`) | `mem` recomputed (`GROUP BY`, `168h` clamp) | — (no stored table, avoids dual-write staleness) |
| `UsageRecord` ring (`5000`, evict-oldest) | `mem`, reset on restart by design | — (sync-`DB` on hot chat path rejected; future table only via spill channel) |
| Live rings (`logring 500`, `metricHist`, traces) | `mem` | — |
| Process log (`stderr` + `LOG_FILE`) | `log` file, append, external rotation | — (`0644`: no secrets; `telemetry.go` redaction) |
| `DEBUG_DUMP ./dump/` | `log` file only, `0600` redacted | delete after session; never `DB` |

Raw tokens/keys never reach `DB`/rings/logs: `hex(sha256)[:16]` only
(`00005_client_key_hash.sql`); `0600` preserved on copy/restore.

## Crash / update / backup law

- Live `DB` on `db_data` survives recreate. Never plain-`cp` live
  `.db/-wal/-shm` mid-checkpoint — stop first or `backup()/VACUUM INTO`
  (`scripts/backup-state.sh` → recreate → `verify-state.sh`).
- Corrupt source → warn + boot live-only (reads empty, mutations `503`).
- Legacy carry (`persist_carry.go`, `ImportLegacyHistoryDB`): per-table
  empty-gate, idempotent no-op when converged; never cross-file merge.
- Safety backups EXCLUDE bulk (`log_entries`/`request_records`, 9router
  precedent: keep 3, manual restore); their restore path is `export/import JSON`.
- Directory binds only (`.:/app/state`); single-file binds break atomic rename.

## Changes proposed (additive, no migration)

1. Gate `SESSION_STATE_FILE`/`SESSION_PERSIST`/`LOG_FILE`/`HTTP_READ_TIMEOUT`/
   `AUTO_DISCOVER_TOKEN` to `env`-only `400` (extend `ADMIN_FORCE_SECURE_COOKIES`
   precedent; old rows cleared by `DELETE :key`).
2. ~~Optional timestamped cooldown-hint + bridge-survivor blob~~ — **shipped**
   (`pool/cooldown_hint.go`; hints only, expiry-checked, `mem` stays the
   authority).
3. Optional: dedupe quota double-writer to the session row; fix
   `pool_persist.go` header claiming bridge rows `snapshotPoolState` doesn't stage.

## Open questions

- `AUTO_DISCOVER_TOKEN` overlay-read intent vs hard `400`?
- Accept `400` on the env-only move, or keep persist-with-caveat?
- `UsageRecord` restart-reset permanent, or future spill-fed table?
- Frozen `168h/90d` purge cutoffs permanently untunable?
