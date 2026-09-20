package runs

// Regression guards for the shutdown concurrency fixes:
//
//   - FinishAllRuns must NOT drop runs with an outstanding lease: they are
//     marked draining and kept alive so the LAST lease release re-queues
//     their FINISH (previously they were deleted from both sets first and
//     became unreachable — finishIfReadyCtx skips inflight > 0 and nothing
//     ever re-queued them, leaking the upstream run and losing its
//     terminal status).
//   - Once Shutdown begins, no new run may be STARTed: an in-flight
//     request still in its acquire phase must not rotate a fresh run into
//     the cleared manager after the finish worker stopped (the run would
//     never be FINISHed). rotate refuses with ErrShuttingDown and
//     discards (inline-FINISHing) a run whose upstream START completed
//     after the drain began.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/session"
	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
)

// TestFinishAllRunsDefersInflightRuns is the regression guard for the
// anti-ban contract: FinishAllRuns used to delete every run from m.runs and
// clear m.draining BEFORE calling finishIfReadyCtx, which skips runs with
// inflight > 0 — so a run with an outstanding lease was never FINISHed
// (Release only decremented, Maintain iterates the current sets, and the
// run was in neither). The drain must keep the in-flight run alive and
// defer its FINISH to the last lease release.
func TestFinishAllRunsDefersInflightRuns(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	lease, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	// Record a step so the deferred FINISH's totalSteps is honest (issue
	// #114) and we can pin the exact payload.
	mgr.RecordStep(lease, "msg-1")

	mgr.FinishAllRuns(context.Background())

	// The in-flight run is neither finished by the drain nor dropped: it
	// stays tracked so the last lease release can re-queue the FINISH.
	if snap := mgr.Snapshot(); snap.ActiveRuns != 1 {
		t.Fatalf("ActiveRuns after FinishAllRuns with in-flight lease = %d, want 1 (run kept alive)", snap.ActiveRuns)
	}
	if _, ok := finishedRun(mock, "run-0001"); ok {
		t.Fatal("in-flight run was FINISHed by FinishAllRuns, want it deferred to the last lease release")
	}

	// The last release re-queues the deferred FINISH: the run must reach
	// upstream with its recorded terminal status and step total.
	mgr.Release(lease)

	eventually(t, "FINISH of deferred in-flight run after release", func() bool {
		f, ok := finishedRun(mock, "run-0001")
		return ok && f.Status == "completed" && f.TotalSteps == 1 && len(f.Steps) == 1
	})
	if snap := mgr.Snapshot(); snap.ActiveRuns != 0 {
		t.Errorf("ActiveRuns after release + FINISH = %d, want 0", snap.ActiveRuns)
	}
}

// TestFinishAllRunsDeferredRunCancelledOnAbandon pins the ReleaseAbandoned
// half of the fix: a run deferred by FinishAllRuns whose last lease is
// abandoned must FINISH as "cancelled" (not leak, and not report
// completed — issue #114), with the drained run dropped from the active
// set before the re-queue so finishIfReadyCtx can run it.
func TestFinishAllRunsDeferredRunCancelledOnAbandon(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	lease, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}

	mgr.FinishAllRuns(context.Background())
	mgr.ReleaseAbandoned(lease)

	eventually(t, "cancelled FINISH of deferred run after abandon", func() bool {
		f, ok := finishedRun(mock, "run-0001")
		return ok && f.Status == "cancelled"
	})
	if snap := mgr.Snapshot(); snap.ActiveRuns != 0 {
		t.Errorf("ActiveRuns after abandon + FINISH = %d, want 0", snap.ActiveRuns)
	}
}

// TestAcquireAfterShutdownRefusesStart is the regression guard for the
// acquire-after-shutdown gap: a request reaching the acquire phase after
// Shutdown drained the manager must not START a fresh run — the finish
// worker is stopped, so that run would never be FINISHed. rotate refuses
// with ErrShuttingDown; nothing is tracked and no FINISH is attempted.
func TestAcquireAfterShutdownRefusesStart(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	// A pre-shutdown run is drained normally by Shutdown.
	lease, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(lease)
	mgr.Shutdown(context.Background())

	startedBefore := len(mock.StartedRunsSnapshot())
	finishedBefore := len(mock.FinishedRunsSnapshot())

	_, err = mgr.Acquire(context.Background(), agentA)
	if !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("Acquire after Shutdown = %v, want ErrShuttingDown", err)
	}
	if snap := mgr.Snapshot(); snap.ActiveRuns != 0 {
		t.Errorf("ActiveRuns after post-shutdown acquire = %d, want 0 (no phantom run tracked)", snap.ActiveRuns)
	}
	if started := len(mock.StartedRunsSnapshot()); started != startedBefore {
		t.Errorf("STARTs grew after Shutdown: %d -> %d, want no new run start", startedBefore, started)
	}
	if finished := len(mock.FinishedRunsSnapshot()); finished != finishedBefore {
		t.Errorf("FINISHes grew after Shutdown: %d -> %d, want no finish attempt", finishedBefore, finished)
	}
}

// raceServer is a minimal upstream that blocks the agent-runs START until
// released and records FINISHes — used to hold a rotate's upstream StartRun
// in flight across Shutdown (the in-flight-acquire race).
type raceServer struct {
	srv           *httptest.Server
	startReceived chan struct{} // closed when the START lands
	release       chan struct{} // close to let the blocked START respond
	startOnce     sync.Once

	mu       sync.Mutex
	finishes []raceFinish
}

type raceFinish struct {
	runID  string
	status string
}

func newRaceServer(t *testing.T) *raceServer {
	s := &raceServer{
		startReceived: make(chan struct{}),
		release:       make(chan struct{}),
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *raceServer) handle(w http.ResponseWriter, r *http.Request) {
	all, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agent-runs":
		var payload struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(all, &payload)
		switch payload.Action {
		case "START":
			s.startOnce.Do(func() { close(s.startReceived) })
			<-s.release
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"runId":"race-run-1"}`)
		case "FINISH":
			var f struct {
				RunID  string `json:"runId"`
				Status string `json:"status"`
			}
			_ = json.Unmarshal(all, &f)
			s.mu.Lock()
			s.finishes = append(s.finishes, raceFinish{runID: f.RunID, status: f.Status})
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}
	case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/freebuff/session":
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

func (s *raceServer) finishSnapshot() []raceFinish {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]raceFinish(nil), s.finishes...)
}

// TestRotateDiscardsRunStartedDuringShutdown pins the hard half of the fix: the
// upstream START is already in flight when Shutdown begins. rotate must
// re-check the shutting-down flag after StartRun returns, discard the fresh
// run (never track it), best-effort FINISH it inline so it does not leak
// upstream, and surface ErrShuttingDown to the acquiring request.
func TestRotateDiscardsRunStartedDuringShutdown(t *testing.T) {
	srv := newRaceServer(t)

	client, err := upstream.New("tok", &config.Config{
		UpstreamBaseURL:    srv.srv.URL,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RotationInterval:   6 * time.Hour,
		RegistryRefresh:    6 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	sess := session.NewManager(client)
	mgr := NewRunManager(client, sess, time.Hour)

	acquireDone := make(chan error, 1)
	go func() {
		_, err := mgr.Acquire(context.Background(), agentA)
		acquireDone <- err
	}()

	// The rotate's upstream START is in flight; shut the manager down
	// before it completes.
	<-srv.startReceived
	mgr.Shutdown(context.Background())

	if snap := mgr.Snapshot(); snap.ActiveRuns != 0 {
		t.Fatalf("ActiveRuns while START in flight = %d, want 0 (fresh run must not be tracked)", snap.ActiveRuns)
	}

	// Let the blocked START complete: rotate sees the shutdown flag,
	// discards the run, inline-FINISHes it, and fails the acquire.
	close(srv.release)

	select {
	case err := <-acquireDone:
		if !errors.Is(err, ErrShuttingDown) {
			t.Fatalf("Acquire racing Shutdown = %v, want ErrShuttingDown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire did not return after the START-during-shutdown race")
	}

	if snap := mgr.Snapshot(); snap.ActiveRuns != 0 {
		t.Errorf("ActiveRuns after discarded START = %d, want 0", snap.ActiveRuns)
	}

	// The discarded run was FINISHed inline exactly once (the anti-leak):
	// upstream must not keep an orphaned agent run alive until its own
	// rotation expiry.
	finishes := srv.finishSnapshot()
	if len(finishes) != 1 {
		t.Fatalf("FINISHes for discarded run = %v, want exactly 1 (inline FINISH of race-run-1)", finishes)
	}
	if finishes[0].runID != "race-run-1" || finishes[0].status != "completed" {
		t.Errorf("discarded-run FINISH = %+v, want race-run-1 completed", finishes[0])
	}
}

// TestFinishAllRunsFailedFinishRelists pins the failure half of the
// FinishAllRuns drain contract: idle runs are detached from m.runs /
// m.draining BEFORE finishIfReadyCtx attempts the upstream FINISH, so a
// failed attempt must re-list the run on the draining set
// (appendDrainingLocked dedupes) instead of orphaning it outside both
// sets — otherwise Maintain can never retry it and the run's terminal
// status is lost upstream (anti-ban contract).
func TestFinishAllRunsFailedFinishRelists(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SetFinishFailures(1)
	mgr, _ := newTestManager(t, mock, time.Hour)

	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(run) // idle active run; plain Release does not enqueue a FINISH

	// The synchronous FINISH inside FinishAllRuns fails (budget = 1); the
	// run must land on the draining list, not vanish.
	mgr.FinishAllRuns(context.Background())

	eventually(t, "failed FinishAllRuns FINISH relists run as draining", func() bool {
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		for _, d := range mgr.draining {
			if d == run {
				return true
			}
		}
		return false
	})
	// First (failing) attempt fully observed before nudging Maintain, so
	// the finishing guard cannot swallow the retry.
	eventually(t, "first failed FINISH attempt observed", func() bool {
		if mock.FinishesStartedSnapshot() < 1 {
			return false
		}
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		return !run.finishing
	})

	mgr.Maintain(context.Background())
	eventually(t, "relisted run FINISHed after maintain retry", func() bool {
		f, ok := finishedRun(mock, run.RunID)
		return ok && f.Status == "completed"
	})
	mgr.Shutdown(context.Background())
}
