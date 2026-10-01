package runs

import (
	"context"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
)

// TestConcurrentMintHoldsPredecessorFinishUntilRelease is the regression guard
// for the 2026-10-01 live 502: two concurrent turns minted runs f9e3 then
// e793 on the same token+agent. The second mint pushed f9e3 to draining, and
// the async worker FINISHed it 124ms later while turn 1's chat was still in
// flight — the mint tracked the run unleased (inflight 0), so
// finishIfReadyCtx saw a finishable run and upstream rejected turn 1's chat
// with 400 runId Not Running. Leased mints (Acquire, inflight 1) hold the
// predecessor's FINISH until the holding turn Releases.
func TestConcurrentMintHoldsPredecessorFinishUntilRelease(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	// Turn 1 mints and HOLDS its lease (chat in flight): no Release.
	first, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	// Turn 2 mints concurrently: the predecessor drains, but its async
	// FINISH must wait for turn 1's Release.
	second, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Release(second)
	if second.RunID == first.RunID {
		t.Fatalf("turns shared run %s, want a fresh run per turn", first.RunID)
	}

	// Drive the drain through both paths: the maintain tick re-enqueues the
	// deferred FINISH and the background worker owns it.
	mgr.Maintain(context.Background())

	// The draining run has an outstanding lease: the worker must observe
	// inflight > 0 and skip it. No exposed state signals "the worker ran
	// and skipped", so this negative assertion is bounded by wall time:
	// 400ms gives a buggy implementation that ignores the lease ample
	// chance to FINISH.
	time.Sleep(400 * time.Millisecond)
	if _, ok := finishedRun(mock, first.RunID); ok {
		t.Fatal("draining run FINISHed while its turn still held the lease")
	}

	// Turn 1's chat ends: the Release re-queues the deferred FINISH.
	mgr.Release(first)

	eventually(t, "FINISH of released draining run", func() bool {
		f, ok := finishedRun(mock, first.RunID)
		return ok && f.Status == "completed" && f.TotalSteps == 1
	})

	// Exactly once: the mint-time enqueue, the maintain re-enqueue, and the
	// release re-queue must not FINISH the run twice (enqueue dedupes, and
	// the finishing flag pins the run after its FINISH dispatches).
	finishes := 0
	for _, f := range mock.FinishedRunsSnapshot() {
		if f.RunID == first.RunID {
			finishes++
		}
	}
	if finishes != 1 {
		t.Errorf("FINISHes of %s = %d, want exactly 1", first.RunID, finishes)
	}
}
