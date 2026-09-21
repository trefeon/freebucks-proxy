# SmartProbe — activity- and reset-triggered session-less quota refresh

Status: **shipped**. Proposed in #627 (`git show 8cd7b385:docs/decisions/smart-probe.md`),
implemented in #644 (`5e662f5f`, "feat(pool,dashboard): smart zero-cost
auto-refresh + probe honesty"). The prober is
`backend/internal/pool/smart_probe.go`. Goal: keep a parked account's quota
truthful without ever sweeping — a token is probed only when real activity
marked it dirty or a known reset instant arrives, so an idle pool costs zero
upstream traffic (`smart_probe.go:1-18`).

## What the original proposal got wrong

The proposal was written before the implementation and inverted the premise:
it described a *data-gated sweep* (probe the tokens that lack quota data,
once per restart), while what shipped is *trigger-only* — activity and reset
instants create eligibility, and missing data never does
(`smart_probe.go:1-19`, `:107-113`, `:176-189`). Divergences, each re-verified
in the code:

| The proposal said | What shipped |
|---|---|
| `QuotaSavedAt` zero or `QuotaStale` makes a token a candidate; fresh tokens "exit permanently until the next restart". | Trigger-only: a dirty mark or a past reset instant is required; with no quota memory at all the reason is `idle` and the token is never due (`smart_probe.go:107-113`, `:176-189`). |
| "No new goroutines, tickers, or lifecycle": the leg rides `backfillLoop`. | Rides `maintainTick` on the 1m maintain grid and dispatches a detached, wg-tracked, Shutdown-cancelable stagger worker (`pool_lifecycle.go:24`, `:318-322`; `smart_probe.go:243-259`). |
| A pure "tier classifier" (`tier-new`/`tier-stale`/`tier-fresh`/`tier-dead`) plus a "no live session" gate. | No classifier and no run-in-flight gate: one guard chain inside `smartProbeDueToken` that returns a reason string (`smart_probe.go:140-190`). |
| Failure backoff base 1m doubling. | 5m base; the first probe-429 parks 10m, then 20m, … to the cap (`smart_probe.go:47-50`, `:66-83`; pinned by `smart_probe_test.go:196-220`). |
| Probe ctx timeout 8s. | 15s (`smart_probe.go:38-40`) — the same bound `ProbeAllTokens` gives each token. |
| Fields `nextProbeAt time.Time`, `probeFetch atomic.Bool`, `lastSmartProbeAt`. | `probeNextAt atomic.Int64` (unix nanos), `probeInflight`, `probeDirty`, `probeLastAt`, `probeBackoffStep`, plus pool-wide `smartProbeInflight` (`pool.go:399-400`, `:438-450`). |
| Restart burst: 20 stale tokens ≈ 7 ticks ≈ 3.5 min. | A restart enqueues nothing by itself: the schedule is memory-only (`pool.go:438-445`), so eligibility re-derives from quota memory — a token whose remembered reset instant is already past fires once (reason `reset`, `probeLastAt` is 0), a token with no memory stays `idle`. A 20-token due set is dispatched 3 per 1m round with 5s in-round spacing: 7 rounds, the last ~6 min after the first (`smart_probe.go:44-46`, `pool_lifecycle.go:24`). |
| All outcomes log at debug except state transitions at info. | Refusals and the exhausted park log at Info; dispatch, suppression, ok and error-park log at Debug (`smart_probe.go:300`, `:309`, `:240`, `:264`, `:322`, `:317`). |
| Excise `QuotaAutoProbe`, `QuotaProbeActiveInterval`, `QuotaProbeIdleHeartbeat`. | Not excised: still declared (`config.go:164-180`) and still documented in `.env.full-example:153-156` (`#QUOTA_AUTO_PROBE=true`, "ADR-0022"), with no Go reader anywhere. |
| Non-goal: "No frontend changes". | The implementing commit also shipped dashboard honesty copy and the Probe-all button (`5e662f5f`: `frontend/src/lib/pages/Tokens.svelte`, `frontend/src/lib/components/AllowancesPanel.svelte`, `backend/internal/dashboard/dashboard_render.go`). |

## Rules (shipped)

### Triggers — a token becomes due only through one of these

- **Dirty mark.** Three call sites mark refresh interest: a lease grant
  (`acquire_route.go:973-974`), a successful chat (`pool.go:930-931`), and a
  429 refusal (`cooldown.go:33-36`). `markProbeDirty` sets the flag and only
  ever moves `probeNextAt` *earlier* to now, never past a pending 429 backoff
  (`smart_probe.go:85-105`).
- **Reset instant.** The earliest known quota-reset instant for the token:
  per-model `ResetAt` rows, the remembered-429 reset (live window only), the
  Freebucks daily refill. Quota memory with no usable instant falls back to
  the next Pacific midnight; no memory at all reports *none*, so idle
  accounts are never due (`smart_probe.go:107-138`).
- A reset fire needs `!now.Before(resetAt)` **and**
  `resetAt.After(probeLastAt)`, so one fire per known window — the "catch-up"
  cannot repeat inside one window (`smart_probe.go:179-188`).

### Due guards, in order (`smartProbeDueToken`)

`no-entry` → `locked` → `quarantined` → `banned` (live `BanError`) →
`cooling` (`CooldownUntil`) → `country-blocked` → `inflight` (per-token probe
single-flight) → `fresh` (probed or seeded within 60s, checked against
`QuotaSavedAt`) → `debounced` (`probeNextAt` not yet reached); only then the
triggers apply, returning `dirty`, `reset`, or `reset-pending`, else `idle`
(`smart_probe.go:140-190`).

Notes on what is deliberately absent: there is no run-in-flight gate here —
the mid-chat gate belongs to the session-liveness polls
(`pool_lifecycle.go:349-352`), not to this prober — because the probe is
session-less by construction and claims no session slot
(`smart_probe.go:11-18`, `probe.go:96-99`). The 60s fresh window is the
debounce that absorbs hot-lane mark bursts.

## Non-goals (shipped)

- No periodic full sweep and no page-visit sweep: nothing is probed merely
  for lacking data (`smart_probe.go:3-9`).
- No session work: probes are `GET /api/v1/freebuff/session` with no instance
  header — they never admit, claim a slot, or create a session
  (`smart_probe.go:15-18`, `probe.go:96-99`).
- No cooldown or ledger writes by the scheduler: quota truth lands through
  the shared `UpdateQuotaFromProbe` write (`smart_probe.go:267-274`).
- No persisted schedule: the dirty mark, the timers and the backoff step are
  in-memory only and re-derive from quota memory after a restart
  (`pool.go:438-445`).
- No new ticker: the only added lifecycle is the wg-tracked round worker on
  the existing maintain clock (`smart_probe.go:243-259`).

## Cadence and budget

- **Clock.** The pass is called from `maintainTick` — `maintainInterval = 1m`
  (`pool_lifecycle.go:24`, call at `:318-322`). The idle early-returns leave
  before that line, so a parked pool parks the prober too
  (`pool_lifecycle.go:296-306`, comment `:318-321`).
- **Dispatch.** The tick runs predicate checks only on the maintain
  goroutine; a due round is single-flight (`smartProbeInflight` CAS,
  `smart_probe.go:239-242`) and runs in a detached, wg-tracked, ctx-cancelable
  goroutine (`:243-259`). Shutdown is terminal — no dispatch past
  `draining` (`:229-231`) — and the switch is read per tick via `cfg.Load()`
  (`:223-226`).
- **Per-round budget.** At most 3 tokens (`smartProbeMaxPerTick = 3`,
  `:44-46`) with fires spaced 5s apart (`smartProbeStagger = 5s`, `:41-43`) —
  the proposal's rule, shipped unchanged. A wider due set waits for the next
  pass: 20 due tokens → `ceil(20/3) = 7` rounds, last dispatch ~6 min after
  the first. This is strictly gentler than the manual path, where dashboard
  "Probe all" fires `ProbeAllTokens` — bounded concurrency 4, 15s per token,
  no pool-level stagger (`server/admin_tokens_probe.go:41-53`,
  `pool/probe.go:262-263`).
- **Per-token probe.** One 15s ctx (`:38-40`), per-token single-flight CAS
  (`:281-284`), then the outcome is folded into the schedule (`:287-322`).

## Failure schedule

- **429 / rate-limit family** (typed `RateLimitError`, `ErrRateLimited`, or a
  `rate_limited` outcome): `probeBackoffStep++`, then
  `probeNextAt = now + 5m × 2^step` saturating at the cap — 10m, 20m, 30m
  with the 30m default (`:291-302`, `:47-54`, `:66-83`). A `spend_limited`
  probe status is mapped to the same typed error by the shared probe path
  (`probe.go:158-181`), so it steps the same schedule.
- **Freebucks exhausted**: park exactly until the probe's parsed `reset_at`
  when it lies ahead, else the next Pacific midnight
  (`:303-311`, `:325-333`).
- **Any other error** — a refusal outside that family (e.g. `ip_capped`,
  deliberately distinct from `ErrRateLimited`, `upstream/errors.go:58-66`),
  or a transport failure — parks one quiet 5m interval *without* consuming a
  backoff step (`:312-319`).
- **Success / anything else** clears the schedule (`probeBackoffStep = 0`,
  `probeNextAt = 0`); the next fire waits for fresh activity or the next
  reset instant (`:320-322`).
- **429-class refusals never cooldown the token.** The scheduler installs no
  cooldown and touches no ledger — quota truth arrives only through
  `ProbeTokenDetailed`'s `UpdateQuotaFromProbe` write (`:267-274`,
  `probe.go:210-212`). The pre-existing rule is unchanged: a 429 (including
  `spend_limited` and `ip_capped`) is never persisted as a per-token park
  (`cooldown_hint.go:18-21`).

## Knobs (Group Pool, read per tick via `cfg.Load()`)

| Key | Kind | Default | Notes |
|---|---|---|---|
| `SMART_PROBE_ENABLED` | bool | `true` | Master switch. False = manual probes only (`keycatalog.go:283-287`). |
| `SMART_PROBE_BACKOFF_MAX` | text duration | `30m` | Ceiling for the 429 doubling. Zero-tolerant: empty or non-positive falls back to 30m (`config_load.go:226-234`, `keycatalog.go:278-282`). |

Chain per knob: `config.go` struct (`:181-191`) → `config_keys.go` JSON +
defaults (`:89-95`, `:164-165`) → `config_load.go` dotenv (`:131-132`) and DB
overlay tier (`:613-614`) with the zero-tolerant duration parse
(`:226-234`) → `data.go` effective display (`:164-167`) → `keycatalog.go` row
(`:278-287`, static-set map `:422`) → `server/admin_env.go` live map
(`:429-430`) → `.env.example` (`:189-197`). Both keys apply live on reload
(`.env.example:195`), which is what `cfg.Load()` per tick and per fire
implements (`smart_probe.go:223`, `:298`).

## Files

- `backend/internal/pool/smart_probe.go` — constants, `markProbeDirty`,
  `smartProbeResetInstant`, `smartProbeDueToken`, `smartProbeDue`,
  `smartProbeTick`/`smartProbeTickAt`, `smartProbeFireOne`,
  `smartProbeExhaustedResume`.
- `backend/internal/pool/pool.go` — per-token schedule fields and the
  pool-wide round flag (`:399-400`, `:438-450`).
- `backend/internal/pool/pool_lifecycle.go` — the `maintainTick` call
  (`:318-322`).
- Mark sites: `acquire_route.go:973-974`, `pool.go:930-931`,
  `cooldown.go:33-36`.
- `backend/internal/pool/probe.go` — the shared session-less probe
  (`ProbeTokenDetailed`) and its 429-status mapping (`:158-181`).
- Config chain files above + `.env.example`.
- `backend/internal/pool/smart_probe_test.go` — the behaviour pins.

## Verification

- Idle accounts see zero upstream traffic (`smart_probe_test.go:24-45`); a
  dirty account fires once and goes quiet (`:56-95`); a reset instant probes
  a parked account (`:96-143`); a lease grant and a 429 refusal both mark
  interest (`:73-77`, `:145-166`).
- 429 doubling measured through a real fire: 10m then 20m, with zero session
  creates throughout (`:196-220`); the pure delay table saturates at the
  configured cap (`:173-190`).
- Banned/quarantined accounts are never probed even with interest marked
  (`:235-260`); the switch off dispatches nothing (`:274-289`).
- Manual path parity: dashboard `POST /admin/tokens/test-all` →
  `handleTokensTestAll` → `ProbeAllTokens` (`server_routes.go:151-152`,
  `admin_tokens_probe.go:41-53`).

## Rollout

Shipped behind the default-on switch: verify on a review host (activity
drives the first fills, then the pool goes quiet, zero log spam), then
production. Rollback = `SMART_PROBE_ENABLED=false` — live-apply, no rebuild —
or leave the key unset (`.env.example:189-197`).
