package pool

// queue_wait_test.go — queue-wait telemetry. A request that genuinely parks
// behind a full live-turn lane must report its wait on the request (the
// phasetiming accumulator that rides ctx, which the server puts on the chat
// trace) and on the lease; a request that never parked must report NEITHER
// (the phase is absent, not zero, so the console's "no wait" case stays
// unambiguous); and a waiter whose QUEUE_WAIT elapsed never held a slot, so
// it must not claim a wait either.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/phasetiming"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// parkHold is the bounded hold kept while the waiter is already parked on
// the FIFO queue. It only makes the park measurable at millisecond
// resolution — the waiter's arrival is proven by routeSlotQueued before the
// hold, so nothing races on it.
const parkHold = 25 * time.Millisecond

// waitForParkedWaiter waits until the lane reports exactly one parked
// waiter (the goroutine has genuinely queued, not merely started).
func waitForParkedWaiter(t *testing.T, p *Pool, key any) {
	t.Helper()
	eventually(t, "waiter parks on the live-turn queue", func() bool { return p.routeSlotQueued(key) == 1 })
}

// TestQueueWaitRecordedWhenParkedThenGranted proves the granted-after-park
// path reports its wait: the lease carries it and the request's phase
// accumulator carries queue_wait_ms.
func TestQueueWaitRecordedWhenParkedThenGranted(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newSmartTestPool(t, func(c *config.Config) { c.TokenMaxConcurrent = 1 }, mock)
	entry := smartEntry(p, 0)

	holder, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	ctx, phases := phasetiming.WithContext(context.Background())
	parkedCh := make(chan acquireResult, 1)
	go func() {
		lease, err := p.Acquire(ctx, modelA)
		parkedCh <- acquireResult{lease, err}
	}()
	waitForParkedWaiter(t, p, entry)
	time.Sleep(parkHold)
	p.LeaseRelease(holder)

	var parked *Lease
	select {
	case r := <-parkedCh:
		if r.err != nil {
			t.Fatalf("parked acquire err = %v", r.err)
		}
		parked = r.lease
	case <-time.After(5 * time.Second):
		t.Fatal("parked acquire never granted after the holder released")
	}
	defer p.LeaseRelease(parked)

	if parked.QueueWait < parkHold {
		t.Errorf("lease queue wait = %v, want >= %v (the park was held that long)", parked.QueueWait, parkHold)
	}
	wait, ok := phases.All()[phasetiming.QueueWaitMS]
	if !ok {
		t.Fatalf("parked request records no %s phase: %v", phasetiming.QueueWaitMS, phases.All())
	}
	if wait < parkHold.Milliseconds() {
		t.Errorf("%s = %dms, want >= %dms", phasetiming.QueueWaitMS, wait, parkHold.Milliseconds())
	}
	if wait > 5_000 {
		t.Errorf("%s = %dms, want a bounded wait", phasetiming.QueueWaitMS, wait)
	}
}

// TestQueueWaitAbsentWhenNeverParked proves an immediate grant reports no
// wait at all: no phase key and a zero lease wait.
func TestQueueWaitAbsentWhenNeverParked(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newSmartTestPool(t, nil, mock)

	ctx, phases := phasetiming.WithContext(context.Background())
	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatal(err)
	}
	defer p.LeaseRelease(lease)

	if lease.QueueWait != 0 {
		t.Errorf("never-parked lease queue wait = %v, want 0", lease.QueueWait)
	}
	if wait, ok := phases.All()[phasetiming.QueueWaitMS]; ok {
		t.Errorf("never-parked request recorded %s = %d, want the phase absent", phasetiming.QueueWaitMS, wait)
	}
}

// TestQueueWaitAbsentAfterQueueWaitTimeout proves a waiter whose QUEUE_WAIT
// elapsed claims no wait: it never held a slot, so no wait was granted.
func TestQueueWaitAbsentAfterQueueWaitTimeout(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newSmartTestPool(t, func(c *config.Config) {
		c.TokenMaxConcurrent = 1
		c.QueueWait = 80 * time.Millisecond
	}, mock)

	holder, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	defer p.LeaseRelease(holder)

	ctx, phases := phasetiming.WithContext(context.Background())
	lease, err := p.Acquire(ctx, modelA)
	if err == nil {
		p.LeaseRelease(lease)
		t.Fatal("timed-out waiter acquire succeeded, want the queue-exhausted 429")
	}
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("timed-out waiter err = %T %v, want *upstream.RateLimitError", err, err)
	}
	if wait, ok := phases.All()[phasetiming.QueueWaitMS]; ok {
		t.Errorf("timed-out waiter recorded %s = %d, want the phase absent (no slot was granted)", phasetiming.QueueWaitMS, wait)
	}
}

// TestBridgeQueueWaitRecordedWhenParkedThenGranted is the bridge half: the
// bridge lane keys the slot state off the *bridgeEntry, and its lease must
// report the park identically.
func TestBridgeQueueWaitRecordedWhenParkedThenGranted(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgeSmartPool(t, func(c *config.Config) { c.TokenMaxConcurrent = 1 }, mock)
	const token = "bridge-queue-wait"
	entry := bridgeLane(t, p, token)

	holder, err := p.AcquireBridge(context.Background(), token, modelA)
	if err != nil {
		t.Fatal(err)
	}
	ctx, phases := phasetiming.WithContext(context.Background())
	parkedCh := make(chan acquireResult, 1)
	go func() {
		lease, err := p.AcquireBridge(ctx, token, modelA)
		parkedCh <- acquireResult{lease, err}
	}()
	waitForParkedWaiter(t, p, entry)
	time.Sleep(parkHold)
	p.LeaseRelease(holder)

	select {
	case r := <-parkedCh:
		if r.err != nil {
			t.Fatalf("parked bridge acquire err = %v", r.err)
		}
		defer p.LeaseRelease(r.lease)
		if r.lease.QueueWait < parkHold {
			t.Errorf("bridge lease queue wait = %v, want >= %v", r.lease.QueueWait, parkHold)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parked bridge acquire never granted after the holder released")
	}
	wait, ok := phases.All()[phasetiming.QueueWaitMS]
	if !ok {
		t.Fatalf("parked bridge request records no %s phase: %v", phasetiming.QueueWaitMS, phases.All())
	}
	if wait < parkHold.Milliseconds() {
		t.Errorf("bridge %s = %dms, want >= %dms", phasetiming.QueueWaitMS, wait, parkHold.Milliseconds())
	}
}

// TestLeaseAcquiredLineReportsQueueWait pins the secondary surface (the log
// table view): the granted-park lease line reports the wait and marks the
// park, while the never-parked line carries neither. Before this telemetry
// existed, a granted park left no line at all — the only queue line was the
// timeout/exhausted skip, so an operator could not tell a queued admission
// from a slow one.
func TestLeaseAcquiredLineReportsQueueWait(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newSmartTestPool(t, func(c *config.Config) { c.TokenMaxConcurrent = 1 }, mock)
	var sink bytes.Buffer
	p.logger = slog.New(slog.NewTextHandler(&sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	entry := smartEntry(p, 0)

	holder, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	parkedCh := make(chan acquireResult, 1)
	go func() {
		lease, err := p.Acquire(context.Background(), modelA)
		parkedCh <- acquireResult{lease, err}
	}()
	waitForParkedWaiter(t, p, entry)
	time.Sleep(parkHold)
	p.LeaseRelease(holder)

	select {
	case r := <-parkedCh:
		if r.err != nil {
			t.Fatalf("parked acquire err = %v", r.err)
		}
		p.LeaseRelease(r.lease)
	case <-time.After(5 * time.Second):
		t.Fatal("parked acquire never granted after the holder released")
	}

	var leaseLines []string
	for _, line := range strings.Split(sink.String(), "\n") {
		if strings.Contains(line, "pool: lease acquired") {
			leaseLines = append(leaseLines, line)
		}
	}
	if len(leaseLines) != 2 {
		t.Fatalf("lease-acquired lines = %d, want 2 (holder + parked)\n%s", len(leaseLines), sink.String())
	}
	if strings.Contains(leaseLines[0], "queue_parked") || strings.Contains(leaseLines[0], "queue_wait_ms") {
		t.Errorf("never-parked lease line claims a wait: %s", leaseLines[0])
	}
	if !strings.Contains(leaseLines[1], "queue_parked=true") {
		t.Errorf("granted-park lease line does not mark the park: %s", leaseLines[1])
	}
	if !strings.Contains(leaseLines[1], "queue_wait_ms=") {
		t.Errorf("granted-park lease line carries no queue_wait_ms: %s", leaseLines[1])
	}
}
