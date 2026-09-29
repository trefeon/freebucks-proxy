package pool

import (
	"context"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPinBurst5OneAccount is the MASQ I4 burst keeper: slot 0 pinned to
// modelA absorbs a 5-burst for modelA alone — 2 live plus 3 parked FIFO on
// the pinned lane — while slot 1 (pinned to modelB) sees zero contact and
// no client-visible error surfaces.
func TestPinBurst5OneAccount(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newSmartTestPool(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueWait = 30 * time.Second
		c.QueueDepth = 16
		c.PinModel = map[int]string{0: modelA, 1: modelB}
	}, mock0, mock1)
	// Warm lane #1 (pinned to modelA): the holders grant instantly while
	// lane #2 stays cold and pinned away, so the burst queues on #1.
	smartWarmLane(t, p, modelA, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	hold1, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("holder 1: %v", err)
	}
	if hold1.Token != 0 {
		t.Fatalf("holder 1 token = %d, want pinned slot 0", hold1.Token)
	}
	hold2, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("holder 2: %v", err)
	}
	if hold2.Token != 0 {
		t.Fatalf("holder 2 token = %d, want pinned slot 0", hold2.Token)
	}

	type result struct {
		lease *Lease
		err   error
	}
	parked := make(chan result, 3)
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := p.Acquire(ctx, modelA)
			parked <- result{l, err}
		}()
	}
	// All three park on the pinned lane; the other slot is never touched.
	eventually(t, "3 waiters parked on slot 0", func() bool {
		snaps := p.Snapshot()
		return len(snaps) == 2 && snaps[0].QueuedWaiters == 3 && snaps[1].QueuedWaiters == 0
	})
	if n := mock1.RequestsSnapshot(); n != 0 {
		t.Fatalf("slot 1 requests while burst parks = %d, want 0 (pinned lane never spills)", n)
	}
	p.LeaseRelease(hold1)
	p.LeaseRelease(hold2)
	// Collect incrementally, releasing each grant so the last waiter can
	// take its slot (2 slots, 3 waiters — holding every grant deadlocks).
	granted := make([]*Lease, 0, 3)
	for range 3 {
		select {
		case r := <-parked:
			if r.err != nil {
				t.Fatalf("parked acquire: %v", r.err)
			}
			granted = append(granted, r.lease)
			p.LeaseRelease(r.lease)
		case <-ctx.Done():
			t.Fatalf("timed out waiting for parked grants: %v", ctx.Err())
		}
	}
	wg.Wait()
	for _, l := range granted {
		if l.Token != 0 {
			t.Errorf("burst lease token = %d, want pinned slot 0", l.Token)
		}
		if l.QueueWait <= 0 {
			t.Errorf("burst lease QueueWait = %v, want >0 (parked on the pin)", l.QueueWait)
		}
	}
	if n := mock1.SessionCreatesSnapshot(); n != 0 {
		t.Errorf("slot 1 session creates = %d, want 0", n)
	}
	if n := mock1.RequestsSnapshot(); n != 0 {
		t.Errorf("slot 1 requests = %d, want 0", n)
	}
	if got := mock0.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("slot 0 creates = %d, want 1 (one session shared)", got)
	}
}

// TestPinLeavesOtherModelSlotsFree pins slot 0 to modelA and proves modelB
// traffic still lands on the free slot with zero contact on the pinned one
// — a pin never starves other models' slots.
func TestPinLeavesOtherModelSlotsFree(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.PinModel = map[int]string{0: modelA}
	}, mock0, mock1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	leaseB, err := p.Acquire(ctx, modelB)
	if err != nil {
		t.Fatalf("acquire %s with slot 0 pinned away: %v", modelB, err)
	}
	if leaseB.Token != 1 {
		t.Errorf("acquire %s leased token %d, want free slot 1", modelB, leaseB.Token)
	}
	p.LeaseRelease(leaseB)
	if n := mock0.SessionCreatesSnapshot(); n != 0 {
		t.Errorf("pinned slot creates during other-model acquire = %d, want 0", n)
	}

	leaseA, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire %s: %v", modelA, err)
	}
	if leaseA.Token != 0 {
		t.Errorf("acquire %s leased token %d, want pinned slot 0", modelA, leaseA.Token)
	}
	p.LeaseRelease(leaseA)
}

// TestPinFailFast pins every slot away from the requested model: Acquire
// fails with a routing error naming the model and attempts no upstream
// admission at all.
func TestPinFailFast(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.PinModel = map[int]string{0: modelA, 1: modelA}
	}, mock0, mock1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := p.Acquire(ctx, modelB)
	if err == nil {
		t.Fatalf("acquire %s with all slots pinned away succeeded, want fail-fast", modelB)
		return
	}
	if !strings.Contains(err.Error(), modelB) || !strings.Contains(err.Error(), "no account pinned to model") {
		t.Errorf("fail-fast error = %q, want it to name the model and the pin", err)
	}
	if n := mock0.SessionCreatesSnapshot() + mock1.SessionCreatesSnapshot(); n != 0 {
		t.Errorf("session creates during pinned-out acquire = %d, want 0 (no admission attempted)", n)
	}
	if n := len(mock0.StartedRunsSnapshot()) + len(mock1.StartedRunsSnapshot()); n != 0 {
		t.Errorf("run STARTs during pinned-out acquire = %d, want 0", n)
	}
}

// TestPinSkipsCounted pins the observability counter: every pin-skip
// decision increments the slot's pinSkips, surfaced on the snapshot for
// cards and metrics.
func TestPinSkipsCounted(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.PinModel = map[int]string{0: modelA, 1: modelA}
	}, mock0, mock1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire %s: %v", modelA, err)
	}
	p.LeaseRelease(lease)

	snaps := p.Snapshot()
	if len(snaps) != 2 {
		t.Fatalf("snapshots = %d, want 2", len(snaps))
	}
	if snaps[0].PinnedModel != modelA {
		t.Errorf("slot 0 PinnedModel = %q, want %s", snaps[0].PinnedModel, modelA)
	}
	// The fail-fast probe below exercises the loop gate on both slots.
	if _, err := p.Acquire(ctx, modelB); err == nil {
		t.Fatalf("acquire %s with all slots pinned away succeeded, want fail-fast", modelB)
		return
	}
	snaps = p.Snapshot()
	if snaps[0].PinSkips == 0 || snaps[1].PinSkips == 0 {
		t.Errorf("PinSkips = [%d %d], want both >0 after pinned-out acquire",
			snaps[0].PinSkips, snaps[1].PinSkips)
	}
}
