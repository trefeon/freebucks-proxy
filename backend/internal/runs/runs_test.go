package runs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	agentA = "agent-alpha"
	agentB = "agent-beta"
)

// newTestManager wires the mock upstream through a real client and session
// manager.
func newTestManager(t *testing.T, mock *testutil.MockUpstream, rotationInterval time.Duration) (*RunManager, *session.Manager) {
	t.Helper()
	client, err := upstream.New("tok", &config.Config{
		UpstreamBaseURL:    mock.URL(),
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RotationInterval:   6 * time.Hour,
		RegistryRefresh:    6 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	sess := session.NewManager(client)
	return NewRunManager(client, sess, rotationInterval), sess
}

// eventually polls cond until it holds or the deadline passes. Thin
// package wrapper over testutil.WaitFor so the runs package's many call
// sites keep their signature; the loop itself lives in testutil/poll.go.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	testutil.WaitFor(t, 3*time.Second, cond, what)
}

func finishedRun(mock *testutil.MockUpstream, runID string) (testutil.FinishedRun, bool) {
	// Snapshot under the mock's lock: FINISH arrives from a background
	// goroutine while the test polls (eventually), so raw field reads race.
	for _, f := range mock.FinishedRunsSnapshot() {
		if f.RunID == runID {
			return f, true
		}
	}
	return testutil.FinishedRun{}, false
}

// TestPerTurnMintMintsFreshRun pins the CLI parity core (run-agent-step.ts:926-944:
// one START per prompt): back-to-back turns NEVER share a run — every mint
// STARTs fresh, and the superseded turn drains with an async FINISH. No aging
// or rotation interval is involved: the 6h multiplexing is retired.
func TestPerTurnMintMintsFreshRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	// Direct primitive: two MintTurnRun calls mint two upstream STARTs and
	// return distinct run ids.
	firstID, err := mgr.MintTurnRun(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if firstID != "run-0001" {
		t.Fatalf("first run id = %q, want run-0001", firstID)
	}
	if got := mgr.TurnRun(agentA); got == nil || got.RunID != firstID {
		t.Fatalf("TurnRun = %+v, want the tracked first turn %q", got, firstID)
	}
	secondID, err := mgr.MintTurnRun(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if secondID == "" || secondID == firstID {
		t.Fatalf("second run id = %q, want a fresh run per turn (got %q)", secondID, firstID)
	}
	if got := mgr.TurnRun(agentA); got == nil || got.RunID != secondID {
		t.Fatalf("TurnRun after second mint = %+v, want %q", got, secondID)
	}

	// The superseded turn is FINISHed asynchronously with its request count.
	eventually(t, "FINISH of superseded turn", func() bool {
		f, ok := finishedRun(mock, "run-0001")
		return ok && f.Status == "completed" && f.TotalSteps == 1
	})

	// The leased path mints fresh too: the next Acquire is its own turn.
	third, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if third.RunID != "run-0003" {
		t.Fatalf("third turn run id = %q, want run-0003", third.RunID)
	}
	if third.StartedAt.IsZero() {
		t.Error("StartedAt not set")
	}
	mgr.Release(third)
}

// TestMintTurnRunContract pins the server slice's consumption API: MintTurnRun
// returns the run id string, TurnRun exposes the tracked turn metadata (runID
// + agentID + step count) for the server-owned FINISH, and both refuse on
// cooldown/shutdown.
func TestMintTurnRunContract(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	if got := mgr.TurnRun(agentA); got != nil {
		t.Fatalf("TurnRun before any mint = %+v, want nil", got)
	}
	runID, err := mgr.MintTurnRun(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if runID == "" {
		t.Fatal("MintTurnRun returned an empty run id")
	}
	meta := mgr.TurnRun(agentA)
	if meta == nil {
		t.Fatal("TurnRun = nil right after mint, want the tracked turn")
	}
	if meta.RunID != runID || meta.AgentID != agentA {
		t.Errorf("TurnRun = (%q, %q), want (%q, %q)", meta.RunID, meta.AgentID, runID, agentA)
	}
	if n := meta.StepCount.Load(); n != 0 {
		t.Errorf("fresh turn StepCount = %d, want 0", n)
	}

	// Cooldown refuses the mint with no upstream contact.
	mgr.Cooldown(time.Hour)
	if _, err := mgr.MintTurnRun(context.Background(), agentA); err == nil {
		t.Error("MintTurnRun during cooldown succeeded, want refusal")
	}
	if got := len(mock.StartedRunsSnapshot()); got != 1 {
		t.Errorf("STARTs after refused mint = %d, want 1", got)
	}
	mgr.ClearCooldowns()

	// Shutdown refuses the mint.
	mgr.Shutdown(context.Background())
	if _, err := mgr.MintTurnRun(context.Background(), agentA); !errors.Is(err, ErrShuttingDown) {
		t.Errorf("MintTurnRun after Shutdown = %v, want ErrShuttingDown", err)
	}
}

func TestFinishRunDropsFromActive(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if run.RunID != "run-0001" {
		t.Fatalf("run id = %q", run.RunID)
	}
	mgr.Release(run)

	// Issue #114: record 3 completed steps — totalSteps must come from the
	// recorded steps (preferred over the request-count fallback) and the
	// steps must ride IN the FINISH payload.
	for i := 0; i < 3; i++ {
		mgr.RecordStep(run, "")
	}
	mgr.FinishRun(context.Background(), run)

	eventually(t, "FINISH payload", func() bool {
		f, ok := finishedRun(mock, "run-0001")
		return ok && f.Status == "completed" && f.TotalSteps == 3 && len(f.Steps) == 3 &&
			f.Steps[0].StepNumber == 1 && f.Steps[2].StepNumber == 3
	})

	// Dropped from active: the next acquire must START afresh.
	next, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if next.RunID != "run-0002" {
		t.Fatalf("run after FinishRun = %q, want run-0002 (re-START)", next.RunID)
	}
}

func TestInvalidateRestarts(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if run.RunID != "run-0001" {
		t.Fatalf("run id = %q", run.RunID)
	}
	mgr.Release(run)

	// Upstream said "runId not found"; drop it without FINISH.
	mgr.Invalidate(agentA)

	next, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if next.RunID != "run-0002" {
		t.Fatalf("run after Invalidate = %q, want run-0002 (re-START)", next.RunID)
	}
	if started := mock.StartedRunsSnapshot(); len(started) != 2 {
		t.Errorf("STARTs = %d, want 2", len(started))
	}
	// The invalidated run must never be FINISHed (Invalidate deletes it
	// without an upstream FINISH), and with the #91 child-run traffic gone
	// no other FINISH may exist either.
	if finished := mock.FinishedRunsSnapshot(); len(finished) != 0 {
		t.Errorf("invalidated run must not be FINISHed, got %v", finished)
	}
}

func TestCooldownBlocksAcquire(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	if _, err := mgr.Acquire(context.Background(), agentA); err != nil {
		t.Fatal(err)
	}
	mgr.Cooldown(30 * time.Minute)

	until := mgr.CooldownUntil()
	if until.Before(time.Now().Add(30*time.Minute - time.Second)) {
		t.Errorf("CooldownUntil = %v, want ~now+%s", until, 30*time.Minute)
	}
	if snap := mgr.Snapshot(); snap.CooldownUntil != until {
		t.Errorf("snapshot CooldownUntil = %v, want %v", snap.CooldownUntil, until)
	}

	_, err := mgr.Acquire(context.Background(), agentA)
	if err == nil {
		t.Fatal("Acquire succeeded while cooling down")
		return
	}
	if !strings.Contains(err.Error(), "cooling down until") {
		t.Errorf("error = %v, want cooldown error", err)
	}
	if errors.Is(err, upstream.ErrAuthRejected) {
		t.Error("cooldown error must not alias ErrAuthRejected")
	}
}

func TestShutdownFinishesAllAndEndsSession(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, sess := newTestManager(t, mock, time.Hour)

	// Prime an active upstream session so EndSession actually fires.
	if _, err := sess.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	a, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := mgr.Acquire(context.Background(), agentB)
	if err != nil {
		t.Fatal(err)
	}
	// Leases deliberately left un-released: shutdown must force-finish.
	_ = a
	_ = b

	mgr.Shutdown(context.Background())

	for _, want := range []struct {
		runID string
		steps int
	}{{"run-0001", 1}, {"run-0002", 1}} {
		f, ok := finishedRun(mock, want.runID)
		if !ok {
			t.Errorf("run %s not FINISHed on shutdown", want.runID)
			continue
		}
		if f.Status != "completed" || f.TotalSteps != want.steps {
			t.Errorf("run %s finished as %+v, want completed/%d", want.runID, f, want.steps)
		}
	}
	if mock.SessionEnds != 1 {
		t.Errorf("session ends = %d, want 1", mock.SessionEnds)
	}
	if snap := mgr.Snapshot(); snap.ActiveRuns != 0 {
		t.Errorf("active runs after shutdown = %d, want 0", snap.ActiveRuns)
	}

	// Idempotent: a second shutdown must not duplicate FINISHes.
	mgr.Shutdown(context.Background())
	if got := len(mock.FinishedRunsSnapshot()); got != 2 {
		t.Errorf("finished runs after double shutdown = %d, want 2", got)
	}
}

func TestMaintainDrainsOnlyWhenIdle(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	// The first turn holds its lease; the second turn mints fresh (per-turn
	// mint, no aging) and drains the first while its lease is still out.
	first, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	second, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Release(second)
	if second.RunID == first.RunID {
		t.Fatalf("turns shared run %s, want a fresh run per turn", first.RunID)
	}
	mgr.Maintain(context.Background())

	// The drained turn has an outstanding lease: the async finish must
	// observe inflight > 0 and skip it. No exposed state signals "the worker
	// ran and skipped", so this negative assertion is bounded by wall time:
	// 400ms gives a buggy implementation that ignores the lease ample chance
	// to FINISH.
	time.Sleep(400 * time.Millisecond)
	if _, ok := finishedRun(mock, first.RunID); ok {
		t.Fatal("draining run FINISHed while inflight > 0")
	}

	mgr.Release(first)
	mgr.Maintain(context.Background())

	eventually(t, "FINISH of released draining run", func() bool {
		f, ok := finishedRun(mock, first.RunID)
		return ok && f.Status == "completed" && f.TotalSteps == 1
	})
}

func TestMaintainSkipsDuringCooldown(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	// With the token in cooldown no upstream call may happen: no START and
	// no draining FINISH, and nothing may be logged (production observed a
	// maintain error line once per minute).
	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(run)

	mgr.Cooldown(time.Hour)

	restore, logged := captureSlog()
	defer restore()

	started := len(mock.StartedRunsSnapshot())
	finished := len(mock.FinishedRunsSnapshot())
	mgr.Maintain(context.Background())

	if got := len(mock.StartedRunsSnapshot()); got != started {
		t.Errorf("STARTs during cooldown = %d, want %d (no rotate)", got, started)
	}
	if got := len(mock.FinishedRunsSnapshot()); got != finished {
		t.Errorf("FINISHes during cooldown = %d, want %d (no drain)", got, finished)
	}
	if out := logged(); out != "" {
		t.Errorf("Maintain logged during cooldown:\n%s", out)
	}
}

// syncBuffer is a thread-safe bytes.Buffer wrapper so concurrent slog emissions
// and String() reads in parallel test workers don't race under -race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureSlog swaps the default slog handler for one recording Debug+
// messages and returns a restore func plus a snapshot of everything logged
// since capture. Used to assert that quiet paths emit no log lines.
func captureSlog() (restore func(), logged func() string) {
	buf := new(syncBuffer)
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return func() { slog.SetDefault(prev) }, func() string { return buf.String() }
}

// TestPrewarmIsNoop pins the per-turn-mint retirement of prewarming: Prewarm
// STARTs nothing (a pre-created run would be drained by the next mint without
// ever serving a chat), and the next Acquire mints the turn's run itself.
func TestPrewarmIsNoop(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	mgr.Prewarm(context.Background(), []string{agentA, agentB})
	if started := mock.StartedRunsSnapshot(); len(started) != 0 {
		t.Fatalf("STARTs after prewarm = %d, want 0 (per-turn mint, no pre-creation)", len(started))
	}

	// A second prewarm is equally inert.
	mgr.Prewarm(context.Background(), []string{agentA, agentB})
	if started := mock.StartedRunsSnapshot(); len(started) != 0 {
		t.Errorf("STARTs after second prewarm = %d, want still 0", len(started))
	}

	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if run.RunID != "run-0001" {
		t.Errorf("turn run id = %q, want run-0001 (the first START belongs to the turn)", run.RunID)
	}
	mgr.Release(run)
}

func TestConcurrentAcquireRelease(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Per-turn mint: every Acquire is its own START, so the hammer needs one
	// id per lease.
	ids := make([]string, 400)
	for i := range ids {
		ids[i] = fmt.Sprintf("run-%04d", i)
	}
	mock.RunIDs = ids
	mgr, _ := newTestManager(t, mock, time.Hour)

	const goroutines = 8
	const perGoroutine = 40
	var wg sync.WaitGroup
	var failures atomicError
	var mu sync.Mutex
	seen := make(map[string]bool)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			agent := agentA
			if g%2 == 1 {
				agent = agentB
			}
			for i := 0; i < perGoroutine; i++ {
				run, err := mgr.Acquire(context.Background(), agent)
				if err != nil {
					failures.set(err)
					continue
				}
				mu.Lock()
				if seen[run.RunID] {
					mu.Unlock()
					failures.set(fmt.Errorf("duplicate run id %s across turns", run.RunID))
					mgr.Release(run)
					continue
				}
				seen[run.RunID] = true
				mu.Unlock()
				mgr.Release(run)
			}
		}(g)
	}
	wg.Wait()

	if err := failures.get(); err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	if snap := mgr.Snapshot(); snap.Requests != goroutines*perGoroutine {
		t.Errorf("snapshot requests = %d, want %d", snap.Requests, goroutines*perGoroutine)
	}
	if got := len(mock.StartedRunsSnapshot()); got != goroutines*perGoroutine {
		t.Errorf("STARTs = %d, want %d (one START per turn)", got, goroutines*perGoroutine)
	}
}

// atomicError is a tiny thread-safe first-error holder for the hammer.
type atomicError struct {
	mu  sync.Mutex
	err error
}

func (e *atomicError) set(err error) {
	e.mu.Lock()
	if e.err == nil {
		e.err = err
	}
	e.mu.Unlock()
}

func (e *atomicError) get() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

func TestFinishAllRuns(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	lease, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(lease)

	mgr.FinishAllRuns(context.Background())
	snap := mgr.Snapshot()
	if snap.ActiveRuns != 0 {
		t.Errorf("ActiveRuns = %d, want 0 after FinishAllRuns", snap.ActiveRuns)
	}
}

func TestRateLimitAndBanCooldowns(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)
	rle := &upstream.RateLimitError{Body: "rate limit", RetryAfter: 5 * time.Minute}
	mgr.CooldownRateLimit(rle)

	if mgr.RateLimitError() == nil {
		t.Errorf("RateLimitError() = nil, want rate limit error")
	}

	// Cooldown expired in the past should automatically unlock (return nil)
	mgr.mu.Lock()
	mgr.cooldownUntil = time.Now().Add(-1 * time.Second)
	mgr.mu.Unlock()
	if mgr.RateLimitError() != nil {
		t.Errorf("RateLimitError() != nil for expired cooldown, want automatic unlock")
	}

	be := &upstream.BanError{Body: "account banned", ResumesAt: time.Now().Add(10 * time.Minute)}
	mgr.CooldownBan(be)

	if mgr.BanError() == nil {
		t.Errorf("BanError() = nil, want ban error")
	}

	snap := mgr.Snapshot()
	if snap.BanError == nil {
		t.Errorf("Snapshot.BanError = nil, want non-nil")
	}
}

func TestCountryBlockCooldown(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)
	cbe := &upstream.CountryBlockedError{CountryCode: "CN", CountryBlockReason: "region_restricted"}

	mgr.CooldownCountryBlocked(cbe)

	if got := mgr.CountryBlockedError(); got == nil || got.CountryCode != "CN" {
		t.Fatalf("CountryBlockedError() = %v, want remembered CN block", got)
		return
	}
	if until := mgr.CooldownUntil(); !time.Now().Before(until) || time.Until(until) > 16*time.Minute {
		t.Errorf("cooldown until = %v, want ~15m country window", until)
	}
	// The country block clears any remembered rate-limit/ban state so the
	// cooldown-skip surfaces the country error, not a stale 429.
	if mgr.RateLimitError() != nil || mgr.BanError() != nil {
		t.Errorf("rate/ban memory not cleared by country block")
	}

	// Acquire skips the token during the window (shared cooldown deadline).
	if _, err := mgr.Acquire(context.Background(), agentA); err == nil {
		t.Error("Acquire during country cooldown succeeded, want skip error")
	}

	// Expired country window unlocks like the rate-limit memory.
	mgr.mu.Lock()
	mgr.countryUntil = time.Now().Add(-1 * time.Second)
	mgr.mu.Unlock()
	if mgr.CountryBlockedError() != nil {
		t.Errorf("CountryBlockedError() != nil for expired window, want automatic unlock")
	}
}

func TestCountryBlockDoesNotDowngradeBan(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)
	be := &upstream.BanError{Body: "banned", ResumesAt: time.Now().Add(2 * time.Hour)}
	mgr.CooldownBan(be)
	cbe := &upstream.CountryBlockedError{CountryCode: "CN", CountryBlockReason: "region_restricted"}
	mgr.CooldownCountryBlocked(cbe)

	// A ban outranks a country block (pool precedence ban > country): the
	// ban memory and window must survive the country block.
	if mgr.BanError() == nil {
		t.Errorf("BanError() = nil after country block, ban must outrank country")
	}
	if mgr.CountryBlockedError() != nil {
		t.Errorf("CountryBlockedError() != nil, country must not overwrite an active ban")
	}
}

func TestInvalidateRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	lease, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(lease)

	mgr.Invalidate(agentA)
	snap := mgr.Snapshot()
	if snap.ActiveRuns != 0 {
		t.Errorf("ActiveRuns = %d after Invalidate, want 0", snap.ActiveRuns)
	}
}

// TestShutdownSkipsMidFinishRun is the regression guard for the
// double-FINISH race: rotate spawns an untracked finishIfReady goroutine
// that may be mid-FINISH (finishing=true, run on the draining list) when
// Shutdown gathers. Shutdown must skip those runs instead of calling
// FinishRun again for the same run id upstream.
func TestShutdownSkipsMidFinishRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	lease, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(lease)

	// Simulate the async rotator mid-FINISH for agentA's run (run-0001):
	// draining with the finishing flag set — finishIfReady's upstream
	// FINISH is in flight.
	mgr.mu.Lock()
	runA := mgr.runs[agentA]
	runA.finishing = true
	mgr.draining = append(mgr.draining, runA)
	mgr.mu.Unlock()

	// A second agent with a plain run must still be finished exactly once.
	leaseB, err := mgr.Acquire(context.Background(), agentB)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(leaseB)

	mgr.Shutdown(context.Background())

	finished := mock.FinishedRunsSnapshot()
	if len(finished) != 1 {
		t.Fatalf("finished runs = %v, want exactly 1 (only agentB's run)", finished)
	}
	if finished[0].RunID != "run-0002" {
		t.Errorf("finished run = %q, want run-0002 (agentB); mid-FINISH run-0001 must not be re-FINISHed", finished[0].RunID)
	}
	if snap := mgr.Snapshot(); snap.ActiveRuns != 0 {
		t.Errorf("active runs after shutdown = %d, want 0", snap.ActiveRuns)
	}
}

// TestAcquireConcurrentFinishAllRuns hammers Acquire against a concurrent
// idle FinishAllRuns: every Acquire mints its own turn through the shared
// path, so a cleared run map is re-populated by the mint itself — each
// acquire either completes or fails with a real error, never a phantom.
func TestAcquireConcurrentFinishAllRuns(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Generous id pool: acquires racing the idle FINISH can re-START the
	// run several times per cleared window.
	ids := make([]string, 2000)
	for i := range ids {
		ids[i] = fmt.Sprintf("run-%04d", i)
	}
	mock.RunIDs = ids
	mgr, _ := newTestManager(t, mock, time.Hour)

	const acquireGoroutines = 8
	const perGoroutine = 60
	var wg sync.WaitGroup
	var failures atomicError

	for g := 0; g < acquireGoroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				run, err := mgr.Acquire(context.Background(), agentA)
				if err != nil {
					failures.set(err)
					continue
				}
				mgr.Release(run)
			}
		}()
	}
	for i := 0; i < 60; i++ {
		mgr.FinishAllRuns(context.Background())
	}
	wg.Wait()

	if err := failures.get(); err != nil {
		t.Fatalf("acquire failed while FinishAllRuns raced: %v", err)
	}
}

// TestSnapshotBannedUntil surfaces the ban window deadline the pool uses to
// gate quarantined acquires (fixes the sticky quarantine after an
// expired ban).
func TestSnapshotBannedUntil(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	// InflightCount reflects outstanding leases (bridge eviction skips
	// busy entries on it).
	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if got := mgr.InflightCount(); got != 1 {
		t.Errorf("InflightCount with one lease = %d, want 1", got)
	}
	mgr.Release(run)
	if got := mgr.InflightCount(); got != 0 {
		t.Errorf("InflightCount after release = %d, want 0", got)
	}

	until := time.Now().Add(10 * time.Minute)
	mgr.CooldownBan(&upstream.BanError{Body: "banned", ResumesAt: until})

	snap := mgr.Snapshot()
	if snap.BanError == nil {
		t.Fatal("Snapshot.BanError = nil, want non-nil during the ban window")
		return
	}
	if !snap.BannedUntil.Equal(until) {
		t.Errorf("Snapshot.BannedUntil = %v, want %v", snap.BannedUntil, until)
	}
}

func TestClearCooldowns(t *testing.T) {
	m := NewRunManager(nil, nil, time.Hour)
	now := time.Now()
	m.Cooldown(time.Hour)
	m.CooldownRateLimit(&upstream.RateLimitError{RetryAfter: time.Hour})
	if m.RateLimitError() == nil {
		t.Fatal("expected rate-limit lock to be active")
		return
	}
	// A ban supersedes the rate-limit lock (mutually exclusive by design).
	m.CooldownBan(&upstream.BanError{ResumesAt: now.Add(2 * time.Hour)})
	if m.RateLimitError() != nil || m.BanError() == nil {
		t.Fatal("expected ban to supersede rate-limit lock")
		return
	}
	m.ClearCooldowns()
	if !m.CooldownUntil().IsZero() {
		t.Errorf("cooldown not cleared: %v", m.CooldownUntil())
	}
	if m.RateLimitError() != nil {
		t.Error("rate-limit lock not cleared")
	}
	if m.BanError() != nil {
		t.Error("ban window not cleared")
	}
}

// TestCooldownDeadlineCeiling pins the pass-through deadline rule: an
// upstream RetryAfter or far-future ResetAt lands on the token verbatim
// (no truncation to a small default, no wrap on absurd input — the
// upstream parse already saturates at the largest representable
// duration, so what runs receives always fits).
func TestCooldownDeadlineCeiling(t *testing.T) {
	m := NewRunManager(nil, nil, time.Hour)

	t.Run("huge RetryAfter passes through", func(t *testing.T) {
		m.ClearCooldowns()
		m.CooldownRateLimit(&upstream.RateLimitError{RetryAfter: 1000 * 24 * time.Hour})
		until := m.CooldownUntil()
		now := time.Now()
		// No truncation to a short default and no wrap to a
		// past/near deadline: the window stays ~1000d out.
		if until.Before(now.Add(900 * 24 * time.Hour)) {
			t.Errorf("CooldownUntil = %v, want ~1000d out (pass-through, not truncated/wrapped)", until)
		}
	})

	t.Run("far-future ResetAt passes through", func(t *testing.T) {
		m.ClearCooldowns()
		m.CooldownRateLimit(&upstream.RateLimitError{ResetAt: time.Now().Add(500 * 24 * time.Hour)})
		until := m.CooldownUntil()
		now := time.Now()
		if until.Before(now.Add(400*24*time.Hour)) || until.After(now.Add(600*24*time.Hour)) {
			t.Errorf("CooldownUntil = %v, want ~500d out (the far-future ResetAt)", until)
		}
	})

	t.Run("normal RetryAfter untouched", func(t *testing.T) {
		m.ClearCooldowns()
		m.CooldownRateLimit(&upstream.RateLimitError{RetryAfter: 5 * time.Minute})
		until := m.CooldownUntil()
		now := time.Now()
		if until.Before(now.Add(4*time.Minute)) || until.After(now.Add(6*time.Minute)) {
			t.Errorf("CooldownUntil = %v, want ~5m from now (unchanged)", until)
		}
	})
}

// TestConcurrentMintTurnRunsAreDistinct pins the retired single-flight: with
// per-turn mint there is no START coalescing — 20 concurrent turns mint 20
// distinct runs.
func TestConcurrentMintTurnRunsAreDistinct(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ids := make([]string, 40)
	for i := range ids {
		ids[i] = fmt.Sprintf("run-%04d", i)
	}
	mock.RunIDs = ids
	mgr, _ := newTestManager(t, mock, time.Hour)

	const concurrency = 20
	var wg sync.WaitGroup
	runs := make([]*Run, concurrency)
	errs := make([]error, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r, err := mgr.Acquire(context.Background(), agentA)
			runs[idx] = r
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d failed: %v", i, err)
		}
		if runs[i] == nil {
			t.Fatalf("goroutine %d returned nil run", i)
			return
		}
		if seen[runs[i].RunID] {
			t.Errorf("goroutine %d RunID = %q, want a distinct run per turn (no coalescing)", i, runs[i].RunID)
		}
		seen[runs[i].RunID] = true
		mgr.Release(runs[i])
	}

	if started := mock.StartedRunsSnapshot(); len(started) != concurrency {
		t.Errorf("StartedRuns count = %d, want %d (one START per turn)", len(started), concurrency)
	}
}

// TestAcquireAlwaysMintsFresh pins that sequential Acquires never reuse: no
// aging, no rotation interval — every turn mints.
func TestAcquireAlwaysMintsFresh(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	r1, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if r1.RunID != "run-0001" {
		t.Fatalf("first RunID = %q, want run-0001", r1.RunID)
	}
	mgr.Release(r1)

	r2, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if r2.RunID != "run-0002" {
		t.Fatalf("second RunID = %q, want run-0002 (no reuse without aging)", r2.RunID)
	}
	mgr.Release(r2)

	if started := mock.StartedRunsSnapshot(); len(started) != 2 {
		t.Fatalf("StartedRuns count = %d, want 2 (one START per turn)", len(started))
	}
	// The superseded first turn drains asynchronously.
	eventually(t, "FINISH of superseded turn", func() bool {
		_, ok := finishedRun(mock, "run-0001")
		return ok
	})
}

// ── Wave 1 issue tests (#80) ─────────────────────────────────────────────

// TestTraceSessionIDMintedPerRun verifies #80: each run mints a crypto/rand
// trace session id once and reuses it across the run's requests; a rotated
// run gets a fresh one.
// TestTraceSessionIDMintedPerRun verifies per-turn minting: each turn mints a
// fresh crypto/rand trace session id AND client id, while the turn's own steps
// share the run (llm_step_number 1..N on one run_id).
func TestTraceSessionIDMintedPerRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if run.TraceSessionID == "" {
		t.Fatal("TraceSessionID empty, want minted UUID per run")
	}
	if run.ClientID == "" {
		t.Fatal("ClientID empty, want minted id per run")
	}
	// Tool steps within the turn share the run: stamps 1, 2 on the same run.
	if got := run.NextStepNumber(); got != 1 {
		t.Fatalf("first stamp = %d, want 1", got)
	}
	if got := run.NextStepNumber(); got != 2 {
		t.Fatalf("second stamp = %d, want 2", got)
	}
	firstTrace, firstClient := run.TraceSessionID, run.ClientID
	mgr.Release(run)

	// The next turn mints fresh ids — never reused across turns.
	run2, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if run2.TraceSessionID == "" || run2.TraceSessionID == firstTrace {
		t.Errorf("TraceSessionID = %q after mint, want a fresh id (was %q)", run2.TraceSessionID, firstTrace)
	}
	if run2.ClientID == "" || run2.ClientID == firstClient {
		t.Errorf("ClientID = %q after mint, want a fresh id (was %q)", run2.ClientID, firstClient)
	}
	mgr.Release(run2)
}

// lockedBuffer is a mutex-guarded bytes.Buffer for captureSlogLocked: the
// deferred finish worker logs asynchronously while the test reads, so the
// underlying buffer must not be written concurrently with a read.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *lockedBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *lockedBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// captureSlogLocked swaps the default slog handler for a locked Debug-level
// recorder and returns a restore func plus a snapshot of everything logged
// since capture.
func captureSlogLocked() (restore func(), logged func() string) {
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return func() { slog.SetDefault(prev) }, buf.String
}

// TestRunStartedFinishedLogTraceSessionID verifies the run's
// trace_session_id (the value threaded into codebuff_metadata) appears on
// BOTH "runs: run started" and "runs: run finished" with the same value.
// TestRunStartedFinishedLogTraceSessionID verifies the run's
// trace_session_id (the value threaded into codebuff_metadata) appears on
// BOTH "runs: run started" and "runs: run finished" with the same value.
func TestRunStartedFinishedLogTraceSessionID(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	restore, logged := captureSlogLocked()
	defer restore()

	first, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(first)
	// The next turn mints (per-turn mint, no aging): the first turn drains
	// and FINISHes asynchronously through the deferred queue.
	second, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(second)

	startedRe := regexp.MustCompile(`runs: run started[^\n]*trace_session_id=([0-9a-f-]+)`)
	started := startedRe.FindStringSubmatch(logged())
	if started == nil {
		t.Fatalf("no run started line with trace_session_id:\n%s", logged())
		return
	}

	eventually(t, "run finished line", func() bool {
		return strings.Contains(logged(), "runs: run finished")
	})
	finishedRe := regexp.MustCompile(`runs: run finished[^\n]*trace_session_id=([0-9a-f-]+)`)
	finished := finishedRe.FindStringSubmatch(logged())
	if finished == nil {
		t.Fatalf("run finished line missing trace_session_id:\n%s", logged())
		return
	}
	if finished[1] != started[1] {
		t.Errorf("run finished trace_session_id = %q, want the run started value %q", finished[1], started[1])
	}
}

// ── Runs lifecycle telemetry ───────────────────────────────────────────

// TestRunFinishedLogCarriesLifecycleAttrs verifies that the "runs: run
// finished" record carries duration_ms (run lifetime), steps (the run's
// in-memory recorded step count), and termination ("finish" via the FINISH
// queue), alongside the existing run_id/requests/trace_session_id fields.
func TestRunFinishedLogCarriesLifecycleAttrs(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	restore, logged := captureSlogLocked()
	defer restore()

	first, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.RecordStep(first, "chatcmpl-1")
	mgr.RecordStep(first, "chatcmpl-2")
	mgr.Release(first)
	// The next turn mints (per-turn mint, no aging): the first turn drains
	// and FINISHes asynchronously through the deferred queue.
	second, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(second)

	eventually(t, "run finished record", func() bool {
		return strings.Contains(logged(), "runs: run finished")
	})

	re := regexp.MustCompile(`runs: run finished[^\n]*duration_ms=([0-9]+) steps=([0-9]+) status=([a-z]+) termination=([a-z]+)`)
	m := re.FindStringSubmatch(logged())
	if m == nil {
		t.Fatalf("run finished record missing lifecycle attrs:\n%s", logged())
		return
	}
	if m[4] != "finish" {
		t.Errorf("termination = %q, want finish (FINISH queue path)", m[4])
	}
	if m[2] != "2" {
		t.Errorf("steps = %s, want 2 (recorded before the next mint)", m[2])
	}
	if m[3] != "completed" {
		t.Errorf("status = %q, want completed (no failure or abandon on this run)", m[3])
	}
	duration, err := strconv.Atoi(m[1])
	if err != nil || duration < 0 {
		t.Errorf("duration_ms = %q, want a non-negative integer", m[1])
	}
}

// TestRunFinishedDropLogsTermination verifies the drop arm: a
// draining run force-dropped without FINISH (DrainTTL, issue #55) emits the
// same "runs: run finished" record with termination=drop, while the existing
// TTL-expired warn keeps its run_id/agent_id/age fields unchanged.
func TestRunFinishedDropLogsTermination(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManagerOpts(t, mock, Options{
		RotationInterval: time.Hour,
		DrainTTL:         50 * time.Millisecond,
	})

	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.RecordStep(run, "chatcmpl-1")
	mgr.Release(run)

	// Age the run past its draining TTL so Maintain's prune pass drops it
	// without FINISH (mirrors a persistently failing FINISH, issue #55).
	mgr.mu.Lock()
	run.drainedAt = time.Now().Add(-time.Hour)
	mgr.draining = append(mgr.draining, run)
	mgr.mu.Unlock()

	restore, logged := captureSlogLocked()
	defer restore()
	mgr.Maintain(context.Background())

	out := logged()
	if !strings.Contains(out, "runs: dropping draining run (TTL expired)") {
		t.Fatalf("expected TTL-expired drop warn:\n%s", out)
	}
	// The existing warn keeps its run_id/agent_id/age fields (unchanged).
	warnRe := regexp.MustCompile(`runs: dropping draining run \(TTL expired\)[^\n]*run_id=run-0001[^\n]*agent_id=agent-alpha[^\n]*age=`)
	if !warnRe.MatchString(out) {
		t.Errorf("TTL-expired warn lost its fields:\n%s", out)
	}
	dropRe := regexp.MustCompile(`runs: run finished[^\n]*duration_ms=[0-9]+ steps=([0-9]+) status=([a-z]+) termination=drop`)
	m := dropRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no run finished drop record:\n%s", out)
		return
	}
	if m[1] != "1" {
		t.Errorf("dropped run steps = %s, want 1", m[1])
	}
	if m[2] != "completed" {
		t.Errorf("dropped run status = %s, want completed (never failed or abandoned)", m[2])
	}
	// Dropped without FINISH: nothing may have reached the upstream.
	if got := len(mock.FinishedRunsSnapshot()); got != 0 {
		t.Errorf("dropped run must not be FINISHed upstream, got %d finished runs", got)
	}
}

// TestRestartMintsFreshRun pins the retired store-resume: a restart never
// adopts the persisted run — the next turn mints fresh (per-turn mint, one
// START per prompt). The persisted record is overwritten by the new mint.
func TestRestartMintsFreshRun(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	mock := testutil.NewMock()
	defer mock.Close()
	store := session.NewStore(t.TempDir() + "/state.json")

	// First process: START a turn (persisted).
	mgr1, _ := newTestManagerOpts(t, mock, Options{RotationInterval: time.Hour, Store: store})
	run, err := mgr1.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr1.Release(run)
	mgr1.Shutdown(context.Background())

	// Second process (restart) on the same store: the next turn mints fresh
	// instead of adopting the persisted run.
	mgr2, _ := newTestManagerOpts(t, mock, Options{RotationInterval: time.Hour, Store: store})
	fresh, err := mgr2.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.RunID == run.RunID {
		t.Errorf("restart acquired persisted run %s, want a fresh mint (no adopt)", run.RunID)
	}
	if fresh.TraceSessionID == run.TraceSessionID {
		t.Errorf("restart reused persisted trace session id %q, want a fresh id", run.TraceSessionID)
	}
	if started := mock.StartedRunsSnapshot(); len(started) != 2 {
		t.Errorf("STARTs after restart = %d, want 2 (fresh mint, no adopt)", len(started))
	}
	mgr2.Release(fresh)
	mgr2.Shutdown(context.Background())
}

// TestReleaseAbandonedLogsCancelled verifies ReleaseAbandoned's lifecycle
// record: the last-lease abandon logs run_id/agent/status=cancelled with the
// in-flight count at drop time (0 by construction), and the run's eventual
// FINISH reports the cancelled status upstream instead of completed.
func TestReleaseAbandonedLogsCancelled(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	restore, logged := captureSlogLocked()
	defer restore()
	mgr.ReleaseAbandoned(run)

	abandonRe := regexp.MustCompile(`runs: run abandoned[^\n]*run_id=(run-[0-9]+)[^\n]*agent=agent-alpha[^\n]*status=(cancelled|failed)[^\n]*inflight=([0-9]+)`)
	m := abandonRe.FindStringSubmatch(logged())
	if m == nil {
		t.Fatalf("no run abandoned record:\n%s", logged())
		return
	}
	if m[1] != run.RunID {
		t.Errorf("abandoned run_id = %q, want %q", m[1], run.RunID)
	}
	if m[2] != "cancelled" {
		t.Errorf("abandoned status = %q, want cancelled", m[2])
	}
	if m[3] != "0" {
		t.Errorf("abandoned inflight = %s, want 0 (last lease dropped)", m[3])
	}
	eventually(t, "cancelled FINISH of abandoned run", func() bool {
		f, ok := finishedRun(mock, run.RunID)
		return ok && f.Status == "cancelled"
	})
	mgr.Shutdown(context.Background())
}

// TestShutdownAbandonWarnLogsFields verifies that the shutdown drain
// abandoning WARN logs pending_jobs/runs/key instead of the whole manager
// struct (a *RunManager dump would leak internal state to the log).
// TestShutdownAbandonWarnLogsFields verifies that the shutdown drain
// abandoning WARN logs pending_jobs/runs/key instead of the whole manager
// struct (a *RunManager dump would leak internal state to the log).
func TestShutdownAbandonWarnLogsFields(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SetFinishDelay(250 * time.Millisecond)
	// Four sequential turns (five ids staged); the default pool holds three.
	mock.RunIDs = append([]string{"run-0001", "run-0002", "run-0003", "run-0004", "run-0005"}, mock.RunIDs...)
	mgr, _ := newTestManager(t, mock, time.Hour)

	// Each mint drains its predecessor, so after four turns the worker is
	// mid-FINISH (slow mock) with more FINISHes queued when Shutdown's
	// deadline expires.
	for i := 0; i < 4; i++ {
		r, err := mgr.Acquire(context.Background(), agentA)
		if err != nil {
			t.Fatal(err)
		}
		mgr.Release(r)
	}

	// The worker is mid-FINISH (250ms slow mock) when Shutdown abandons,
	// so at least one finish job must still be queued; the logged values
	// must match what the queue/runs hold at that moment.
	mgr.mu.Lock()
	wantPending := len(mgr.finishQueue)
	wantRuns := len(mgr.runs)
	mgr.mu.Unlock()
	if wantPending < 1 {
		t.Fatalf("setup: queued finish jobs = %d, want >= 1 (worker must be busy)", wantPending)
	}

	restore, logged := captureSlogLocked()
	defer restore()
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	mgr.Shutdown(expired)

	out := logged()
	re := regexp.MustCompile(`runs: finish-queue drain exceeded shutdown deadline; abandoning remaining jobs[^\n]*pending_jobs=([0-9]+) runs=([0-9]+) key=([0-9a-f]+)`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("abandon warn missing pending_jobs/runs/key:\n%s", out)
		return
	}
	if m[1] != fmt.Sprint(wantPending) || m[2] != fmt.Sprint(wantRuns) {
		t.Errorf("abandon warn pending_jobs/runs = %s/%s, want %d/%d", m[1], m[2], wantPending, wantRuns)
	}
	if m[3] == "" {
		t.Error("abandon warn key empty")
	}
	if strings.Contains(out, "manager=") {
		t.Error("abandon warn leaked the manager struct (manager= attr present)")
	}

	// The worker must still drain and exit so no goroutine outlives the
	// mock server.
	select {
	case <-mgr.finishExited:
	case <-time.After(3 * time.Second):
		t.Fatal("finish worker did not exit after shutdown abandon")
	}
}
