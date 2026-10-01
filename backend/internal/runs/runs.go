// Package runs implements the per-turn FreeBuff agent-run lifecycle for a
// single token: one agent-runs START per prompt turn, per-run step counters
// (llm_step_number 1..N within the turn), async FINISH drain, 30-min auth
// cooldown, and a shutdown drain. Port of
// reference/gateways/proxy-freebuff/lib/runs.js and
// reference/gateways/freebuff-2api-lza6/run_manager.go (tokenPool half), adapted to
// this project's layout: the
// session manager is owned by the caller (pool) and only used here for the
// shutdown EndSession, and the pool — not this package — decides which token
// serves a request.
//
// CLI parity (upstream/freebuff packages/agent-runtime/src/run-agent-step.ts:926-944):
// every prompt mints a FRESH runId — one START per prompt — and the turn's
// tool/llm steps all reuse that run_id with an incrementing llm_step_number.
// The 6h run rotation/multiplexing (one run per agent reused across turns)
// has no CLI counterpart and is retired: MintTurnRun always STARTs.
//
// Concurrency: all run bookkeeping is guarded by the manager mutex; no lock
// is held across upstream calls. Minting swaps the current run under the
// lock and hands the old one to the async FINISH queue, so concurrent
// turns each get their own run, race-safe.
package runs

import (
	"context"
	"errors"
	"fmt"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/upstream"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// shutdownTimeout bounds Shutdown when the caller passes a context without a
// deadline (PRD §5: "10s force deadline").
const shutdownTimeout = 10 * time.Second

// ErrShuttingDown is returned by MintTurnRun and Acquire once Shutdown has
// begun: the manager has been (or is being) drained and the deferred-finish
// worker is stopped, so a run STARTed now would never be FINISHed. The
// shared mint re-checks the flag after its upstream StartRun returns and
// discards/finishes the freshly started run inline instead of tracking it.
var ErrShuttingDown = errors.New("runs: manager shutting down; new run starts refused")

// Defaults for the bounded deferred-FINISH queue (issue #90) and the
// draining-runs list bounds (issue #55). Overridable via
// runs.Options (pool wires config knobs RUN_FINISH_QUEUE_SIZE /
// RUN_FINISH_INLINE_TIMEOUT / RUNS_DRAIN_QUEUE_CAP / RUNS_DRAIN_TTL).
const (
	defaultFinishQueueSize     = 64
	defaultInlineFinishTimeout = 250 * time.Millisecond
	defaultDrainQueueCap       = 64
	defaultDrainTTL            = 10 * time.Minute
)

// Options configures a RunManager: the bounded deferred-FINISH worker queue
// bounds (#90/#55) and the optional session-state store for run persistence
// across restarts (#40). Zero values fall back to the package defaults.
//
// RotationInterval is RETIRED (per-turn mint has no CLI counterpart): kept
// only so pool runOptions still compiles — the value is ignored.
// TODO(port-runsapi): delete the field with the unleased MintTurnRun path
// once its last caller migrates to Acquire (only tests exercise it now
// that pool mints leased).
type Options struct {
	RotationInterval    time.Duration // Deprecated: ignored; per-turn mint always STARTs.
	FinishQueueSize     int
	InlineFinishTimeout time.Duration
	DrainQueueCap       int
	DrainTTL            time.Duration
	Store               *session.Store
}

// RunSnapshot is a best-effort view of the manager state for healthz.
type RunSnapshot struct {
	ActiveRuns    int
	CooldownUntil time.Time
	Requests      int
	BanError      *upstream.BanError
	// BannedUntil is the ban window deadline; BanError is only "live"
	// while now < BannedUntil (mirrors BanError()'s time check). The pool
	// gates its ban risk label on it so an expired ban is not sticky.
	BannedUntil time.Time
	// RateLimitKind / RateLimitWindowHours / RateLimitResetsAt expose WHY the
	// token is cooling down when upstream's refusal was a distinguishable
	// window refusal (upstream.WindowKindFreebucks, the freebucks ceiling):
	// the kind, the window length upstream declared, and the instant upstream
	// says the window refills. CooldownRateLimit remembers the whole
	// RateLimitError, so the reset instant is never re-derived — these fields
	// are just that memory, and they are filled only while the cooldown is
	// live (mirroring RateLimitError()). Empty/zero for every other cooldown
	// (plain retry-after rate limit, ip_capped, ban, auth).
	RateLimitKind        string
	RateLimitWindowHours int
	RateLimitResetsAt    time.Time
}

// RunManager owns the current runs (one per agent) plus the draining list
// for a single token.
type RunManager struct {
	client           *upstream.Client
	session          *session.Manager
	rotationInterval time.Duration // Deprecated: ignored (per-turn mint); kept so pool runOptions compiles.

	mu            sync.Mutex
	runs          map[string]*Run // agentID â†’ current run
	draining      []*Run          // drained runs awaiting FINISH
	cooldownUntil time.Time
	// rateLimit is the last 429 rate-limit error applied to this token's
	// cooldown. It is surfaced by RateLimitError() so exhausted tokens
	// keep returning 429 + Retry-After instead of a generic 502 while the
	// cooldown window is active.
	rateLimit *upstream.RateLimitError
	// banUntil is set when the account is banned; MintTurnRun refuses with the
	// remembered ban error until the unban time.
	banUntil time.Time
	ban      *upstream.BanError
	// banPermanent marks a hard ban (no resumes_at): the account never
	// self-heals upstream (past_enforcement sticks permanently), so the
	// remembered ban stays live until the operator unlocks the token —
	// never auto-lifted (mirrors upstream freebuff-trust.ts cap semantics;
	// the old 24h safety window re-contacted a dead account daily).
	banPermanent bool
	// countryBlock is the last country-block error applied to this token's
	// cooldown. It is surfaced by CountryBlockedError() so a region-blocked
	// token keeps returning the block error instead of re-hitting upstream
	// during the window (mirrors the rate-limit/ban memory).
	countryBlock *upstream.CountryBlockedError
	countryUntil time.Time
	// modelLimits remembers per-model admission/run-start rate-limit
	// refusals that carry an expiry (see RememberModelRateLimit): an
	// admission 429's quota truth dies with the walk unless kept here, so
	// the next same-model request skips the dead lane contact-free until
	// the window resets. Keyed by model id; lazy-expired on read.
	modelLimits map[string]*modelLimitEntry
	// totalRequests is the cumulative count of minted turns handed out.
	// It is kept separate from the per-run counters because drained runs
	// that get FINISHed leave the active+draining sets and would otherwise
	// take their request counts out of Snapshot.
	totalRequests int

	// Deferred-FINISH queue (issue #90): drained runs are FINISHed by one
	// background worker per manager;
	// finishQueue is bounded (Options.FinishQueueSize); when it is
	// full the caller runs the job inline bounded by inlineFinishTimeout.
	// finishStop is closed once (finishOnce) by Shutdown; the worker drains
	// the queue and exits, tracked by finishWg. finishStartOnce starts the
	// worker on first use. finishExited is closed by the worker on exit
	// (test hook for goroutine-leak assertions).
	finishQueue chan asyncJob
	finishStop  chan struct{}
	// finishDrainCtx is bounded by Shutdown's deadline; the drain loop
	// abandons queued jobs when it expires.
	finishDrainCtx      context.Context
	finishOnce          sync.Once
	finishStartOnce     sync.Once
	finishWg            sync.WaitGroup
	finishExited        chan struct{}
	inlineFinishTimeout time.Duration

	// keptForPersistence is set by Shutdown when run persistence kept the
	// active runs alive across restart (issue #40). Pool.Shutdown reads it
	// to avoid a spurious "runs left after shutdown" warning on a clean
	// shutdown of a persisted deployment.
	keptForPersistence bool
	// shuttingDown is set at the START of Shutdown, before the drain: no
	// new run may be STARTed from that point on. An in-flight request
	// still in its acquire phase when the drain begins would otherwise
	// mint a fresh run into the cleared manager after the finish worker
	// stopped — that run would never be FINISHed. MintTurnRun consults it both
	// before the upstream StartRun and after it returns. Guarded by mu.
	shuttingDown bool
	// drainQueueCap / drainTTL bound the draining list (issue #55).
	drainQueueCap int
	drainTTL      time.Duration

	// store persists active runs across restarts (SESSION_PERSIST, issue
	// #40); nil disables. key is the stable token hash
	// (upstream.Client.TokenKey) mirroring the session store's key space.
	store *session.Store
	key   string
}

// NewRunManager builds the manager for one token. rotationInterval is
// RETIRED (per-turn mint ignores it; kept only for compat) — pass any value.
// The session manager is used only for Shutdown's EndSession.
func NewRunManager(client *upstream.Client, session *session.Manager, rotationInterval time.Duration) *RunManager {
	return NewRunManagerOpts(client, session, Options{RotationInterval: rotationInterval})
}

// NewRunManagerOpts builds the manager with full Options (the bounded finish
// queue and draining-list bounds from issues #90/#55 and optional run
// persistence from #40). Zero option values fall back to the package
// defaults. Options.RotationInterval is ignored (retired 6h rotation).
func NewRunManagerOpts(client *upstream.Client, session *session.Manager, opts Options) *RunManager {
	queueSize := opts.FinishQueueSize
	if queueSize < 1 {
		queueSize = defaultFinishQueueSize
	}
	inlineTimeout := opts.InlineFinishTimeout
	if inlineTimeout <= 0 {
		inlineTimeout = defaultInlineFinishTimeout
	}
	drainCap := opts.DrainQueueCap
	if drainCap < 1 {
		drainCap = defaultDrainQueueCap
	}
	drainTTL := opts.DrainTTL
	if drainTTL <= 0 {
		drainTTL = defaultDrainTTL
	}
	m := &RunManager{
		client:              client,
		session:             session,
		rotationInterval:    opts.RotationInterval,
		runs:                make(map[string]*Run),
		finishQueue:         make(chan asyncJob, queueSize),
		finishStop:          make(chan struct{}),
		finishExited:        make(chan struct{}),
		inlineFinishTimeout: inlineTimeout,
		drainQueueCap:       drainCap,
		drainTTL:            drainTTL,
	}
	if client != nil {
		m.key = client.TokenKey()
	}
	if opts.Store != nil {
		m.store = opts.Store
	}
	return m
}

// SetStore injects the shared session-state store used for run persistence
// (SESSION_PERSIST, issue #40). The pool calls this on SetSessionStore for
// the fixed-token managers (built before the store exists) and passes the
// store through Options for runtime-added tokens. A nil store disables run
// persistence. Runs already tracked keep their state; persistence applies
// to subsequent START/FINISH transitions.
func (m *RunManager) SetStore(store *session.Store) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = store
}

// KeptForPersistence reports whether the last Shutdown preserved the active
// runs for restart-resume (issue #40) instead of FINISHing them.
func (m *RunManager) KeptForPersistence() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.keptForPersistence
}

// MintTurnRun mints a brand-new agent run for one CLI turn: every call POSTs
// a fresh agent-runs START and returns its run id — one START per prompt
// (upstream/freebuff packages/agent-runtime/src/run-agent-step.ts:926-944).
// The turn's tool/llm steps all reuse that run id with an incrementing
// llm_step_number; nothing re-STARTs mid-turn.
//
// The previous turn's run for agentID (if any) is pushed to the draining
// list and FINISHed asynchronously, so back-to-back turns never leak an
// upstream run. The minted run is tracked unleased (see TurnRun): the caller
// owns its FINISH through the upstream sender — this package's drain paths
// (Maintain/Shutdown/the next mint's safety net) only FINISH runs the caller
// left behind.
//
// MintTurnRun refuses while the token cools down and after Shutdown begins
// (ErrShuttingDown), like Acquire.
func (m *RunManager) MintTurnRun(ctx context.Context, agentID string) (string, error) {
	run, err := m.startTurnRun(ctx, agentID, false)
	if err != nil {
		return "", err
	}
	return run.RunID, nil
}

// TurnRun returns the latest minted run for agentID (nil when none): the run
// metadata the server needs to FINISH the turn through the upstream sender
// (RunID + AgentID + the step count via StepCount/RecordStep). The returned
// pointer is the manager's tracked run — read its fields and advance it only
// through RunManager methods.
func (m *RunManager) TurnRun(agentID string) *Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runs[agentID]
}

// startTurnRun is the shared mint behind MintTurnRun and Acquire: gates, one
// upstream StartRun, track-as-current (draining the predecessor), and persist.
// leased leases the run to the caller (inflight 1, the Acquire contract); an
// unleased mint stays at inflight 0 for a caller-owned lifecycle (the
// MintTurnRun contract). The lease is taken in the same critical section as
// the store, so a racing FinishAllRuns can only detach (and defer) the run —
// never orphan the lease.
func (m *RunManager) startTurnRun(ctx context.Context, agentID string, leased bool) (*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	if m.shuttingDown {
		m.mu.Unlock()
		return nil, ErrShuttingDown
	}
	if now := time.Now(); now.Before(m.cooldownUntil) {
		until := m.cooldownUntil
		m.mu.Unlock()
		return nil, fmt.Errorf("token cooling down until %s", until.Format(time.RFC3339))
	}
	m.mu.Unlock()

	runID, err := m.client.StartRun(ctx, agentID)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if m.shuttingDown {
		// Shutdown began while the upstream START was in flight: the
		// manager was (or is being) drained and the finish worker is
		// stopped, so tracking this fresh run would leave it never
		// FINISHed. Discard it — best-effort FINISH it inline
		// (bounded by the shutdown deadline) so the upstream agent run
		// does not leak until its own expiry.
		m.mu.Unlock()
		m.finishInline(runID, agentID)
		return nil, ErrShuttingDown
	}
	// Mint the trace session id before logging so the run-started line
	// and every chat trace of this run share it.
	traceSessionID := newTraceSessionID()
	slog.Debug("runs: run started", "agent_id", agentID, "run_id", runID, "trace_session_id", traceSessionID)

	inflight := 0
	if leased {
		inflight = 1
	}
	newRun := &Run{
		AgentID:        agentID,
		RunID:          runID,
		StartedAt:      time.Now(),
		TraceSessionID: traceSessionID,
		// One client id for the whole turn (CLI: one promptId per prompt).
		ClientID: upstream.NewClientID(),
		inflight: inflight,
		Requests: 1,
	}
	oldRun := m.runs[agentID]
	m.runs[agentID] = newRun
	m.totalRequests++
	if oldRun != nil {
		m.appendDrainingLocked(oldRun)
	}
	m.mu.Unlock()

	m.persistRun(newRun)
	if oldRun != nil {
		m.enqueueFinish(oldRun)
	}
	return newRun, nil
}

// Acquire mints the turn's run leased (inflight 1): every call STARTs fresh
// through the shared per-turn path — the 6h run reuse is retired (it has no
// CLI counterpart). Pool (acquire_route.go) mints every turn through here so
// a concurrent next-turn mint's drain cannot FINISH a live turn's run; the
// turn's Release re-queues the deferred FINISH. MintTurnRun is the unleased
// variant (caller-owned FINISH, pinned by TestMintTurnRunContract).
func (m *RunManager) Acquire(ctx context.Context, agentID string) (*Run, error) {
	return m.startTurnRun(ctx, agentID, true)
}

// Release decrements the inflight counter of a leased run. Safe on nil.
// Draining finishes happen on the maintain tick or the next rotation;
// when the LAST lease of a draining run releases (a FinishAllRuns
// deferral, a rotation, or an abandonment), the FINISH is re-queued
// immediately — otherwise a run drained with an outstanding lease would
// never reach finishIfReadyCtx after its final release.
func (m *RunManager) Release(run *Run) {
	if run == nil {
		return
	}
	m.mu.Lock()
	if run.inflight > 0 {
		run.inflight--
	}
	if run.inflight > 0 || !m.runDrainingLocked(run) {
		m.mu.Unlock()
		return
	}
	// Last lease on a draining run: drop it from the active set (a
	// drain-marked run may still be current) so finishIfReadyCtx does not
	// skip it as still-current, then re-queue the deferred FINISH.
	if current, ok := m.runs[run.AgentID]; ok && current == run {
		delete(m.runs, run.AgentID)
	}
	m.mu.Unlock()
	m.enqueueFinish(run)
}

// InflightCount returns the total in-flight request count across all runs,
// or for a specific agentID if provided.
func (m *RunManager) InflightCount(agentID ...string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(agentID) > 0 && agentID[0] != "" {
		if r := m.runs[agentID[0]]; r != nil {
			return r.inflight
		}
		return 0
	}
	count := 0
	for _, r := range m.runs {
		count += r.inflight
	}
	for _, r := range m.draining {
		count += r.inflight
	}
	return count
}

// Invalidate drops the active run for agentID without finishing it (e.g. on runId not found).
func (m *RunManager) Invalidate(agentID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.runs, agentID)
	if m.store != nil && m.key != "" {
		m.store.RemoveRun(m.key, agentID)
	}
}

// Maintain FINISHes the draining list. Runs with outstanding inflight leases
// or an in-flight FINISH are skipped. Best effort: failures are logged,
// never returned (background job). While the token is cooling down (auth
// rejection, rate limit, ban) the pass returns immediately: no draining
// FINISH, no log — retrying upstream work during a cooldown looks like abuse
// (observed in production). The pool logs the skip.
//
// The 6h rotation is retired: per-turn minting always STARTs, so this pass
// never STARTs anything — it only bounds the draining list and re-enqueues
// deferred FINISHes (e.g. after a transient FINISH failure).
func (m *RunManager) Maintain(_ context.Context) {
	if !m.MaintenanceEligible() {
		return
	}
	m.mu.Lock()
	// Bound the draining list before re-enqueuing its FINISHes: entries
	// past the TTL or cap are force-dropped (issue #55) so a persistently
	// failing FINISH cannot grow the list without bound.
	m.pruneDrainingLocked()
	draining := append([]*Run(nil), m.draining...)
	m.mu.Unlock()

	// Deferred-FINISH through the bounded queue (issue #90): the maintain
	// tick never blocks on upstream FINISH calls; the worker (or the inline
	// fallback) owns them. finishIfReady skips busy/finishing runs, so a
	// run with an outstanding lease stays draining for the next pass.
	for _, run := range draining {
		m.enqueueFinish(run)
	}
}

// Shutdown FINISHes every run (active and draining) and ends the upstream
// session. When ctx carries no deadline a 10s force deadline is applied
// (PRD Â§5.5 shutdown sequence).
func (m *RunManager) Shutdown(ctx context.Context) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, shutdownTimeout)
		defer cancel()
	}

	// Refuse new run STARTs from this instant: an in-flight request
	// still in its acquire phase when the drain begins must not start a
	// fresh run after the manager is cleared — the finish worker is
	// stopped, so that run would never be FINISHed. MintTurnRun re-checks the
	// flag after its upstream StartRun returns and discards (inline-
	// FINISHing) the fresh run instead of tracking it.
	m.mu.Lock()
	m.shuttingDown = true
	m.mu.Unlock()

	// Stop the deferred-job worker first (issue #90): it drains whatever is
	// queued, so its FINISHes land before our own claim below, and no job
	// can start after we snapshot the runs. The worker's finishing-flag
	// claims are respected by the claim loop (finishing runs are skipped).
	// The drain is bounded by the same deadline as the rest of Shutdown: a
	// saturated queue (up to FinishQueueSize jobs x session-call timeout)
	// must not stall shutdown for minutes.
	m.finishDrainCtx = ctx
	m.finishOnce.Do(func() { close(m.finishStop) })
	// Ensure the worker exists BEFORE waiting: its finishWg.Add(1) must be
	// ordered before the Wait below — a lazy first-start racing Shutdown
	// would be a WaitGroup Add/Wait race and Shutdown could proceed without
	// the late worker. If it was never started, it exits immediately on
	// the closed stop channel and balances the count.
	m.startFinishWorker()
	// Wait for the drain, but never past the caller's deadline: if the
	// worker is still draining after ctx expires, the remaining jobs are
	// abandoned (best-effort FINISHes; the upstream connection dies with
	// the process anyway).
	waitDone := make(chan struct{})
	go func() {
		m.finishWg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-ctx.Done():
		// Report the queue and run counts, never the manager struct itself
		// (a *RunManager dump would leak internal state to the log).
		m.mu.Lock()
		runs := len(m.runs)
		m.mu.Unlock()
		slog.Warn("runs: finish-queue drain exceeded shutdown deadline; abandoning remaining jobs",
			"pending_jobs", len(m.finishQueue),
			"runs", runs,
			"key", m.key)
	}
	_ = ctx.Err() // lint: ctx is still used by the caller below

	// Run persistence (issue #40): with a store, keep the active runs alive
	// across the restart like the session keep-alive — FINISHing them here
	// would force the next process to re-START and burn upstream calls. The
	// runs are already persisted on START; re-save so the latest Requests
	// counter survives.
	if m.store != nil && m.key != "" {
		m.mu.Lock()
		snapshot := make([]*Run, 0, len(m.runs))
		for _, run := range m.runs {
			// cloneRun, never *run: the Run carries an atomic.Int64 step
			// counter that must not be copied after first use.
			snapshot = append(snapshot, m.cloneRun(run))
		}
		// Drained runs are finished, never resumed: best-effort
		// FINISH them NOW — the worker is stopped, so this is their last
		// chance, and a stale store entry must not resurrect a finished
		// run on the next boot.
		draining := make([]*Run, 0, len(m.draining))
		for _, run := range m.draining {
			if run.finishing {
				continue
			}
			run.finishing = true
			draining = append(draining, run)
		}
		m.mu.Unlock()
		for _, run := range snapshot {
			m.persistRun(run)
		}
		var errs []string
		for _, run := range draining {
			status, steps, totalSteps := m.finishPayload(run)
			if err := m.client.FinishRun(ctx, run.RunID, status, totalSteps, steps, ""); err != nil {
				errs = append(errs, fmt.Sprintf("finish run %s: %v", run.RunID, err))
			} else {
				m.removeRun(run)
			}
		}
		m.keptForPersistence = true
		if err := m.session.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Sprintf("shutdown session: %v", err))
		}
		if len(errs) > 0 {
			slog.Warn("runs: shutdown with errors", "errors", strings.Join(errs, "; "))
		}
		return
	}

	m.mu.Lock()
	// Skip runs with a FINISH already in flight (an async rotate drain
	// owns them): re-FINISHing the same run id upstream is a duplicate
	// call the drain goroutine is already completing. Claim the rest by
	// setting finishing so a concurrently starting finishIfReady cannot
	// double-FINISH a run we are about to finish here.
	all := make([]*Run, 0, len(m.runs)+len(m.draining))
	for _, run := range m.runs {
		if run.finishing {
			continue
		}
		run.finishing = true
		all = append(all, run)
	}
	for _, run := range m.draining {
		if run.finishing {
			continue
		}
		run.finishing = true
		all = append(all, run)
	}
	m.runs = make(map[string]*Run)
	m.draining = nil
	m.mu.Unlock()

	var errs []string
	for _, run := range all {
		status, steps, totalSteps := m.finishPayload(run)
		if err := m.client.FinishRun(ctx, run.RunID, status, totalSteps, steps, ""); err != nil {
			errs = append(errs, fmt.Sprintf("finish run %s: %v", run.RunID, err))
		}
	}
	if err := m.session.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Sprintf("shutdown session: %v", err))
	}
	if len(errs) > 0 {
		slog.Warn("runs: shutdown with errors", "errors", strings.Join(errs, "; "))
	}
}

// RestoreRequestsTotal raises the cumulative minted-turn counter to n after a
// restart (pool_state owns the number; the pool restores it at Start). Runs
// START lazily, so a fresh process begins at zero and every mint increments
// from the restored base — dashboard Requests never zeroes on restart.
// Max-guarded: a late restore must never drag a live counter backwards.
func (m *RunManager) RestoreRequestsTotal(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if n > m.totalRequests {
		m.totalRequests = n
	}
}

// Snapshot returns a best-effort view of the manager state.
func (m *RunManager) Snapshot() RunSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := RunSnapshot{ActiveRuns: len(m.runs), CooldownUntil: m.cooldownUntil, Requests: m.totalRequests, BanError: m.ban, BannedUntil: m.banUntil}
	// Window evidence rides the same live-window gate as RateLimitError():
	// the remembered refusal is the cooldown memory, and a lifted cooldown
	// must never keep rendering as the current reason.
	if m.rateLimit != nil && time.Now().Before(m.cooldownUntil) {
		s.RateLimitKind = m.rateLimit.WindowKind()
		s.RateLimitWindowHours = m.rateLimit.WindowHours
		s.RateLimitResetsAt = m.rateLimit.ResetAt
	}
	return s
}

// Prewarm is a no-op kept for pool compatibility.
//
// TODO(port-runsapi): per-turn mint STARTs exactly one run per prompt, so a
// pre-created run would be drained (and FINISHed) by the next mint without
// ever serving a chat — pure waste. Worse, a boot fleet of live runs is the
// ban-grade fanout shape upstream refuses with free_mode_run_fanout (pool
// already stopped prewarming for this reason). Kept only because pool's
// maintainToken still calls it; the server slice removes the call.
func (m *RunManager) Prewarm(_ context.Context, _ []string) {}

// Precreate is a no-op kept for pool compatibility: the turn's Acquire
// covers the START, so there is nothing to pre-create.
//
// TODO(port-runsapi): kept only because pool (acquire_route.go, maintainToken)
// still calls it after session admission. Returns ctx.Err() (nil on a live
// context) so the pool's best-effort `_ =` call sites behave. The server
// slice removes the calls.
func (m *RunManager) Precreate(ctx context.Context, _ string) error {
	return ctx.Err()
}
