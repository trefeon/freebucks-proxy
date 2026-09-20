# MASQ — Minimal Account Slot Queue Implementation Plan

> Mechanism: Ordered Queue-Spill (with Precious Holders). MASQ is the official queue name; Ordered Queue-Spill remains the mechanism name in docs.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the freebucks-proxy pool controls with MASQ (Minimal Account Slot Queue): strict Account #1→#N order, 2 concurrent slots per (account, model) — one account may hold 2×modelA + 2×modelB at the same time, a FIFO queue per (account, model) that spills to the next account only when the queue-wait expires, precious sessions are never dropped, PIN_MODEL strict 1 account = 1 model, and natural 429s send requests back to the queue without parking the account.

**Architecture:** Three sequential phases on a single branch. Phase E prunes every artificial limiter (correlative failover walk, ip_capped cooldown, global 5m unfit, bounded COOLDOWN_*, proactive probe/maturity, scorer/rotation/leader-election, chat retry-once, park/sweep) until only natural admission + queue remain. Phase I builds the new mechanism (slot ledger, ordered lanes + spill, precious open-set, PIN_MODEL, 429-requeue) on top of those remains. Phase C locks strict order, the final knobs, and the PIN UI. Each phase ends with a pool that still compiles and is covered by throwaway tests.

**Tech Stack:** Go (backend/internal/pool, backend/internal/config, backend/internal/server, backend/internal/session, backend/internal/upstream classification only), Svelte (frontend/src/lib/pages + components + utils), throwaway Go tests using the existing harness (`newTestPoolCfg`, `testutil.NewMock`, `SessionHandler`, `RequestsSnapshot`).

**Spec:** Proposal "Ordered Queue-Spill with precious holders" (approved 2026-09-17, R1–R6) + the UpstreamLimits natural-limiter table + the CurrentControl flow. Recovery note: design sections 1/2/3 from the SlotOverflowDesign lane were unreadable (null agent payload; that lane’s final yield was cut off by the token limit), so this plan is grounded directly in R1–R6 from the batch contract, the fully recovered UpstreamLimits/CurrentControl payloads, and direct code reads at this worktree’s revision (`origin/main` 328261be). Every `file:line` below was verified at that revision.

## Global Constraints

- Shared checkout `D:/github_repo/freebucks-proxy` contains uncommitted user work: DO NOT touch. All work in this worktree + the `plan/ordered-queue-spill` branch only.
- No project-wide build/lint/test mid-flight. Evidence per task comes only from that task’s own throwaway test file (`go test ./backend/internal/pool/ -run TestNama -count=1`), deleted before the next phase unless promoted to a keeper.
- Secrets: never dump email/token/session-id in logs/tests; sanitize to Account #N.
- Code comment language: English (repo convention). UI language: follow the existing `$tr(...)` pattern.
- Clean cutover: every knob deletion migrates every caller + test + UI + keycatalog; no shims/aliases/deprecated paths. The only alias allowed: NONE — `TOKEN_MAX_CONCURRENT` is fully renamed to `SLOTS_PER_ACCOUNT`.
- R1–R6 are the final acceptance; each R maps to its implementing task in the §Traceability table, and each behavior task carries a Repro block with actual code.

**Final knobs (the only ones alive after Phase C):**

| Knob | Default | Meaning |
|---|---|---|
| `SLOTS_PER_ACCOUNT` | `2` | 2 concurrent slots per (account, model) — one account may hold 2×modelA + 2×modelB at the same time (full rename of `TOKEN_MAX_CONCURRENT`) |
| `QUEUE_WAIT` | `30s` | total parking per lane before spill; no per-model override (single writer) |
| `QUEUE_DEPTH` | `16` | parked waiters per account; `0` = no queue |
| `PIN_MODEL` | `""` | strict `idx:model` pin, one model per account, e.g. `"0:z-ai/glm-5.2"` |
| `MAX_SPILL_ACCOUNTS` | `0` | limit on follow-on accounts per request; `0` = unbounded (full index chain) |

**Dead knobs (deleted entirely, including UI + tests + keycatalog):** `TOKEN_ROTATION`, `ROUTING_SMART`, `RATE_LIMIT_FAILOVER`, `MODEL_LOCKS` (replaced by `PIN_MODEL`), `TOKEN_MAX_CONCURRENT` (old name), all `COOLDOWN_*_MS` + `COOLDOWN_IP_*`, `SESSION_PARK_*`, `SESSION_POLL_MAX_MS` (poll pacing hardcoded to 20s base / 5m cap per the vendor port in `backend/internal/session/session_park.go:35-38`), `SMART_PROBE_BACKOFF_MAX_MS`, `MATURITY_*`, `QUOTA_PROBE_*`, `MATURITY_TOUCH_MODEL`.

---

## File Structure

### Phase E — Excise (drop artificial limiters; one removal responsibility per file)

- Modify `backend/internal/pool/acquire_route.go` — E1: cut the failover walk for correlative refusals (surface directly).
- Modify `backend/internal/pool/cooldown.go` — E2: delete `CooldownTokenIpCapped` + the ip_capped classification in `classifyAndCooldown`; E4: delete the general bounded-backoff (keep ban/quarantine).
- Modify `backend/internal/runs/cooldown.go` — E2/E4: delete the runs side for ip_capped + bounded consts (auth 30m, country 15m, 7d ceiling).
- Delete `backend/internal/pool/unfit.go` (+ `unfit_test.go`) — E3: delete the global unfit registry.
- Modify `backend/internal/upstream/classify.go` — E4: delete the bounded `COOLDOWN_*` consts, pass RetryAfter through as-is instead.
- Delete `backend/internal/config/cooldown.go` — E4: delete accessors + constants (except whatever is kept for the ban park if any; default: delete entirely).
- Delete `backend/internal/pool/quota_smartprobe.go` (+ `quota_smartprobe_test.go`, `quota_smartprobe_fleet_test.go`, `quota_autoprobe_test.go`, `smartprobe_persist_test.go`) — E5.
- Delete `backend/internal/pool/quota_visitprobe.go` (+ `quota_visitprobe_test.go`) — E5.
- Delete `backend/internal/pool/quota_bootseed.go` (+ `quota_bootseed_test.go`) — E5.
- Delete `backend/internal/pool/maturity.go` (+ `maturity_test.go`, `maturity_auto_test.go`, `maturity_firegate_test.go`, `maturity_persist_test.go`, `maturity_resultday_test.go`, `maturity_skip_loop_test.go`, `maturity_touch_test.go`) — E5.
- Modify `backend/internal/pool/route_smart.go` — E6: delete scorer/stick/overflow (keep the interim FIFO slot; it moves in I1).
- Modify `backend/internal/pool/acquire_order.go` — E6: strip ke order index polos (diganti total di C1).
- Modify `backend/internal/pool/acquire_route.go` — E6: delete the leader-election gate (let the per-token single-flight session manager stand).
- Modify `backend/internal/server/engine_attempt.go` — E7: delete retry-once (single attempt, surface).
- Delete `backend/internal/session/session_park.go` — E8: delete park-vs-drop.
- Modify `backend/internal/pool/pool_lifecycle.go` + `backend/internal/pool/lifecycle.go` — E8: delete the idle-end sweep + poll-drop; hardcode poll pacing.

### Phase I — Implement (build the new mechanism)

- Create `backend/internal/pool/slot_ledger.go` — I1: FIFO slot ledger per (account, model), keyed `map[token]map[model]slotState` or equivalent (moved `routeSlotState`/`routeSlotPermit`/`routeSlotAcquire` from `route_smart.go:151-277`, renamed + keyed per model).
- Modify `backend/internal/config/config.go` + `config_keys.go` + `config_load.go` — I1: `SLOTS_PER_ACCOUNT`; I4: `PIN_MODEL`; I2: `MAX_SPILL_ACCOUNTS`.
- Create `backend/internal/pool/spill_queue.go` — I2: ordered lanes per model + spill-on-wait-expired + 429-requeue `notBefore`.
- Create `backend/internal/pool/precious.go` — I3: precious open-set {(token, model)} + anti-drop guard.
- Create `backend/internal/config/pin_model.go`, delete `backend/internal/pool/model_locks.go` (+ `model_locks_test.go`) — I4: strict `PIN_MODEL` parse/validation, delete `MODEL_LOCKS`.
- Modify `backend/internal/pool/acquire_route.go` — I5: admission/run-start 429 quota → requeue the same lane (no cooldown write, no failover).

### Phase C — Cutover (lock order + knobs + UI)

- Create `backend/internal/pool/spill_order.go`, delete `backend/internal/pool/acquire_order.go` (+ `acquire_order_test.go`) — C1: strict #1→#N index order.
- Modify `backend/internal/config/keycatalog.go` + `data.go` — C2: final knob catalog, delete dead entries.
- Modify `frontend/src/lib/utils/poolStrategy.js` + `StrategyPresetCard.svelte` + `TrafficSettings.svelte` + `AdvancedSettings.svelte` — C2: presets + copy follow the final knobs.
- Modify `frontend/src/lib/components/TokenDetailsDrawer.svelte` — C3: single-PIN editor per account.
- Modify `backend/internal/pool/concurrency_ladder_test.go` + `queue_wait_test.go` — C4: old expectations (overflow-assist, rotation) replaced with spill expectations; R1–R6 repro keepers promoted.

---

### Task E1: Stop the failover walk on correlative refusals

**Files:**
- Modify: `backend/internal/pool/acquire_route.go:435-480` (admission-error branches: `c.ipCapped`, `c.limitedIp`, `c.countryBlocked`)
- Modify: `backend/internal/pool/acquire_route.go:538-559` (same run-start-error branches)
- Test: `backend/internal/pool/spill_excise_correlative_test.go` (throwaway, delete after green unless promoted in C4)

**Interfaces:**
- Consumes: `p.classifyAndCooldown(runsMgr, err) *classifiedError` (`backend/internal/pool/cooldown.go:267`) with fields `ipCapped *upstream.IpCappedError`, `limitedIp *upstream.LimitedIpError`, `countryBlocked *upstream.CountryBlockedError`; `testutil.MockUpstream{SessionHandler, RequestsSnapshot}` (`backend/internal/testutil/mockupstream.go:115,686`).
- Produces: invariant for E2–E5: "after classify, correlative refusals never `continue` to the next token".

- [ ] **Step 1: write the correlative-no-walk throwaway test**

```go
func TestExciseCorrelativeNoWalk(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"ip_capped","retryAfterMs":60000}`))
	}
	p := newTestPoolCfg(t, func(c *config.Config) {}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := p.Acquire(ctx, modelA)
	if err == nil {
		t.Fatal("want ip_capped surface, got lease")
	}
	if !errors.Is(err, upstream.ErrIpCapped) {
		t.Fatalf("want ErrIpCapped, got %v", err)
	}
	if n := mock1.RequestsSnapshot(); n != 0 {
		t.Fatalf("walked to account #2 (%d requests), want 0", n)
	}
}
```

- [ ] **Step 2: run, expect FAIL** (today it walks: mock1 gets touched + every account gets a cooldown)

Run: `go test ./backend/internal/pool/ -run TestExciseCorrelativeNoWalk -count=1`
Expected: FAIL at `walked to account #2`.

- [ ] **Step 3: cut the walk — surface directly without `continue`**

In `acquire_route.go:435-456` (admission), change all three correlative-bucket branches to surface directly; example for ip_capped (repeat the same pattern for `limitedIp` and `countryBlocked`, and for the run-start block at `:538-559`):

```go
if ice := c.ipCapped; ice != nil {
    routeSlot.Release()
    return nil, ice
}
```

Delete `appendIpCapped`/`appendCountryBlock`/`MarkModelUnfit` on this path (unfit is deleted entirely in E3; the `MarkModelUnfit` at `:469` goes away here too, its registry in E3). `banned` STAYS in bucket+quarantine (keeper, see E4).

- [ ] **Step 4: run, expect PASS**

Run: `go test ./backend/internal/pool/ -run TestExciseCorrelativeNoWalk -count=1`
Expected: PASS; `mock1.RequestsSnapshot()==0`.

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/acquire_route.go backend/internal/pool/spill_excise_correlative_test.go
git commit -m "feat(pool): surface correlative refusals without failover walk"
```

### Task E2: Delete the per-token cooldown for ip_capped

**Files:**
- Modify: `backend/internal/pool/cooldown.go:39-51` (delete `CooldownTokenIpCapped`)
- Modify: `backend/internal/pool/cooldown.go:267-398` (delete the ip_capped branch + jitter + readmit-cap in `classifyAndCooldown`)
- Modify: `backend/internal/runs/cooldown.go:17-36` (delete the runs side: `CooldownIpCapped`, `IPMaxReadmits`, jitter)
- Modify: `backend/internal/config/cooldown.go:146-165` (delete `IpMaxReadmits`, `IpJitterRatio` + constants)
- Modify: `backend/internal/server/engine_attempt.go:379-391` (delete the `CooldownIpCapped` chat path; surface directly instead)
- Test: update `spill_excise_correlative_test.go` (add the cooldown-not-written subtest)

**Interfaces:**
- Consumes: E1 invariant (ip_capped surfaces directly, no `continue`).
- Produces: `runs.RunManager` without the `CooldownIpCapped` method; `config.Config` without `IpMaxReadmits()/IpJitterRatio()` — E4 and I5 assume this API is already gone.

- [ ] **Step 1: write the anti-cooldown subtest**

```go
func TestExciseIpCappedWritesNoCooldown(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"ip_capped","retryAfterMs":60000}`))
	}
	p := newTestPoolCfg(t, func(c *config.Config) {}, mock0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = p.Acquire(ctx, modelA)
	tok := (*p.roster.Load())[0]
	if until := tok.runs.CooldownUntil(); time.Now().Before(until) {
		t.Fatalf("ip_capped wrote cooldown until %s, want none", until.Format(time.RFC3339))
	}
	if ice := tok.runs.IpCappedError(); ice != nil {
		t.Fatalf("ip_capped remembered %v, want nil", ice)
	}
}
```

- [ ] **Step 2: run, expect FAIL** (cooldown written + jitter + readmit counter)

Run: `go test ./backend/internal/pool/ -run TestExciseIpCappedWritesNoCooldown -count=1`
Expected: FAIL at `wrote cooldown`.

- [ ] **Step 3: delete the implementation**

Delete `CooldownTokenIpCapped` (`pool/cooldown.go:45-51`), the ip_capped branch in `classifyAndCooldown` (`:267-398`, including the ±20% jitter and the day-3 lock), the runs methods + `IPMaxReadmits`/jitter constants (`runs/cooldown.go`), the config accessors (`config/cooldown.go:151-165`). In `engine_attempt.go:379-391`, change the branch body to:

```go
case errors.Is(err, upstream.ErrIpCapped):
    release()
    return nil, nil, err
```

- [ ] **Step 4: run, expect PASS** + remove leftover references via the compiler

Run: `go test ./backend/internal/pool/ -run 'TestExciseCorrelativeNoWalk|TestExciseIpCappedWritesNoCooldown' -count=1`
Expected: PASS. Remaining `CooldownIpCapped`/`IpMaxReadmits` references (old tests) are also deleted in this task — the compiler is the list.

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/cooldown.go backend/internal/runs/cooldown.go backend/internal/config/cooldown.go backend/internal/server/engine_attempt.go backend/internal/pool/spill_excise_correlative_test.go
git commit -m "feat(pool): drop per-token ip_capped cooldown, surface natural 429"
```

### Task E3: Delete the global 5m unfit registry

**Files:**
- Delete: `backend/internal/pool/unfit.go`, `backend/internal/pool/unfit_test.go`
- Modify: `backend/internal/pool/acquire_route.go:458-474` (leftover `c.limitedIp` branch not yet cut in E1 — surface directly)
- Modify: `backend/internal/server/engine_attempt.go:220-229,254-271` (delete `MarkModelUnfit` + `ClearModelUnfitBefore` on success and in the limited_ip branch)
- Test: `spill_excise_correlative_test.go` (limited_ip no-walk + no-mark subtest)

**Interfaces:**
- Consumes: invarian E1.
- Produces: no `MarkModelUnfit`/`ModelUnfit`/`ClearModelUnfit*` symbols anywhere in the repo — I5 and C1 assume this registry is gone.

- [ ] **Step 1: write the limited_ip subtest**

```go
func TestExciseLimitedIPNoWalkNoMark(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"session_model_mismatch","message":"model limited on this egress ip"}`))
	}
	p := newTestPoolCfg(t, func(c *config.Config) {}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := p.Acquire(ctx, modelA)
	if err == nil || !errors.Is(err, upstream.ErrModelIPLimited) {
		t.Fatalf("want ErrModelIPLimited, got %v", err)
	}
	if n := mock1.RequestsSnapshot(); n != 0 {
		t.Fatalf("walked to account #2 (%d requests), want 0", n)
	}
}
```

The body uses the `limited` marker so the `classify.go:143-151` classification yields `ErrModelIPLimited`; if the mock scripts via `SessionSequence`, swap the body for the equivalent sequence mode and keep the same assertions.

- [ ] **Step 2: run, expect FAIL** (walk and/or 5m unfit mark)

Run: `go test ./backend/internal/pool/ -run TestExciseLimitedIPNoWalkNoMark -count=1`
Expected: FAIL.

- [ ] **Step 3: delete the registry + call sites**

Delete `unfit.go`/`unfit_test.go`. In `acquire_route.go:458-474`, change the `c.limitedIp` branch to:

```go
if lie := c.limitedIp; lie != nil {
    lie.Model = model
    errs = append(errs, fmt.Sprintf("%s: %v", name, err))
    routeSlot.Release()
    return nil, err
}
```

In `engine_attempt.go`, delete the `ClearModelUnfitBefore` block (`:227-229`) and collapse the whole `ErrModelIPLimited` branch (`:254-271`) to:

```go
case errors.Is(err, upstream.ErrModelIPLimited):
    release()
    return nil, nil, err
```

- [ ] **Step 4: run, expect PASS**

Run: `go test ./backend/internal/pool/ -run 'TestExciseLimitedIPNoWalkNoMark|TestExciseCorrelativeNoWalk' -count=1`
Expected: PASS. `grep -r MarkModelUnfit backend/` must be empty.

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/acquire_route.go backend/internal/server/engine_attempt.go backend/internal/pool/spill_excise_correlative_test.go
git rm -q backend/internal/pool/unfit.go backend/internal/pool/unfit_test.go
git commit -m "feat(pool): delete global model-unfit registry, surface limited_ip"
```

### Task E4: Delete bounded COOLDOWN_* + ceiling (RetryAfter passthrough); keep ban quarantine

**Files:**
- Modify: `backend/internal/upstream/classify.go:260-308` (delete the Fanout/InvalidModel/LoadShed/PeakHours/Opaque constants + usages; upstream RetryAfter is passed through as-is)
- Modify: `backend/internal/config/cooldown.go` (delete the whole file if only dead accessors remain; if `DefaultMs`/`CountryBlockMs` are still used, delete their callers first in this task)
- Modify: `backend/internal/runs/cooldown.go:17-36` (delete `DefaultCooldown`, countryBlock, ceiling on the runs side)
- Modify: `backend/internal/pool/cooldown.go:267-398` (simplify `classifyAndCooldown`: 401/ban/rate-limit passthrough; country-block → surface like the E1 correlatives)
- Modify: `backend/internal/server/engine_attempt.go:345-355,392-401` (auth + country: surface with no cooldown write)
- Test: `spill_excise_cooldown_test.go` (throwaway: opaque 429 without RetryAfter writes no cooldown; ban still quarantines)

**Interfaces:**
- Consumes: invarian E1–E3.
- Produces: `classifyAndCooldown` with no `authRejected`-cooldown field (401 = surface), no cooldown writes for non-quota 429s; the only remaining per-account state: ban/suspend quarantine + slot ledger + precious set. I5 assumes there is no `CooldownRateLimit` for quota 429s.

- [ ] **Step 1: write the passthrough throwaway test**

```go
func TestExciseOpaque429WritesNoCooldown(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"some_future_code"}`))
	}
	p := newTestPoolCfg(t, func(c *config.Config) {}, mock0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = p.Acquire(ctx, modelA)
	tok := (*p.roster.Load())[0]
	if until := tok.runs.CooldownUntil(); time.Now().Before(until) {
		t.Fatalf("opaque 429 wrote cooldown until %s, want passthrough", until.Format(time.RFC3339))
	}
}

func TestExciseBanStillQuarantined(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock0.SetBan(true)
	p := newTestPoolCfg(t, func(c *config.Config) {}, mock0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := p.Acquire(ctx, modelA)
	if err == nil || !errors.Is(err, upstream.ErrBanned) {
		t.Fatalf("want ErrBanned, got %v", err)
	}
	if q := (*p.roster.Load())[0].quarantine.Load(); q == nil {
		t.Fatal("ban did not quarantine, want terminal quarantine")
	}
}
```

- [ ] **Step 2: run, expect partial FAIL** (opaque writes a 60s cooldown via `COOLDOWN_OPAQUE_MS`)

Run: `go test ./backend/internal/pool/ -run 'TestExciseOpaque429WritesNoCooldown|TestExciseBanStillQuarantined' -count=1`
Expected: FAIL on opaque; ban PASS (keeper).

- [ ] **Step 3: delete the bounded backoff, keep the ban**

Delete the bounded constants in `classify.go:260-308` (Fanout 60s, InvalidModel 60s, Opaque 60s, LoadShed 90s, PeakHours 30m) and the 7d ceiling clamp: upstream RetryAfter/ResetAt pass through unclamped. Delete `config/cooldown.go` entirely + its callers, `runs/cooldown.go:17-36` (except `CooldownBan` + the quarantine path `pool/cooldown.go:56-71,400-449`). 401 (`engine_attempt.go:345-348`, `acquire_route.go:407-413`) and country-block (`:392-401`, `pool/cooldown.go:78-84`) become surface-without-cooldown like E1. Ban/suspend quarantine + `clearLiftedQuarantine` stay FULLY KEPT.

- [ ] **Step 4: run, expect PASS**

Run: `go test ./backend/internal/pool/ -run 'TestExciseOpaque429WritesNoCooldown|TestExciseBanStillQuarantined' -count=1`
Expected: PASS. `grep -rn COOLDOWN_ backend/internal --include='*.go' | grep -v _test` must be empty (old tests that fail compilation are migrated in this task).

- [ ] **Step 5: commit**

```bash
git add backend/internal/upstream/classify.go backend/internal/pool/cooldown.go backend/internal/runs/cooldown.go backend/internal/server/engine_attempt.go backend/internal/pool/spill_excise_cooldown_test.go
git rm -q backend/internal/config/cooldown.go
git commit -m "feat(pool): delete bounded cooldowns, passthrough upstream RetryAfter, keep ban quarantine"
```

### Task E5: Delete proactive probe/maturity

**Files:**
- Delete: `backend/internal/pool/quota_smartprobe.go` (+ 4 test), `quota_visitprobe.go` (+ test), `quota_bootseed.go` (+ test), `maturity.go` (+ 7 test)
- Modify: `backend/internal/pool/pool.go` (delete the scheduler wiring: `probeCtx`, `maintain` fields, `StreakHits`/probe triggers; keep `wg sync.WaitGroup` for other shutdowns if still used)
- Modify: `backend/internal/config/config_keys.go:196-200` + `keycatalog.go` + `frontend` copy (delete `MATURITY_*`, `QUOTA_PROBE_*`, `MATURITY_TOUCH_MODEL`, `QuotaAutoProbe`)
- Test: no behavioral throwaway; verification = `SessionProbesSnapshot()==0` + `StreakHitsSnapshot()==0` after traffic (add to `spill_excise_cooldown_test.go` as a subtest, keeper in C4)

**Interfaces:**
- Consumes: none (independent of E1–E4; may run in parallel).
- Produces: zero upstream contact outside traffic + on-demand re-admit; `testutil.MockUpstream.SessionProbesSnapshot/StreakHitsSnapshot` (`mockupstream.go:745,760`) becomes the zero-probe oracle for every task.

- [ ] **Step 1: write the zero-probe subtest**

```go
func TestExciseNoProactiveContact(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	p := newTestPoolCfg(t, func(c *config.Config) {}, mock0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	l, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	p.LeaseRelease(l)
	if n := mock0.SessionProbesSnapshot(); n != 0 {
		t.Fatalf("%d token-level probes, want 0", n)
	}
	if n := mock0.StreakHitsSnapshot(); n != 0 {
		t.Fatalf("%d streak hits, want 0", n)
	}
}
```

- [ ] **Step 2: run, expect FAIL** (probe/maturity touches the mock)

Run: `go test ./backend/internal/pool/ -run TestExciseNoProactiveContact -count=1`
Expected: FAIL (probe and/or streak > 0).

- [ ] **Step 3: delete the scheduler + files**

Delete the 4 source files + 13 test files above. Unwire in `pool.go` (maintain goroutine, probeCtx, streak trigger). Delete the `MATURITY_ENABLED`, `MATURITY_BACKOFF_MS`, `MATURITY_TOUCH_MODEL`, `QUOTA_AUTO_PROBE`, `QUOTA_PROBE_ACTIVE_INTERVAL`, `QUOTA_PROBE_IDLE_HEARTBEAT`, `SMART_PROBE_BACKOFF_MAX_MS` knobs from `config_keys.go` + keycatalog + UI copy.

- [ ] **Step 4: run, expect PASS**

Run: `go test ./backend/internal/pool/ -run TestExciseNoProactiveContact -count=1`
Expected: PASS.

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/pool.go backend/internal/config/config_keys.go backend/internal/pool/spill_excise_cooldown_test.go
git rm -q backend/internal/pool/quota_smartprobe.go backend/internal/pool/quota_smartprobe_test.go backend/internal/pool/quota_smartprobe_fleet_test.go backend/internal/pool/quota_autoprobe_test.go backend/internal/pool/smartprobe_persist_test.go backend/internal/pool/quota_visitprobe.go backend/internal/pool/quota_visitprobe_test.go backend/internal/pool/quota_bootseed.go backend/internal/pool/quota_bootseed_test.go backend/internal/pool/maturity.go backend/internal/pool/maturity_test.go backend/internal/pool/maturity_auto_test.go backend/internal/pool/maturity_firegate_test.go backend/internal/pool/maturity_persist_test.go backend/internal/pool/maturity_resultday_test.go backend/internal/pool/maturity_skip_loop_test.go backend/internal/pool/maturity_touch_test.go
git commit -m "feat(pool): delete proactive probes and maturity touches"
```

### Task E6: Delete scorer/rotation/leader-election (interim index order)

**Files:**
- Modify: `backend/internal/pool/route_smart.go:56-85,333-365,389-569` (delete weights, transient penalty, stick/overflow helpers, `routeScore`, `routeSmartRank`; KEEP `routeSlotState`/`routeSlotPermit`/`routeSlotAcquire`/`routeSlotParams` for I1)
- Modify: `backend/internal/pool/acquire_order.go:60-205` (replace tier+rotation+demote with plain index order)
- Modify: `backend/internal/pool/acquire_route.go:56-121` (delete the leader gate + `rr` start; `start` is always 0)
- Delete: `backend/internal/pool/admission_leader_election_test.go`, `route_smart_test.go` (slot keeper replaces it in I1), `acquire_order_test.go`, `random_rotation_test.go`
- Test: throwaway `spill_order_strict_test.go`: cold concurrent burst → every lease is `Token==0` first; `SessionCreatesSnapshot` on mock0 == 1 (single-flight session manager, not a gate)

**Interfaces:**
- Consumes: E1 invariant (no correlative walk).
- Produces: interim `acquireOrder` flowing plain `[0..N)` index order; `routeSmartRank`/`routeScore`/`routeOverflowHelper`/`routeStickHolders` are gone — C1 replaces `acquireOrder` with the final `spill_order.go`.

- [ ] **Step 1: write the order-index + single-admit throwaway**

```go
func TestExcisePlainIndexOrder(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.TokenMaxConcurrent = 8
	}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	leases := make([]*Lease, 4)
	errs := make([]error, 4)
	for i := range leases {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			leases[i], errs[i] = p.Acquire(ctx, modelA)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		if leases[i].Token != 0 {
			t.Fatalf("worker %d leased account #%d, want #1 (index order)", i, leases[i].Token+1)
		}
		defer p.LeaseRelease(leases[i])
	}
	if n := mock0.SessionCreatesSnapshot(); n != 1 {
		t.Fatalf("account #1 admitted %d sessions, want 1 (no duplicate creates)", n)
	}
	if n := mock1.RequestsSnapshot(); n != 0 {
		t.Fatalf("account #2 touched (%d requests), want 0", n)
	}
}
```

- [ ] **Step 2: run, expect FAIL** (rr-start/lastUsed/scorer spreads to account #2)

Run: `go test ./backend/internal/pool/ -run TestExcisePlainIndexOrder -count=1`
Expected: FAIL at `leased account #2`.

- [ ] **Step 3: strip to index order**

In `acquire_order.go`: delete `lastTokenByModel`, `admissions`, hot/cold/mismatch tiers, Freebucks sort, the `TOKEN_ROTATION` switch, availability demote; return `[0..N)` filtered by `locked/quarantine/lockedOutByModel/capped` only. In `acquire_route.go`: delete the `modelAdmissionGate` gate + `p.rr.Add(1)` (start=0). In `route_smart.go`: delete scorer/stick/overflow/rank, keep the FIFO slot block `:99-322`.

- [ ] **Step 4: run, expect PASS**

Run: `go test ./backend/internal/pool/ -run TestExcisePlainIndexOrder -count=1`
Expected: PASS.

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/route_smart.go backend/internal/pool/acquire_order.go backend/internal/pool/acquire_route.go backend/internal/pool/spill_order_strict_test.go
git rm -q backend/internal/pool/admission_leader_election_test.go backend/internal/pool/route_smart_test.go backend/internal/pool/acquire_order_test.go backend/internal/pool/random_rotation_test.go
git commit -m "feat(pool): strip scorer, rotation and leader gate to plain index order"
```

### Task E7: Delete chat retry-once (single attempt, surface)

**Files:**
- Modify: `backend/internal/server/engine_attempt.go:139-479` (delete the re-acquire loop; every error branch = `release()` + surface; delete `transientErr`, the `attempts>1` budget)
- Modify: `backend/internal/config/config.go:91` + `config_keys.go` + keycatalog + UI (delete `RATE_LIMIT_FAILOVER`)
- Test: throwaway `spill_excise_nochatretry_test.go` in `backend/internal/server/`: one chat 429 → `SessionCreatesSnapshot` does not grow (no second re-admit)

**Interfaces:**
- Consumes: `backend.CooldownRateLimit/CooldownIpCapped/CooldownBan` as slimmed down by E2/E4 (call only what remains; the 429 branch writes NO cooldown after E4/I5).
- Produces: single-attempt `chatCore`; I5 + C1 assume no re-acquire on the chat path.

- [ ] **Step 1: write the anti-retry throwaway**

```go
func TestExciseChatSingleAttempt(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	mock0.ChatStatus = 429
	mock0.ChatErrorBody = `{"error":"rate_limited","retryAfterMs":60000}`
	p := newTestPoolCfg(t, func(c *config.Config) {}, mock0, mock1)
	before := mock0.SessionCreatesSnapshot() + mock1.SessionCreatesSnapshot()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	l, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer p.LeaseRelease(l)
	_ = doChatOnce(ctx, p, l, modelA)
	after := mock0.SessionCreatesSnapshot() + mock1.SessionCreatesSnapshot()
	if after != before {
		t.Fatalf("chat refusal re-admitted %d sessions, want 0 (single attempt)", after-before)
	}
}
```

`doChatOnce` is a test helper that calls one `backend.Chat` with no loop (write inline in the test file, 10 lines, using the engine constructor the existing server tests already use — do NOT copy the `attempts` pattern; a single direct call).

- [ ] **Step 2: run, expect FAIL** (retry-once adds 1 admission on another token)

Run: `go test ./backend/internal/server/ -run TestExciseChatSingleAttempt -count=1`
Expected: FAIL at `re-admitted 1`.

- [ ] **Step 3: cut the loop**

Change the `for {` at `engine_attempt.go:213` into a single `backend.Chat` call; every `case` branch at `:253-429` ends with `release(); return nil, nil, err`; delete the re-acquire block `:430-478` + `transientErr`. Delete the `RATE_LIMIT_FAILOVER` knob at every layer.

- [ ] **Step 4: run, expect PASS**

Run: `go test ./backend/internal/server/ -run TestExciseChatSingleAttempt -count=1`
Expected: PASS.

- [ ] **Step 5: commit**

```bash
git add backend/internal/server/engine_attempt.go backend/internal/config/config.go backend/internal/config/config_keys.go backend/internal/server/spill_excise_nochatretry_test.go
git commit -m "feat(server): single-attempt chat, delete retry-once and RATE_LIMIT_FAILOVER"
```

### Task E8: Delete park/sweep (sessions live until upstream ends them)

**Files:**
- Delete: `backend/internal/session/session_park.go`
- Modify: `backend/internal/pool/pool_lifecycle.go:237-265` (delete the `SESSION_IDLE_END` sweep), `:40-49` (hardcode poll pacing 20s base / 5m cap)
- Modify: `backend/internal/pool/lifecycle.go:119-203` (delete the drop-on-cooldown path; `Invalidate*` only for upstream corpses: invalid/expired/superseded/428-required)
- Modify: `backend/internal/pool/cooldown.go:54-68` (the `SessionParkThresholdMs` wiring in `cooldown_tuning.go` — delete `cooldown_tuning.go` + test)
- Modify: config + keycatalog + UI (delete `SESSION_PARK_ENABLED`, `SESSION_PARK_THRESHOLD_MS`, `SESSION_POLL_MAX_MS`, `SESSION_IDLE_END`)
- Test: throwaway: a short cooldown does not drop the session (`Snapshot().Usable()` stays true; `SessionEndsSnapshot()==0`)

**Interfaces:**
- Consumes: none (parallel with E1–E7).
- Produces: interim precious invariant "no local drops"; I3 builds `precious.go` on top of it.

- [ ] **Step 1: write the anti-drop throwaway**

```go
func TestExciseShortCooldownKeepsSession(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	p := newTestPoolCfg(t, func(c *config.Config) {}, mock0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	l, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	p.LeaseRelease(l)
	tok := (*p.roster.Load())[0]
	tok.runs.Cooldown(5 * time.Minute)
	p.MaintainOnce(ctx)
	if n := mock0.SessionEndsSnapshot(); n != 0 {
		t.Fatalf("short cooldown ended %d sessions, want 0", n)
	}
	if !tok.session.Snapshot().Usable() {
		t.Fatal("session not usable after short cooldown, want usable")
	}
}
```

`MaintainOnce` is one synchronous maintain tick that already exists (if it is named differently in `pool_lifecycle.go`, use the actual name — do NOT create a new helper).

- [ ] **Step 2: run, expect FAIL** (park-threshold/drop ends the session)

Run: `go test ./backend/internal/pool/ -run TestExciseShortCooldownKeepsSession -count=1`
Expected: FAIL.

- [ ] **Step 3: delete park/sweep/drop**

Delete `session_park.go`, `cooldown_tuning.go`(+test), the `shouldPark`/drop branches in `lifecycle.go:119-203`, the `SESSION_IDLE_END` sweep (`pool_lifecycle.go:237-265`), hardcode poll pacing, delete the 4 knobs at every layer.

- [ ] **Step 4: run, expect PASS**

Run: `go test ./backend/internal/pool/ -run TestExciseShortCooldownKeepsSession -count=1`
Expected: PASS.

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/pool_lifecycle.go backend/internal/pool/lifecycle.go backend/internal/pool/spill_excise_park_test.go
git rm -q backend/internal/session/session_park.go backend/internal/pool/cooldown_tuning.go backend/internal/pool/cooldown_tuning_test.go backend/internal/pool/cooldown_session_survive_test.go
git commit -m "feat(pool): delete park-vs-drop and idle sweep, sessions survive cooldowns"
```

---

### Task I1: Slot ledger, 2 per (account, model) (R2)

**Files:**
- Create: `backend/internal/pool/slot_ledger.go` (moved `routeSlotState`, `routeSlotWaiter`, `routeSlotPermit`, `routeSlotAcquire`, `routeSlotLive`, `routeSlotQueued`, `routeSlotStats` from `route_smart.go:99-322`, renamed `slot*`; ledger keyed per (token, model) — `map[token]map[model]slotState` or equivalent, FIFO per (account, model); `routeSlotParams` → `slotParams` reads `SLOTS_PER_ACCOUNT` as the per-(account, model) cap)
- Modify: `backend/internal/pool/route_smart.go` (delete the moved block; this file is deleted entirely in C1 if nothing remains — note it)
- Modify: `backend/internal/config/config.go` + `config_keys.go:202` + `config_load.go` + keycatalog + UI copy (rename `TOKEN_MAX_CONCURRENT` → `SLOTS_PER_ACCOUNT`, default 2, floor 1; `0` = unlimited as in `route_smart.go:124-137` today)
- Modify: `backend/internal/pool/acquire_route.go:295-355` (use `slotAcquire`; delete overflow-assist `:304-336`, keep the queue-exhausted mapping for I2)
- Test: keeper `backend/internal/pool/slot_ledger_test.go`: cap 2 per (account, model) → the 3rd SAME-model lease parks (`QueueWait>0` after release) + model-isolation sub-case (2×modelA + 2×modelB on account #1 = 4 running, account #2 zero contact), `routeQueueHint`/`Retry-After 1s` DELETED — waiters receive no local 429 (see I2)

**Interfaces:**
- Consumes: `config.Config.SlotsPerAccount int` (per-(account, model) cap), `QueueDepth int`, `QueueWait time.Duration` (resolved, zero-tolerant like today’s loader).
- Produces:
```go
func (p *Pool) slotAcquire(ctx context.Context, key slotKey, displayIdx int, cap, depth int, wait time.Duration) (*slotPermit, bool, error)
func (s *slotPermit) Release()
func (p *Pool) slotLive(key slotKey) int
type slotQueueExhaustedError struct { Reason string; Token, Cap, Live int; Wait time.Duration }
```
`slotKey` covers (token, model) — slots and FIFO keyed per (account, model).
I2 uses `slotAcquire` + `slotQueueExhaustedError` for spill; C1 deletes the rest of `route_smart.go`.

- [ ] **Step 1: write the cap-2-park + model-isolation keeper (R2)**

```go
func TestSlotLedgerTwoSlotsThirdParks(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.TokenMaxConcurrent = 2
		c.QueueWait = 2 * time.Second
		c.QueueDepth = 16
	}, mock0)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	m1, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("lease 1: %v", err)
	}
	defer p.LeaseRelease(m1)
	m2, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("lease 2: %v", err)
	}
	defer p.LeaseRelease(m2)
	done := make(chan *Lease, 1)
	go func() {
		l, err := p.Acquire(ctx, modelA)
		if err != nil {
			return
		}
		done <- l
	}()
	select {
	case <-done:
		t.Fatal("lease 3 granted while 2 slots held, want parked")
	case <-time.After(300 * time.Millisecond):
	}
	p.LeaseRelease(m1)
	select {
	case l3 := <-done:
		if l3.QueueWait <= 0 {
			t.Fatalf("lease 3 QueueWait=%v, want >0 (parked FIFO)", l3.QueueWait)
		}
		if l3.Token != 0 {
			t.Fatalf("lease 3 on account #%d, want #1", l3.Token+1)
		}
		p.LeaseRelease(l3)
	case <-time.After(5 * time.Second):
		t.Fatal("lease 3 never granted after slot freed")
	}
}
```

Model-isolation sub-case (other-model slots stay untouched):

```go
func TestSlotLedgerPerModelIsolation(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueWait = 2 * time.Second
		c.QueueDepth = 16
	}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var held []*Lease
	defer func() {
		for _, l := range held {
			p.LeaseRelease(l)
		}
	}()
	for i := 0; i < 2; i++ {
		l, err := p.Acquire(ctx, modelA)
		if err != nil {
			t.Fatalf("modelA lease %d: %v", i, err)
		}
		held = append(held, l)
	}
	for i := 0; i < 2; i++ {
		l, err := p.Acquire(ctx, modelB)
		if err != nil {
			t.Fatalf("modelB lease %d: %v", i, err)
		}
		held = append(held, l)
	}
	for i, l := range held {
		if l.Token != 0 {
			t.Fatalf("lease %d on account #%d, want #1 (2xA + 2xB share one account)", i, l.Token+1)
		}
		if l.QueueWait != 0 {
			t.Fatalf("lease %d parked (%v), want zero parks (per-model slots free)", i, l.QueueWait)
		}
	}
	if n := mock1.RequestsSnapshot(); n != 0 {
		t.Fatalf("account #2 touched (%d requests), want 0", n)
	}

(The `TokenMaxConcurrent`/`QueueWait`/`QueueDepth` config field names become `SlotsPerAccount`/etc. in this task’s rename step too.)

- [ ] **Step 2: run, expect PASS after the move** (cap-2 behavior already exists; model isolation FAILs until the ledger is keyed per (token, model); this task is rename + file isolation)

Run: `go test ./backend/internal/pool/ -run 'TestSlotLedgerTwoSlotsThirdParks|TestSlotLedgerPerModelIsolation' -count=1`
Expected: PASS after the move (compile FAIL before the field rename — that is the rename list; isolation FAIL before per-model keying — that is the keying list).

- [ ] **Step 3: pindah + rename + key per model**

Move the FIFO block to `slot_ledger.go` with the renames above, keyed `map[token]map[model]slotState` or equivalent (FIFO per (account, model)); rename the knobs in config/load/keycatalog/UI copy — but not `poolStrategy` (not a strategy key); `acquire_route.go` uses `slotAcquire` keyed by (token, model), delete overflow-assist.

- [ ] **Step 4: run the keepers + reproduce the R2 numbers**

Run: `go test ./backend/internal/pool/ -run 'TestSlotLedgerTwoSlotsThirdParks|TestSlotLedgerPerModelIsolation' -count=1 -v`
Expected: PASS. **Repro R2 (3-req-1-account-same-model):** 3 SAME-model concurrent requests to 1 account (cap 2 per (account, model)) → 2 leases `Token==0, QueueWait==0` + 1 parked, then `QueueWait>0` after a slot frees; `SessionCreatesSnapshot()` on mock0 == 1 (one shared precious session, not 3 admissions). **Model isolation:** 2×modelA + 2×modelB on account #1 = 4 running with no queue/spill, account #2 zero contact.

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/slot_ledger.go backend/internal/pool/slot_ledger_test.go backend/internal/pool/route_smart.go backend/internal/pool/acquire_route.go backend/internal/config/
git commit -m "feat(pool): slot ledger SLOTS_PER_ACCOUNT=2 per account-model"
```

### Task I2: Ordered lanes + spill-on-wait-expiry (R3)

**Files:**
- Create: `backend/internal/pool/spill_queue.go` (per-model lane: `lanes[model] = [akun...]` in pin/liveness-filtered index order; waiters park on the head lane; total parking > QUEUE_WAIT → move to the next lane (`spillCount++`); `MAX_SPILL_ACCOUNTS>0` caps it; no next lane → surface the `slotQueueExhaustedError` timeout)
- Modify: `backend/internal/pool/acquire_route.go:149-355` (`leaseFromOrder` becomes the lane driver: try the head lane → `slotAcquire` → timeout/full → next lane)
- Modify: `backend/internal/config/*` (add `MAX_SPILL_ACCOUNTS`, default 0 = unbounded)
- Test: keeper `spill_queue_test.go`: burst-5-2-accounts (below)

**Interfaces:**
- Consumes: `slotAcquire/slotPermit/slotQueueExhaustedError` (I1); `spillOrder(model) []int`, interim = E6 index order (final in C1); `config.MaxSpillAccounts int`, `config.QueueWait time.Duration`.
- Produces:
```go
func (p *Pool) spillLane(model string) []int
func (p *Pool) acquireSpill(ctx context.Context, model, agentID string, cfg *config.Config, toks *[]*tokenEntry) (*Lease, error)
```
C1 uses `spillLane`; I5 uses `acquireSpill` as the requeue point.

- [ ] **Step 1: write the burst-5-2-accounts keeper (R3)**

```go
func TestSpillBurst5TwoAccounts(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	mock0.ChatBlocks = true
	mock1.ChatBlocks = true
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueWait = 300 * time.Millisecond
		c.QueueDepth = 16
	}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	leases := make([]*Lease, 5)
	for i := range leases {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l, err := p.Acquire(ctx, modelA)
			if err != nil {
				t.Errorf("worker %d: %v", i, err)
				return
			}
			leases[i] = l
		}(i)
	}
	wg.Wait()
	defer func() {
		for _, l := range leases {
			p.LeaseRelease(l)
		}
	}()
	on0, on1 := 0, 0
	for i, l := range leases {
		if l == nil {
			t.Fatalf("worker %d: no lease", i)
		}
		switch l.Token {
		case 0:
			on0++
		case 1:
			on1++
		default:
			t.Fatalf("worker %d on account #%d, want #1/#2", i, l.Token+1)
		}
	}
	if on0 != 2 || on1 != 2 {
		t.Fatalf("split #%d/#%d, want 2/2 with 1 queued", on0, on1)
	}
	queued := 0
	for _, l := range leases {
		if l.QueueWait > 0 {
			queued++
		}
	}
	if queued != 1 {
		t.Fatalf("%d parked leases, want 1 (2+2 run + 1 antre)", queued)
	}
}
```

(`ChatBlocks` holds the turn so slots are full when the burst lands; if the field has another name in `mockupstream.go`, use the actual name — the field is at `:105`.)

- [ ] **Step 2: run, expect FAIL** (without spill: the 5th waiter gets a timeout-429 on account #1, not a grant on #2)

Run: `go test ./backend/internal/pool/ -run TestSpillBurst5TwoAccounts -count=1`
Expected: FAIL (`worker 4: pool: token-1 live-turn queue wait...`).

- [ ] **Step 3: implementasi lane + spill**

In `spill_queue.go`: `spillLane` = filtered index order (locked/quarantine/pin). `acquireSpill`: for each lane in order: `slotAcquire` with the remaining `QUEUE_WAIT`; `slotQueueExhaustedError` timeout/full → move to the next lane (`spillCount++`, check `MAX_SPILL_ACCOUNTS`); grant → admission as in `acquire_route.go:357-404` (no walk: non-quota admission failure → surface; quota → I5). Delete the timeout→429-rate-limit mapping + the old `continue` at `:320-341`.

- [ ] **Step 4: run, expect PASS — repro R3**

Run: `go test ./backend/internal/pool/ -run TestSpillBurst5TwoAccounts -count=1 -v`
Expected: PASS. **Repro R3 (burst-5-2-accounts):** 5 concurrent, 2 accounts cap 2 → 2+2 running + 1 queued; account #2 is touched ONLY after the waiter exhausts QUEUE_WAIT on #1 (prove it: with `QUEUE_WAIT=30s` + instant turns, burst-5 finishes with no `mock1.SessionCreatesSnapshot()>0` when #1 slots suffice — the second subtest uses a short wait + ChatBlocks to force spill; write both).

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/spill_queue.go backend/internal/pool/spill_queue_test.go backend/internal/pool/acquire_route.go backend/internal/config/
git commit -m "feat(pool): ordered lanes with spill on queue-wait expiry"
```

### Task I3: Precious open-set (R4)

**Files:**
- Create: `backend/internal/pool/precious.go` (set of `{(token, model)}` with live sessions; `preciousAdd/preciousHas/preciousRemove`; the `mustKeepSession` guard used by every `Invalidate*/EndSession` path)
- Modify: `backend/internal/pool/lifecycle.go:125-200` (`Invalidate*` refuses to delete a precious that is `Usable()` except for upstream corpses: `superseded`, `session-invalid`, `expired`, `waiting_room_required`)
- Modify: `backend/internal/pool/pool.go` (snapshot: precious label per slot for the dashboard)
- Test: keeper `precious_test.go`: N=2 accounts × same model → 2N=4 slots; fill 4 leases; pressure (manual cooldown + maintain tick + 5th request) → `SessionEndsSnapshot()==0` on both mocks, 4 sessions `Usable()`

**Interfaces:**
- Consumes: E8 invariant (no local drops); `Lease{Token, Model}` (`pool.go:74-84`).
- Produces:
```go
func (p *Pool) preciousAdd(token int, model string)
func (p *Pool) preciousHas(token int, model string) bool
func (p *Pool) preciousRemove(token int, model string)
```
C1 uses `preciousHas` to keep the holder at the head order; C3 displays the label.

- [ ] **Step 1: write the 2N-slot precious keeper (R4)**

```go
func TestPreciousTwoAccountsSameModel(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueWait = 5 * time.Second
		c.QueueDepth = 16
	}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var held []*Lease
	for i := 0; i < 4; i++ {
		l, err := p.Acquire(ctx, modelA)
		if err != nil {
			t.Fatalf("lease %d: %v", i, err)
		}
		held = append(held, l)
	}
	defer func() {
		for _, l := range held {
			p.LeaseRelease(l)
		}
	}()
	for i, tok := range *p.roster.Load() {
		tok.runs.Cooldown(5 * time.Minute)
		_ = i
	}
	p.MaintainOnce(ctx)
	done := make(chan error, 1)
	go func() {
		l, err := p.Acquire(ctx, modelA)
		if err != nil {
			done <- err
			return
		}
		p.LeaseRelease(l)
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("request 5: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("request 5 never served")
	}
	if n := mock0.SessionEndsSnapshot() + mock1.SessionEndsSnapshot(); n != 0 {
		t.Fatalf("%d sessions ended under pressure, want 0 (precious)", n)
	}
	for i, tok := range *p.roster.Load() {
		if !tok.session.Snapshot().Usable() {
			t.Fatalf("account #%d session not usable, want precious-kept", i+1)
		}
	}
}
```

- [ ] **Step 2: run, expect FAIL** (manual cooldown + maintain ends/drops the session)

Run: `go test ./backend/internal/pool/ -run TestPreciousTwoAccountsSameModel -count=1`
Expected: FAIL at `sessions ended` or `not usable`.

- [ ] **Step 3: implement the open-set + guard**

`precious.go` + the guard in every `Invalidate*/EndSession` in `lifecycle.go`: refuse when `preciousHas(token, model)` and the snapshot is `Usable()` and the reason is not an upstream corpse (`superseded` may STILL drop: the row was seized by another instance — upstream ended it, not us; document in a comment).

- [ ] **Step 4: run, expect PASS — repro R4**

Run: `go test ./backend/internal/pool/ -run TestPreciousTwoAccountsSameModel -count=1 -v`
Expected: PASS. **Repro R4:** N=2 accounts × 1 model = 4 filled slots + the 5th request served from the queue; zero `SessionEnds`; both sessions stay `Usable()`.

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/precious.go backend/internal/pool/precious_test.go backend/internal/pool/lifecycle.go backend/internal/pool/pool.go
git commit -m "feat(pool): precious open-set, live sessions never dropped"
```

### Task I4: strict PIN_MODEL, 1 account = 1 model (R5)

**Files:**
- Create: `backend/internal/config/pin_model.go` (`parsePinModel`: `"0:z-ai/glm-5.2;1:modelB"` → `map[int]string`; reject: multi-model per slot, negative index, empty model, duplicate slot)
- Delete: `backend/internal/pool/model_locks.go` (+ `model_locks_test.go`)
- Modify: `backend/internal/pool/acquire_route.go:43-54,215-226` + `acquire_order.go:47-53` (replace `lockedOutByModel/allLockedOut` with `pinnedOut`)
- Modify: `backend/internal/config/config.go:92-97` (`ModelLocks map[int][]string` → `PinModel map[int]string`), `config_load.go:147,358,671`, `data.go:104-105,211-216`, `keycatalog.go:176-178`
- Modify: `frontend/src/lib/components/TokenDetailsDrawer.svelte:57-139,335-377` (single-pin editor; delete the multi add/unpin list → one select + clear)
- Test: keeper `pin_model_test.go` (parse + routing + pin-burst-5 below)

**Interfaces:**
- Consumes: `registry.Registry.AgentForModel` (known-model validation, `model_locks.go:18-42` pattern).
- Produces:
```go
func parsePinModel(value string) (map[int]string, error)
func pinnedOut(cfg *config.Config, idx int, model string) bool
func pinFailFastError(model string, slots int) error
```
C1 uses `pinnedOut` in `spill_order.go`; C3 uses `PinModel` for the UI.

- [ ] **Step 1: write the parse + pin-burst-5 keeper (R5)**

```go
func TestParsePinModelStrict(t *testing.T) {
	got, err := parsePinModel("0:z-ai/glm-5.2;1:mimo/mimo-v2.5")
	if err != nil || len(got) != 2 || got[0] != "z-ai/glm-5.2" {
		t.Fatalf("valid = %v, %v; want 2 pins", got, err)
	}
	for _, bad := range []string{"0:a,b", "x:a", "-1:a", "0:", "0:a;0:b", "banana"} {
		if _, err := parsePinModel(bad); err == nil {
			t.Errorf("parse %q succeeded, want error", bad)
		}
	}
}

func TestPinBurst5OneAccount(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueWait = 5 * time.Second
		c.QueueDepth = 16
		c.PinModel = map[int]string{0: modelA}
	}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	leases := make([]*Lease, 5)
	for i := range leases {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l, err := p.Acquire(ctx, modelA)
			if err != nil {
				t.Errorf("worker %d: %v", i, err)
				return
			}
			leases[i] = l
		}(i)
	}
	wg.Wait()
	defer func() {
		for _, l := range leases {
			p.LeaseRelease(l)
		}
	}()
	running, queued := 0, 0
	for i, l := range leases {
		if l == nil {
			t.Fatalf("worker %d: no lease", i)
		}
		if l.Token != 0 {
			t.Fatalf("worker %d on account #%d, want pinned #1", i, l.Token+1)
		}
		if l.QueueWait > 0 {
			queued++
		} else {
			running++
		}
	}
	if running != 2 || queued != 3 {
		t.Fatalf("running=%d queued=%d, want 2+3 (pin burst 5)", running, queued)
	}
	if n := mock1.RequestsSnapshot(); n != 0 {
		t.Fatalf("unpinned account touched (%d requests), want 0", n)
	}
}
```

Sub-case: pin takes no other-model slot (slots per (account, model)):

```go
func TestPinLeavesOtherModelSlotsFree(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueWait = 5 * time.Second
		c.QueueDepth = 16
		c.PinModel = map[int]string{0: modelA}
	}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var held []*Lease
	defer func() {
		for _, l := range held {
			p.LeaseRelease(l)
		}
	}()
	for i := 0; i < 2; i++ {
		l, err := p.Acquire(ctx, modelA)
		if err != nil {
			t.Fatalf("modelA lease %d: %v", i, err)
		}
		held = append(held, l)
	}
	for i := 0; i < 2; i++ {
		l, err := p.Acquire(ctx, modelB)
		if err != nil {
			t.Fatalf("modelB lease %d: %v", i, err)
		}
		if l.Token != 1 {
			t.Fatalf("modelB lease %d on account #%d, want #2 (modelA pinned to #1)", i, l.Token+1)
		}
		held = append(held, l)
	}
	for i, l := range held {
		if l.QueueWait != 0 {
			t.Fatalf("lease %d parked (%v), want zero parks (pin takes no other-model slot)", i, l.QueueWait)
		}
	}

- [ ] **Step 2: run, expect compile FAIL** (`PinModel` does not exist yet; multi-`MODEL_LOCKS` still alive)

Run: `go test ./backend/internal/pool/ -run 'TestParsePinModelStrict|TestPinBurst5OneAccount|TestPinLeavesOtherModelSlotsFree' -count=1`
Expected: compile FAIL (unknown field) — that is the rename list.

- [ ] **Step 3: implement pin + delete locks**

Write `pin_model.go`, delete `model_locks.go`(+test), replace every `lockedOutByModel/allLockedOut/lockFailFastError` with the pin equivalent, migrate `concurrency_ladder_test.go:967-996,1102-1125` from `ModelLocks` to `PinModel`, delete `MODEL_LOCKS` in config/load/data/keycatalog + the old UI.

- [ ] **Step 4: run, expect PASS — repro R5**

Run: `go test ./backend/internal/pool/ -run 'TestParsePinModelStrict|TestPinBurst5OneAccount|TestPinLeavesOtherModelSlotsFree' -count=1 -v`
Expected: PASS. **Repro R5 (pin-burst-5):** account #1 pins modelA, burst 5 on modelA → 2 running + 3 queued ALL on #1; account #2 zero contact; a modelB request to this pool → `pinFailFastError` with no upstream contact. **Pin isolation:** pinning modelA on #1 takes no modelB slot on #1 — a mixed burst of 2×modelA + 2×modelB unpinned still gets each its own slots (modelA on #1, modelB on #2) with no queueing.

- [ ] **Step 5: commit**

```bash
git add backend/internal/config/pin_model.go backend/internal/config/config.go backend/internal/config/config_load.go backend/internal/config/data.go backend/internal/config/keycatalog.go backend/internal/pool/acquire_route.go backend/internal/pool/acquire_order.go backend/internal/pool/pin_model_test.go frontend/src/lib/components/TokenDetailsDrawer.svelte
git rm -q backend/internal/pool/model_locks.go backend/internal/pool/model_locks_test.go
git commit -m "feat(pool): strict PIN_MODEL 1 account 1 model, delete MODEL_LOCKS"
```

### Task I5: natural 429 = request re-queues (R6)

**Files:**
- Modify: `backend/internal/pool/spill_queue.go` (add per-waiter `notBefore`: admission/run-start quota 429 with RetryAfter → the waiter returns to the tail of THE SAME lane with `notBefore=now+retryAfter`; no `CooldownRateLimit` write, no failover, no spill charge)
- Modify: `backend/internal/pool/acquire_route.go:418-433,523-537` (delete the `CooldownTokenRateLimit` write + `appendRateLimitEntry` failover for quota 429s; forward `*upstream.RateLimitError` to the lane)
- Modify: `backend/internal/server/engine_attempt.go:356-378` (429 branch: no `CooldownRateLimit`, no failover; surface — the chat path does not re-queue because of single-attempt E7; re-queueing happens only in the admission lane)
- Test: keeper `spill_requeue_test.go`: mock0 `RateLimit=true` + `RateLimitRetryAfterMs=400`; acquire → eventually a `Token==0` lease after the window; `mock1.RequestsSnapshot()==0` (no account parking, no failover); empty token cooldown

**Interfaces:**
- Consumes: `acquireSpill` (I2); `*upstream.RateLimitError.RetryAfter` passthrough (E4); `MockUpstream{RateLimit, RateLimitRetryAfterMs, SetRateLimit}` (`mockupstream.go:123,131,720`).
- Produces: semantik "quota 429 = requeue + notBefore"; C1 assumes the lane waits for `notBefore` before the head retries (no upstream contact before then).

- [ ] **Step 1: write the requeue keeper (R6)**

```go
func TestNatural429RequeuesNoParkNoFailover(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	mock0.RateLimit = true
	mock0.RateLimitRetryAfterMs = 400
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueWait = 10 * time.Second
		c.QueueDepth = 16
	}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() {
		time.Sleep(1200 * time.Millisecond)
		mock0.SetRateLimit(false)
	}()
	l, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire: %v (want requeue-until-window, not surface)", err)
	}
	defer p.LeaseRelease(l)
	if l.Token != 0 {
		t.Fatalf("leased account #%d, want #1 (no failover on 429)", l.Token+1)
	}
	if n := mock1.RequestsSnapshot(); n != 0 {
		t.Fatalf("failed over to account #2 (%d requests), want 0", n)
	}
	if until := (*p.roster.Load())[0].runs.CooldownUntil(); time.Now().Before(until) {
		t.Fatalf("429 parked account until %s, want no park", until.Format(time.RFC3339))
	}
}
```

- [ ] **Step 2: run, expect FAIL** (429 today = cooldown write + failover to #2)

Run: `go test ./backend/internal/pool/ -run TestNatural429RequeuesNoParkNoFailover -count=1`
Expected: FAIL at `failed over` or direct surface.

- [ ] **Step 3: implementasi requeue**

The lane waiter carries `notBefore`; a lane head with a future `notBefore` does not attempt admission (no upstream contact) until it arrives; once the total `QUEUE_WAIT` runs out → spill as in I2 (a quota 429 that never clears can still spill — unlike the E1 correlatives that surface instantly). Delete the `CooldownTokenRateLimit` write on both the acquire and chat paths.

- [ ] **Step 4: run, expect PASS — repro R6**

Run: `go test ./backend/internal/pool/ -run TestNatural429RequeuesNoParkNoFailover -count=1 -v`
Expected: PASS. **Repro R6 (429-back-to-queue):** 400ms window → ≤3 re-admissions on #1 then a #1 lease; #2 zero contact; zero cooldown; terminal exception: `SetBan(true)` → quarantine + the request advances to lane #2 (second subtest, `errors.Is(err, upstream.ErrBanned)` when every account is banned).

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/spill_queue.go backend/internal/pool/spill_requeue_test.go backend/internal/pool/acquire_route.go backend/internal/server/engine_attempt.go
git commit -m "feat(pool): natural 429 requeues on same lane, no park no failover"
```

---

### Task C1: strict #1→#N order (R1)

**Files:**
- Create: `backend/internal/pool/spill_order.go` (`spillOrder(model) []int`: ascending `[0..N)` index filtered by locked/quarantine/`pinnedOut`/terminal; precious holders STAY at their index position — no re-rank, no head-boost)
- Delete: `backend/internal/pool/acquire_order.go` (+ `acquire_order_test.go`), the rest of `backend/internal/pool/route_smart.go` if it holds only slots (already moved in I1), plus `routeQueueHint` (`:90`) and the local-429 mapping
- Modify: `backend/internal/pool/spill_queue.go` (`spillLane` uses `spillOrder`), `concurrency_ladder_test.go` (delete rotation/overflow-assist expectations; the ladder becomes the strict-order verification), `queue_wait_test.go`, `queue_wait_matrix_test.go`, `route_bridge_slot_test.go` (bridge: slots only, no order — keep)
- Modify: config + keycatalog + UI (delete `TOKEN_ROTATION`, `ROUTING_SMART`)
- Test: keeper `spill_order_test.go`: 3 idle accounts → 10 sequential acquires ALL `Token==0` until full, then #2, then #3 (pure index drain; not round-robin)

**Interfaces:**
- Consumes: `pinnedOut` (I4); `preciousHas` (I3, for label/telemetry only — NOT for re-rank); `spillLane/acquireSpill` (I2).
- Produces:
```go
func (p *Pool) spillOrder(model string) []int
```
This order is the only ranking in the pool (R1); no other caller may re-sort.

- [ ] **Step 1: write the drain-index keeper (R1)**

```go
func TestStrictOrderDrainsAccountOneFirst(t *testing.T) {
	mocks := make([]*testutil.MockUpstream, 3)
	for i := range mocks {
		mocks[i] = testutil.NewMock()
		t.Cleanup(mocks[i].Close)
	}
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueWait = 300 * time.Millisecond
		c.QueueDepth = 16
	}, mocks[0], mocks[1], mocks[2])
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var held []*Lease
	defer func() {
		for _, l := range held {
			p.LeaseRelease(l)
		}
	}()
	for i := 0; i < 6; i++ {
		l, err := p.Acquire(ctx, modelA)
		if err != nil {
			t.Fatalf("lease %d: %v", i, err)
		}
		held = append(held, l)
	}
	for i, l := range held {
		want := i / 2
		if l.Token != want {
			t.Fatalf("lease %d on account #%d, want #%d (strict #1..#N)", i, l.Token+1, want+1)
		}
		if l.QueueWait != 0 {
			t.Fatalf("lease %d parked (%v), want zero parks (capacity free)", i, l.QueueWait)
		}
	}
}
```

- [ ] **Step 2: run, expect FAIL** (leftover rr/scorer/lastUsed spreads)

Run: `go test ./backend/internal/pool/ -run TestStrictOrderDrainsAccountOneFirst -count=1`
Expected: FAIL on the early leases (not #1 in order).

- [ ] **Step 3: lock the order**

Write `spill_order.go`, delete `acquire_order.go`(+test) and the rest of `route_smart.go` (make sure `slot_*` already moved in I1; delete `routeQueueHint` + the local-429 mapping — queue timeout is only an internal spill signal), delete the `TOKEN_ROTATION`/`ROUTING_SMART` knobs at every layer, migrate the ladder/queue/bridge tests.

- [ ] **Step 4: run, expect PASS — repro R1**

Run: `go test ./backend/internal/pool/ -run TestStrictOrderDrainsAccountOneFirst -count=1 -v`
Expected: PASS. **Repro R1:** 6 sequential acquires over 3 accounts → tokens `[0,0,1,1,2,2]`, zero parking; dashboard reorder (account swap) → order follows the new index (subtest: `RemoveLastToken`/reorder then repeat, the `pool_remove_test.go`/`pool_swap_test.go` pattern).

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/spill_order.go backend/internal/pool/spill_order_test.go backend/internal/pool/spill_queue.go backend/internal/pool/concurrency_ladder_test.go backend/internal/pool/queue_wait_test.go backend/internal/config/ frontend/src/lib/utils/poolStrategy.js
git rm -q backend/internal/pool/acquire_order.go backend/internal/pool/acquire_order_test.go backend/internal/pool/route_smart.go
git commit -m "feat(pool): strict account order 1..N, delete rotation and smart routing"
```

### Task C2: final knobs + strategy presets

**Files:**
- Modify: `backend/internal/config/keycatalog.go` (final 5-knob entries + descriptions; delete: `MODEL_LOCKS`, `TOKEN_ROTATION`, `ROUTING_SMART`, `RATE_LIMIT_FAILOVER`, `TOKEN_MAX_CONCURRENT`, `COOLDOWN_*`, `SESSION_PARK_*`, `SESSION_POLL_MAX_MS`, `SMART_PROBE_*`, `MATURITY_*`, `QUOTA_PROBE_*`)
- Modify: `backend/internal/config/config_validate.go` (cross-validation: `PIN_MODEL` vs existing slots; `MAX_SPILL_ACCOUNTS >= 0`; `SLOTS_PER_ACCOUNT >= 1` per (account, model) or 0=unlimited; `QUEUE_DEPTH >= 0`)
- Modify: `frontend/src/lib/utils/poolStrategy.js` (`STRATEGY_OWNED_KEYS` → 5 knob final; Drain = `QUEUE_WAIT 300s / QUEUE_DEPTH 1024`; Balance = 5–300s threshold defaulting to 60s as `QUEUE_WAIT` + depth 16)
- Modify: `frontend/src/lib/components/StrategyPresetCard.svelte` + `TrafficSettings.svelte` + `AdvancedSettings.svelte` (one editor per knob; delete dead rows)
- Test: `backend/internal/config/config_knobs_test.go` + `routing_knobs_test.go` (add 5 knobs; delete dead ones) + `keycatalog_test.go` (key set)

**Interfaces:**
- Consumes: `parsePinModel` (I4); `slotParams` (I1); `spillLane` (I2).
- Produces: final settings contract for C3; no new symbols.

- [ ] **Step 1: write the catalog test**

```go
func TestFinalKnobCatalog(t *testing.T) {
	for _, k := range []string{"SLOTS_PER_ACCOUNT", "QUEUE_WAIT", "QUEUE_DEPTH", "PIN_MODEL", "MAX_SPILL_ACCOUNTS"} {
		if !keycatalog.Has(k) {
			t.Errorf("knob %s missing from catalog", k)
		}
	}
	for _, k := range []string{"MODEL_LOCKS", "TOKEN_ROTATION", "ROUTING_SMART", "RATE_LIMIT_FAILOVER", "TOKEN_MAX_CONCURRENT", "COOLDOWN_DEFAULT_MS", "SESSION_PARK_ENABLED", "MATURITY_ENABLED", "QUOTA_AUTO_PROBE"} {
		if keycatalog.Has(k) {
			t.Errorf("dead knob %s still catalogued", k)
		}
	}
}
```

(Match the actual catalog accessor in `keycatalog.go` — do NOT create a new helper; if the catalog is a slice, iterate the slice.)

- [ ] **Step 2: run, expect FAIL** (dead keys still present, new keys missing)

Run: `go test ./backend/internal/config/ -run TestFinalKnobCatalog -count=1`
Expected: FAIL.

- [ ] **Step 3: finalisasi katalog + validasi + preset**

Delete/add entries, write the cross-validation, update `poolStrategy.js` owned keys + the Drain/Balance presets + the `StrategyPresetCard`/`TrafficSettings`/`AdvancedSettings` copy (one editor per knob, no duplicate writers).

- [ ] **Step 4: run, expect PASS**

Run: `go test ./backend/internal/config/ -run 'TestFinalKnobCatalog|TestConfig' -count=1`
Expected: PASS.

- [ ] **Step 5: commit**

```bash
git add backend/internal/config/keycatalog.go backend/internal/config/config_validate.go backend/internal/config/config_knobs_test.go frontend/src/lib/utils/poolStrategy.js frontend/src/lib/components/StrategyPresetCard.svelte frontend/src/lib/pages/TrafficSettings.svelte frontend/src/lib/pages/settings/AdvancedSettings.svelte
git commit -m "feat(config): final Ordered Queue-Spill knobs and strategy presets"
```

### Task C3: per-account PIN UI

**Files:**
- Modify: `frontend/src/lib/components/TokenDetailsDrawer.svelte:57-139,335-377` (one model select + Clear button → `PIN_MODEL` slot syntax via settings overlay POST; delete the multi-pin list)
- Modify: `frontend/src/lib/pages/Tokens.svelte:62-86` (concise chips: pinned model per account; delete `TOKEN_ROTATION`/`RATE_LIMIT_FAILOVER`/`ROUTING_SMART` references)
- Test: e2e `frontend/e2e/` pin test (the existing pin+unpin pattern per CurrentControl: switch to single-pin; assert `PIN_MODEL` value `"0:<model>"` + clear back to `""`)

**Interfaces:**
- Consumes: settings overlay POST (`adminApi.settingsSave`, the `TokenDetailsDrawer.svelte:105-111` pattern); the `allowed_models` snapshot becomes a single `pinned_model` (add the field on `TokenSnapshot` `pool.go:229-232` + `snapshot.go:200-202` in this task).
- Produces: no new symbols; e2e is the end-to-end proof of R5.

- [ ] **Step 1: write the single-pin e2e**

```ts
test("pin account to one model and clear", async ({ page }) => {
  await openTokenDrawer(page, 0);
  await selectPinModel(page, "z-ai/glm-5.2");
  await expectSettingsValue(page, "PIN_MODEL", "0:z-ai/glm-5.2");
  await clearPinModel(page);
  await expectSettingsValue(page, "PIN_MODEL", "");
});
```

(Helper names follow the existing e2e files — do NOT build a new framework; if a helper has another name, use the actual name with the same steps.)

- [ ] **Step 2: run, expect FAIL** (the old multi-lock UI does not write `PIN_MODEL`)

Run: `npm --prefix frontend run test:e2e -- pin` (the script name follows the actual `package.json`)
Expected: FAIL (key not found / wrong value).

- [ ] **Step 3: implement the editor + snapshot field**

One select + Clear in the drawer; `TokenSnapshot.pinned_model`; Tokens chips; delete dead knob copy.

- [ ] **Step 4: run, expect PASS**

Run: the same e2e script
Expected: PASS.

- [ ] **Step 5: commit**

```bash
git add frontend/src/lib/components/TokenDetailsDrawer.svelte frontend/src/lib/pages/Tokens.svelte backend/internal/pool/pool.go backend/internal/pool/snapshot.go frontend/e2e/
git commit -m "feat(dashboard): single-model PIN editor per account"
```

### Task C4: promote keepers + clean up + document behavior

**Files:**
- Modify: `backend/internal/pool/concurrency_ladder_test.go`, `queue_wait_test.go`, `queue_wait_matrix_test.go` (delete overflow-assist/rotation/cooldown expectations; make the ladder the R1–R3 verification)
- Keep (promoted from throwaway): `slot_ledger_test.go` (R2), `spill_queue_test.go` (R3), `precious_test.go` (R4), `pin_model_test.go` (R5), `spill_requeue_test.go` (R6), `spill_order_test.go` (R1), zero-probe subtest E5 + ban-keeper E4
- Delete: leftover unpromoted E1–E3/E7–E8 throwaways
- Modify: `frontend/src/lib/components/LiveConsole.svelte:179-183` (QUEUED copy → spill lane), keycatalog descriptions (behavior docs are the knob descriptions + tests)

**Interfaces:**
- Consumes: all E/I/C tasks.
- Produces: green suite at this revision; no new symbols.

- [ ] **Step 1: register the final keepers and delete leftover throwaways**

```bash
git status --short backend/internal/pool/*spill* backend/internal/pool/*slot* backend/internal/pool/*precious* backend/internal/pool/*pin*
```

Make sure only keeper files remain; delete unpromoted throwaways (`spill_excise_nochatretry_test.go` once E7 is proven, etc.).

- [ ] **Step 2: run the R1–R6 keepers in order**

Run: `go test ./backend/internal/pool/ -run 'TestStrictOrderDrainsAccountOneFirst|TestSlotLedgerTwoSlotsThirdParks|TestSlotLedgerPerModelIsolation|TestSpillBurst5TwoAccounts|TestPreciousTwoAccountsSameModel|TestPinBurst5OneAccount|TestPinLeavesOtherModelSlotsFree|TestNatural429RequeuesNoParkNoFailover' -count=1 -v`
Expected: 8/8 PASS — this is the R1–R6 acceptance proof.

- [ ] **Step 3: migrate the old ladder + queue tests**

Delete the `rotation`/`overflow`/`cooldown` expectation branches in `concurrency_ladder_test.go` (including `setLadderRotation`, `ladTripWindow`, `ladQuarantineViaBan` if they assume the old engine — ban quarantine is KEPT so `ladQuarantineViaBan` stays with no-cooldown expectations), `queue_wait_test.go`, `queue_wait_matrix_test.go`.

- [ ] **Step 4: re-run keepers + update UI copy**

Run the same keepers + `npm --prefix frontend run format:check`
Expected: all PASS; the LiveConsole copy names the spill lane, not the local-429.

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/ frontend/src/lib/components/LiveConsole.svelte
git commit -m "feat(pool): promote Ordered Queue-Spill keepers, retire legacy expectations"
```

---

## Traceability R1–R6 → Task

| R | Behavior | Implementing task | Repro |
|---|---|---|---|
| R1 | strict #1→#N order, the only ranking | C1 (`spill_order.go`); E6 foundation | `TestStrictOrderDrainsAccountOneFirst` |
| R2 | 2 concurrent slots per (account, model) — 1 account may hold 2×A + 2×B together | I1 (`slot_ledger.go` keyed (token, model), `SLOTS_PER_ACCOUNT`) | `TestSlotLedgerTwoSlotsThirdParks` (3-req-same-model) + `TestSlotLedgerPerModelIsolation` (2×A + 2×B = 4 running) |
| R3 | spill ONLY when queue-wait expires | I2 (`spill_queue.go`, `MAX_SPILL_ACCOUNTS`) | `TestSpillBurst5TwoAccounts` (burst-5-2-accounts) |
| R4 | precious never dropped; N accounts × model = 2N slots | I3 (`precious.go`); E8 foundation | `TestPreciousTwoAccountsSameModel` |
| R5 | explicit PIN_MODEL, 1 account = 1 model | I4 (`pin_model.go`); C3 UI | `TestPinBurst5OneAccount` (pin-burst-5) + `TestPinLeavesOtherModelSlotsFree` (pin takes no other-model slot) + C3 e2e |
| R6 | natural 429 = back to queue, account not parked (except banned/suspend) | I5 (requeue+notBefore); E1/E2/E4 foundations (correlatives surface, no cooldown); E7 (no second retry) | `TestNatural429RequeuesNoParkNoFailover` (429-back-to-queue) + `TestExciseBanStillQuarantined` |

## Self-Review

1. **Spec coverage:** R1→C1, R2→I1, R3→I2, R4→I3(+E8), R5→I4(+C3), R6→I5(+E1/E2/E4/E7). Knobs `SLOTS_PER_ACCOUNT` (per-(account, model) cap, MASQ)/`QUEUE_WAIT`/`PIN_MODEL`/`MAX_SPILL_ACCOUNTS`→I1/I2/I4/C2; `QUEUE_DEPTH` kept by I1. Excision a→E1, b→E2, c→E3, d→KEEP in E4, e→E4, f+g→E5, h→E6, i→E7, j→E8, k→I2 (timeout = internal spill signal, local-429 deleted). No gaps.
2. **Placeholder scan:** no TBD/TODO/"like Task N" — every step carries actual code, explicit expectation numbers, and run commands. "Actual name" references are only for helpers that must be read from the neighboring file at execution time (MaintainOnce, e2e helpers, catalog accessor), with an explicit fallback forbidding new ones.
3. **Type consistency:** `slotAcquire/slotPermit/slotQueueExhaustedError`, `spillLane/acquireSpill/spillOrder`, `parsePinModel/pinnedOut/pinFailFastError`, `preciousAdd/preciousHas/preciousRemove` defined once in the producer task and used under the same name in consumers. `Lease{Token, QueueWait, SessionInstanceID}`, `p.LeaseRelease`, `newTestPoolCfg`, `testutil.MockUpstream` fields/methods all verified at the worktree revision.
