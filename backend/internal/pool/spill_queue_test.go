package pool

import (
	"context"
	"errors"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSpillWaitsFullQueueWaitBeforeTouchingNextAccount proves the spill
// half of R3: with account #1's 2 slots held, a third request parks on
// #1's lane — account #2 sees zero contact before QUEUE_WAIT elapses —
// then the waiter spills and is granted on #2 with QueueWait>0.
func TestSpillWaitsFullQueueWaitBeforeTouchingNextAccount(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newSmartTestPool(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueWait = 1500 * time.Millisecond
		c.QueueDepth = 16
	}, mock0, mock1)
	// Warm: the two holder slots grant instantly; the third still parks
	// the full wait because the head lane is full, not because it is cold.
	smartWarmLane(t, p, modelA, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	l1, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("lease 1: %v", err)
	}
	defer p.LeaseRelease(l1)
	l2, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("lease 2: %v", err)
	}
	defer p.LeaseRelease(l2)
	thirdCh := make(chan *Lease, 1)
	go func() {
		l, err := p.Acquire(ctx, modelA)
		if err != nil {
			return
		}
		thirdCh <- l
	}()
	select {
	case <-thirdCh:
		t.Fatal("third acquire granted while #1 slots held, want parked")
	case <-time.After(400 * time.Millisecond):
	}
	if n := mock1.RequestsSnapshot(); n != 0 {
		t.Fatalf("account #2 touched (%d requests) before QUEUE_WAIT elapsed, want 0 (no eager spill)", n)
	}
	select {
	case l3 := <-thirdCh:
		defer p.LeaseRelease(l3)
		if l3.Token != 1 {
			t.Fatalf("spilled lease on account #%d, want #2", l3.Token+1)
		}
		if l3.QueueWait <= 0 {
			t.Fatalf("spilled lease QueueWait=%v, want >0 (waited out #1 first)", l3.QueueWait)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("third acquire never spilled to #2 after QUEUE_WAIT")
	}
}

// TestSpillBurst5TwoAccounts is the MASQ R3 burst keeper: 5 concurrent
// requests over 2 accounts (cap 2 per (account, model), short lane
// budget, leases held) converge to 4 leases split 2/2 plus exactly one
// end-of-chain 429: the first two arrivals take #1's lanes, the rest
// park on #1, and only after #1's QUEUE_WAIT elapses do they spill to
// #2 — two grant there (QueueWait>0 proves they waited out #1 first)
// and the last finds every lane full and surfaces the existing 429
// shape. One upstream session serves each account (creates==1). The
// counts hold under any arrival order: whichever goroutines arrive
// first take #1's fast-path slots (QueueWait==0), any two of the
// remaining three take #2's spilled slots, and one always loses.
func TestSpillBurst5TwoAccounts(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newSmartTestPool(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueWait = 1500 * time.Millisecond
		c.QueueDepth = 16
	}, mock0, mock1)
	// Warm: lane #1 serves its two fast-path slots instantly (QueueWait==0
	// proves it); lane #2 stays cold so the rest spill only after #1's
	// wait elapses.
	smartWarmLane(t, p, modelA, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	type result struct {
		lease *Lease
		err   error
	}
	burst := make(chan result, 5)
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := p.Acquire(ctx, modelA)
			burst <- result{l, err}
		}()
	}
	wg.Wait()
	close(burst)
	var granted []*Lease
	var exhausted []error
	for r := range burst {
		if r.err != nil {
			exhausted = append(exhausted, r.err)
			continue
		}
		granted = append(granted, r.lease)
	}
	defer func() {
		for _, l := range granted {
			p.LeaseRelease(l)
		}
	}()
	if len(granted) != 4 || len(exhausted) != 1 {
		t.Fatalf("burst = %d leases + %d errors, want 4 + 1 (2x2 slots, 5 contenders)", len(granted), len(exhausted))
	}
	var rle *upstream.RateLimitError
	if !errors.As(exhausted[0], &rle) {
		t.Fatalf("loser err = %T %v, want end-of-chain *upstream.RateLimitError", exhausted[0], exhausted[0])
	}
	if !strings.Contains(rle.Body, "queue wait") {
		t.Errorf("loser body = %q, want queue-wait wording", rle.Body)
	}
	on0, on1 := 0, 0
	for _, l := range granted {
		switch l.Token {
		case 0:
			on0++
			if l.QueueWait != 0 {
				t.Errorf("account #1 lease parked (%v), want fast-path grant (capacity free at arrival)", l.QueueWait)
			}
		case 1:
			on1++
			if l.QueueWait <= 0 {
				t.Errorf("account #2 lease QueueWait=%v, want >0 (spilled only after #1's wait elapsed)", l.QueueWait)
			}
		default:
			t.Fatalf("burst lease on account #%d, want #1/#2", l.Token+1)
		}
	}
	if on0 != 2 || on1 != 2 {
		t.Fatalf("burst split #%d/#%d, want 2/2", on0, on1)
	}
	if got := mock0.SessionCreatesSnapshot(); got != 1 {
		t.Fatalf("account #1 creates = %d, want 1 (one session shared)", got)
	}
	if got := mock1.SessionCreatesSnapshot(); got != 1 {
		t.Fatalf("account #2 creates = %d, want 1 (one session shared)", got)
	}
}
