# Ordered account placement: the lowest-index lane that can serve wins

Status: shipped · 2026-10-01 · opt-in (`POOL_ORDERED_PLACEMENT`, default
false). The arrival scan no longer lets a later lane's warm session take a
request while an earlier account can serve it.

Non-goals: no change to `spillOrder` ordering or its filter set
(`backend/internal/pool/spill_order.go`), no change to the seat gate's
idle-rotate / busy-defer rule (`backend/internal/pool/seat.go`,
`backend/internal/session/session_manager.go`), no new wire code, no change to
the walk's park-on-the-first-full-lane discipline, and no behavior change with
the knob off (the default).

## Context

- Pooled placement walks the strict roster index order
  (`spillOrder`, `backend/internal/pool/spill_order.go:28`) so drains are
  reproducible. The smart path's first step is the arrival scan
  (`scanWarmFree`, `backend/internal/pool/acquire_route.go:402`): the first
  lane that passes the eligibility gates AND already holds a usable session
  for the requested model AND has a free slot grants instantly, **before**
  the walk runs.
- That scan only looks for a usable session. When the earliest lane is
  eligible and free but COLD for the model (no session for it yet) and a later
  lane is WARM, the later lane wins. Observed on a two-account pool: account 1
  sat idle for a model while account 2 served every turn.
- A cold lane is not a dead lane: admitting on it creates its session inline,
  exactly as the walk already does when no warm lane exists
  (`scaleoutFrom`, `backend/internal/pool/acquire_route.go:516`).

## Decision

`POOL_ORDERED_PLACEMENT` (bool, default **false**) makes placement prefer the
LOWEST roster index that can serve the request. In the arrival scan:

- When the knob is on, the scan stops at the first admissible lane with a
  free slot (`slotHasFree`, a read-only probe —
  `backend/internal/pool/model_queue.go:131`; taking and releasing a slot
  would hand it to a queued waiter, which is not the scan's to give):
  - that lane is WARM → grant there, as before;
  - that lane is COLD and its seat is free → grant **nothing** — the walk
    runs from the head and admits on the earlier cold lane, creating its
    session inline;
  - that lane is COLD and its seat is held (`seatCounter.busy`,
    `backend/internal/pool/seat.go:70`) → keep scanning for a later warm
    lane. Preferring a lane whose seat another model's in-flight turn holds
    would rotate the account's single upstream seat out from under that turn
    (`seat.go` header; `session_manager.go` `SetReAdmitGate`), so the busy
    lane does not pre-empt the warm scan.
- When the knob is off, the scan is byte-identical to the legacy arrival scan:
  a warm lane grants regardless of earlier cold lanes.

Once suppression fires, the existing walk decides — including its
park-on-the-first-full-lane discipline (`scaleoutFrom`),
`parkOnFullLane`) — so nothing about queueing or spill budget changes.

## Cost tradeoff (why opt-in, not on by default)

- Reusing a warm lane costs 0 additional Freebucks: the model's 1-hour
  purchase seat is charged once at admission and every turn inside the window
  on the same `holderInstanceId` is free (`docs/FREE-TIER-GATE.md:69-70`).
  A later warm lane therefore serves at zero marginal cost.
- Admitting on an earlier cold lane is free **only if** that account already
  holds the model's seat. If it does not, the extra admission buys a fresh
  seat: on `deepseek/deepseek-v4-flash` that is 15 Freebucks/hr peak (10
  off-peak) per account (`docs/FREE-TIER-GATE.md:72`,
  `docs/FREE-TIER-GATE.md:535`). Spreading work across every account can
  therefore multiply seat purchases instead of reusing one.
- So the tradeoff is operator-specific: ordered placement favors earlier
  accounts (deterministic drains, one account at a time actually used, less
  concurrency spread) at the risk of paying for more seats. Default off keeps
  the incumbent warm-reuse behavior until the operator opts in.

## How to enable

- DB overlay row (preferred, no file edit): `config:POOL_ORDERED_PLACEMENT=true`
  in the settings table. It applies live on reload (the pool reads the knob per
  `Acquire` via `p.cfg.Load()`), unless the process environment pins the key.
- Alternatives: `POOL_ORDERED_PLACEMENT=true` in `.env`, or the dashboard
  Pool section (both flow through the same `applyMappedValues` key list,
  `backend/internal/config/config_load.go`).

## Files it touches

- `backend/internal/pool/acquire_route.go` (`scanWarmFree`),
  `backend/internal/pool/model_queue.go` (`slotHasFree`),
  `backend/internal/pool/seat.go` (`busy`).
- `backend/internal/config/config.go`, `config_keys.go`, `config_load.go`,
  `data.go`, `keycatalog.go`, `keycatalog_test.go` (the knob's usual
  plumbing), `frontend/e2e/fixtures{,-realworld}/config-meta.json`
  (catalog mirror), `.env.example`.
- `backend/internal/pool/ordered_placement_test.go` (this policy's tests).
- `docs/decisions/ordered-account-placement.md` (this note).

## Verification

- `TestOrderedPlacementPrefersLowestIndexLane`: account #1 cold, account #2
  warm for the model; knob on → lease on #1 (session created inline) and #2
  never served.
- `TestOrderedPlacementOffKeepsWarmGrant`: same layout, default knob → lease
  on #2 and #1 untouched (0 creates, 0 requests) — pins today's behavior so a
  future change cannot silently flip it.
- `TestOrderedPlacementSkipsBusySeatToNextAccount`: account #1 holds a live
  model-A turn; a model-B request with the knob on still lands on the warm
  account #2 (two models on two accounts), and account #1's session is not
  rotated.
- `TestOrderedPlacementRotatesIdleEarlierLaneInPlace`: release the model-A
  lease first; the model-B request then admits on the earlier account #1 and
  switches its session in place (two creates on #1, #2 warm-up only).
- `go test ./backend/internal/pool/...` green; `go vet`/`gofmt` clean; the
  config package's catalog/fixture parity tests green.
