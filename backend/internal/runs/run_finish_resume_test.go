package runs

// Regression for the store-resume race: the persisted run record must die
// when the FINISH is DISPATCHED, not when the FINISH response lands. Live
// 2026-09-21T07:05:05Z: request 8d76a5bb's abandoned run 87c3a9c3 had its
// FINISH dispatched (client had gone away), and 117ms later the next
// request's rotate() adopted the still-persisted record, so upstream
// answered its chats 400 "runId Not Running" → client-visible 502.

import (
	"context"
	"freebucks-proxy/backend/internal/session"
	"freebucks-proxy/backend/internal/testutil"
	"testing"
	"time"
)

// TestDrainedRunRecordRemovedAtFinishDispatch pins the invariant that a
// draining run is never resumable: while its FINISH is still in flight the
// persisted record is already gone, so a concurrent Acquire STARTs a fresh
// run instead of adopting the dying one from the store.
func TestDrainedRunRecordRemovedAtFinishDispatch(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	store := session.NewStore(t.TempDir() + "/state.json")

	mgr, _ := newTestManagerOpts(t, mock, Options{RotationInterval: time.Hour, Store: store})
	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if pr := store.LoadRun(mgr.key, agentA); pr == nil || pr.RunID != run.RunID {
		t.Fatalf("run not persisted after START (got %+v)", pr)
	}

	// Hold the FINISH in flight, then abandon the run's last lease — the
	// exact live trigger (client closed its connection on release).
	mock.SetFinishDelay(2 * time.Second)
	mgr.ReleaseAbandoned(run)

	eventually(t, "FINISH dispatched", func() bool {
		return mock.FinishesStartedSnapshot() >= 1
	})
	if _, done := finishedRun(mock, run.RunID); done {
		t.Fatalf("FINISH of %s already completed; the record check below would not be in-flight", run.RunID)
	}
	if pr := store.LoadRun(mgr.key, agentA); pr != nil {
		t.Fatalf("draining run still persisted while its FINISH is in flight: %+v", pr)
	}

	// A second Acquire must START a fresh run: resuming the drained record
	// is what upstream rejects with 400 "runId Not Running".
	second, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if second.RunID == run.RunID {
		t.Errorf("Acquire resumed drained run %s; want a fresh run", second.RunID)
	}
	if started := mock.StartedRunsSnapshot(); len(started) != 2 {
		t.Errorf("STARTs = %d, want 2 (the drained run must not be resumed)", len(started))
	}

	// Let the held FINISH land so no worker goroutine outlives the test.
	mock.SetFinishDelay(0)
	eventually(t, "FINISH lands", func() bool {
		_, ok := finishedRun(mock, run.RunID)
		return ok
	})
	mgr.Release(second)
	mgr.Shutdown(context.Background())
}
