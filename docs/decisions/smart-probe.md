# SmartProbe — probe only the accounts that have no data

Status: proposed. Goal: after a restart (or for a never-used token) the
dashboard cards fill themselves in; upstream sees the minimum possible
traffic. This replaces the excised QUOTA_AUTO_PROBE interval scheduler
(which probed every token on a timer) with a need-gated prober.

## Non-goals

- No periodic refresh of healthy data. A token with fresh quota is never
  touched, however old the reading. (Aging/liveness is a separate decision.)
- No frontend changes. The existing `quota_stale` last-seen label clearing
  plus `quota_saved_at` advancing IS the UI.
- No new goroutines, tickers, or lifecycle: the leg rides `backfillLoop`
  (first pass 2s after Start, then every 30s, already under `p.wg`/ctx),
  next to the email/streak backfill — the same "missing data" shape.

## Eligibility: a token is probed only when ALL hold

1. **Pooled mode** — bridge has no roster; the loop no-ops (diag parity).
2. **Master switch on** — `SMART_PROBE_ENABLED` (default true).
3. **Needs data** — `QuotaSavedAt` is zero (never probed/admitted this
   process) OR `QuotaStale` is true (disk-restored, no live admission yet).
   Fresh tokens exit permanently until the next restart.
4. **No live session** — no admitted instance id and no run/chat in flight
   on this token. Tokens WITH sessions already get quota via admission +
   session-liveness polls; this gate also removes the 428 mid-chat kick
   hazard entirely (upstream allows one client per account).
5. **Not untouchable** — not quarantined (`quarantine` pointer set), not in
   cooldown, not locked. A probe must never amplify a ban (issue #140).
6. **Throttle** — no probe in flight for this token (atomic single-flight,
   same `CompareAndSwap` shape as `accountFetch`/`streakFetch`), `now >=
   nextProbeAt`, and the pool-wide stagger allows it (see below).

The skip conditions in (4)+(5) mirror `sessionPollTick`'s; reuse its helpers
where factored, mirror where not — the two gates must never disagree about
"chat in flight" or "cooling down".

## Tiers (pure classifier, fake-clock testable — cf. `idleForAt`)

- `tier-new`: `QuotaSavedAt` zero → candidate on the first pass after Start.
- `tier-stale`: `QuotaStale` true → candidate once; success clears stale via
  the existing `UpdateQuotaFromProbe` write path (no new write code).
- `tier-fresh`: neither flag → never touched.
- `tier-dead`: quarantined / cooldown / lock → never touched. A ban error
  carrying `resumes_at` parks `nextProbeAt` at that instant (one re-check
  after unban, not a poll).

## Cadence and anti-spam budget

- Per-tick launch budget: **3 tokens**, launches **>=5s apart**
  (pool-level `lastSmartProbeAt`). Worst post-restart burst for 20 stale
  tokens: ~7 ticks (~3.5 min), sequential, zero-cost GETs. For comparison,
  opening the Diag page today fires N unthrottled probes at once — the
  smart prober is strictly gentler than existing manual behavior.
- Per-token failure backoff: 1m doubling, capped at
  `SMART_PROBE_BACKOFF_MAX` (default 30m). Success resets to zero and (via
  the stale-clear) removes the token from the candidate set, so steady-state
  upstream cost is exactly zero.
- 429-class failures (`rate_limited`, `ip_capped`, `spend_limited`) back the
  PROBE off only — they never cooldown the token (established rule: an
  IP-level refusal must not park the account).
- Probe ctx timeout 8s (diag parity). All probe outcomes log at debug
  except state transitions (first fill, ban/terminal seen) at info.

## No-spam proof (reviewer checklist)

1. Fresh token → tier-fresh → zero calls, forever (until restart).
2. Banned/quarantined/cooling token → tier-dead → zero calls.
3. Flapping token → doubling backoff to 30m cap → <=2 calls/hour.
4. Restart burst → 3/tick + 5s spacing, idle dataless tokens only.
5. Mid-chat token → excluded by the no-live-session gate (no 428 kicks).
6. Switch off → behavior identical to today (manual Test buttons only).

## Knobs (Group Pool, live-apply read per tick via `cfg.Load()`)

| Key | Kind | Default | Notes |
|---|---|---|---|
| `SMART_PROBE_ENABLED` | bool | true | Master switch; false = manual probes only. |
| `SMART_PROBE_BACKOFF_MAX` | text duration | 30m | Failure-backoff ceiling. Reuses the dead `SmartProbeBackoffMax` struct field (declared for the old scheduler, never wired) — wire it, don't add a twin. Zero-tolerant like `QUEUE_WAIT`. |

Chain per knob (memory: dotenv → static → live → SSE hash → store
refresh): `config.go` struct → `config_keys.go` JSON →
`config_load.go` (`overrideString` + `overrideStringFrom` + zero-tolerant
parse) → `data.go` effective display → `keycatalog.go` row + static-set map
→ `server/admin_env.go` live map → SSE hash → store refresh →
`.env.example`. Tests: `config_knobs_test.go`, `keycatalog_test.go` maps.

Excise in the same change (dead since the scheduler removal, catalog rows
already gone): `QuotaAutoProbe`, `QuotaProbeActiveInterval`,
`QuotaProbeIdleHeartbeat` struct fields. No other references exist.

## Files

- NEW `backend/internal/pool/smart_probe.go` — tier classifier (pure func,
  clock-injected) + `smartProbeTick` leg + worker (single-flight guard,
  8s ctx, backoff accounting, transition logs). ~150 lines.
- `backend/internal/pool/pool_lifecycle.go` — call the leg from
  `backfillLoop`'s roster iteration; extend the loop comment.
- `backend/internal/pool/pool.go` — `tokenEntry`: `nextProbeAt time.Time`
  (under entry mutex — check what guards `QuotaSavedAt` reads), `probeFetch
  atomic.Bool`; `Pool`: `lastSmartProbeAt` (+mutex).
- Config chain files above + `.env.example`.
- NEW `backend/internal/pool/smart_probe_test.go` — tiers on fake clock;
  banned/quarantine/cooldown never probed (mock asserts zero upstream hits);
  concurrent ticks single-flight; failure doubles to cap; stale clears on
  mock success; disabled switch = zero calls.

## Rollout

Land behind the default-on switch; verify on a review host (restart with
stale tokens → cards fill within minutes, zero log spam), then production.
Rollback = `SMART_PROBE_ENABLED=false` (no rebuild dance) or env unset.
