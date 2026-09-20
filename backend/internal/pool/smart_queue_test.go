package pool

// smart_queue_test.go — behavior keepers for the smart model queue
// (model_queue.go): global FIFO per model, arrival scan, work-conserving
// handoff, scale-out after QUEUE_WAIT, and I5 same-lane requeue priority.
// All short-tier safe: no test parks a 30s wait.

import (
	"context"
	"errors"
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// smartWarmLane provisions an already-usable session on one lane without
// taking a slot (direct session-manager admission, no park): the smart
// model queue grants instantly only on a free slot PLUS a usable session,
// so measuring tests warm the lanes whose slot mechanics they exercise.
// Cold admission is the spill/queue-wait tests' business.
func smartWarmLane(t *testing.T, p *Pool, model string, idx int) {
	t.Helper()
	toks := p.roster.Load()
	if idx < 0 || idx >= len(*toks) || (*toks)[idx] == nil {
		t.Fatalf("smart warm-up lane %d out of range (roster %d)", idx, len(*toks))
	}
	if _, err := (*toks)[idx].session.EnsureSessionForModel(context.Background(), model); err != nil {
		t.Fatalf("smart warm-up lane %d: %v", idx, err)
	}
}

// TestSmartBurstGrant proves the arrival scan absorbs a burst: 4 concurrent
// requests over 2 warm accounts all grant with QueueWait==0 despite a huge
// QUEUE_WAIT — nobody parks while warm slots stand open.
func TestSmartBurstGrant(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newSmartTestPool(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueWait = 120 * time.Second
		c.QueueDepth = 16
	}, mock0, mock1)
	smartWarmLane(t, p, modelA, 0)
	smartWarmLane(t, p, modelA, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const n = 4
	type result struct {
		lease *Lease
		err   error
	}
	resCh := make(chan result, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := p.Acquire(ctx, modelA)
			resCh <- result{l, err}
		}()
	}
	wg.Wait()
	close(resCh)
	on0, on1 := 0, 0
	var held []*Lease
	for r := range resCh {
		if r.err != nil {
			t.Fatalf("burst acquire: %v (want zero errors)", r.err)
		}
		held = append(held, r.lease)
		switch r.lease.Token {
		case 0:
			on0++
		case 1:
			on1++
		default:
			t.Fatalf("burst lease on account #%d, want #1/#2", r.lease.Token+1)
		}
		if r.lease.QueueWait != 0 {
			t.Errorf("burst lease parked (%v), want QueueWait==0 (warm slots open)", r.lease.QueueWait)
		}
	}
	defer func() {
		for _, l := range held {
			p.LeaseRelease(l)
		}
	}()
	if on0 != 2 || on1 != 2 {
		t.Fatalf("burst split #%d/#%d, want 2/2 (scan fills in index order)", on0, on1)
	}
	if got := mock0.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("account #1 creates = %d, want 1 (warm session reused, zero admission)", got)
	}
	if got := mock1.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("account #2 creates = %d, want 1 (warm session reused, zero admission)", got)
	}
}

// TestSmartFIFOAcrossLanes proves the handoff is work-conserving across
// lanes: with one holder per lane and two waiters queued, freeing EITHER
// lane grants the queue head — the head takes the lane-#2 slot when #2
// frees first, and the next waiter takes lane #1 after.
func TestSmartFIFOAcrossLanes(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newSmartTestPool(t, func(c *config.Config) {
		c.SlotsPerAccount = 1
	}, mock0, mock1)
	smartWarmLane(t, p, modelA, 0)
	smartWarmLane(t, p, modelA, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a1, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("holder 1: %v", err)
	}
	if a1.Token != 0 {
		t.Fatalf("holder 1 token = %d, want 0 (scan fills in index order)", a1.Token)
	}
	a2, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("holder 2: %v", err)
	}
	if a2.Token != 1 {
		t.Fatalf("holder 2 token = %d, want 1 (lane #1 full, warm lane #2 serves)", a2.Token)
	}
	wCh := make(chan acquireResult, 2)
	for range 2 {
		go func() {
			l, err := p.Acquire(ctx, modelA)
			wCh <- acquireResult{l, err}
		}()
	}
	eventually(t, "both waiters park", func() bool { return p.modelQueueDepth(modelA) == 2 })

	// Free lane #2 first: the head must take it (not wait for lane #1).
	p.LeaseRelease(a2)
	var w1 *Lease
	select {
	case r := <-wCh:
		if r.err != nil {
			t.Fatalf("head waiter err = %v", r.err)
		}
		w1 = r.lease
	case <-time.After(5 * time.Second):
		t.Fatal("head waiter never granted after lane #2 freed")
	}
	defer p.LeaseRelease(w1)
	if w1.Token != 1 {
		t.Errorf("head waiter token = %d, want 1 (freed lane grants the head)", w1.Token)
	}
	if w1.QueueWait <= 0 {
		t.Errorf("head waiter QueueWait = %v, want >0 (parked FIFO)", w1.QueueWait)
	}

	// Free lane #1: the next waiter takes it.
	p.LeaseRelease(a1)
	select {
	case r := <-wCh:
		if r.err != nil {
			t.Fatalf("second waiter err = %v", r.err)
		}
		defer p.LeaseRelease(r.lease)
		if r.lease.Token != 0 {
			t.Errorf("second waiter token = %d, want 0 (freed lane grants the head)", r.lease.Token)
		}
		if r.lease.QueueWait <= 0 {
			t.Errorf("second waiter QueueWait = %v, want >0 (parked FIFO)", r.lease.QueueWait)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second waiter never granted after lane #1 freed")
	}
}

// TestSmartColdScaleOutAfterWait proves a cold pool is work-conserving:
// the first arrival admits lane #1 instantly (session created inline,
// zero park — no QUEUE_WAIT paid before scale-out); the next arrival
// (lane #1 full) parks, then spills to lane #2 after QUEUE_WAIT.
func TestSmartColdScaleOutAfterWait(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newSmartTestPool(t, func(c *config.Config) {
		c.SlotsPerAccount = 1
		c.QueueWait = 300 * time.Millisecond
	}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Cold arrival grants instantly: no park, session created inline.
	h1, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("first acquire err = %v", err)
	}
	defer p.LeaseRelease(h1)
	if h1.Token != 0 {
		t.Errorf("first lease token = %d, want 0 (scale-out admits in index order)", h1.Token)
	}
	if h1.QueueWait != 0 {
		t.Errorf("first lease QueueWait = %v, want 0 (cold admits instantly, no park)", h1.QueueWait)
	}
	if got := mock0.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("account #1 creates = %d, want 1 (one session created inline)", got)
	}

	// Lane #1 full: the next arrival parks, then spills to lane #2 after
	// QUEUE_WAIT.

	wCh := make(chan acquireResult, 1)
	go func() {
		l, err := p.Acquire(ctx, modelA)
		wCh <- acquireResult{l, err}
	}()
	eventually(t, "second arrival parks on full lane #1", func() bool { return p.modelQueueDepth(modelA) == 1 })
	select {
	case r := <-wCh:
		if r.err != nil {
			t.Fatalf("second acquire err = %v", r.err)
		}
		defer p.LeaseRelease(r.lease)
		if r.lease.Token != 1 {
			t.Errorf("second lease token = %d, want 1 (lane #1 full, spill admits lane #2)", r.lease.Token)
		}
		if r.lease.QueueWait <= 0 {
			t.Errorf("second lease QueueWait = %v, want >0 (waited out the queue)", r.lease.QueueWait)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second acquire never spilled to lane #2 after QUEUE_WAIT")
	}
	if got := mock0.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("account #1 creates = %d, want 1 (one session shared)", got)
	}
	if got := mock1.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("account #2 creates = %d, want 1 (one session shared)", got)
	}
}

// TestSmartEndOfChain429 proves the timeout path surfaces the existing
// 429 shape when every lane stays full past QUEUE_WAIT: warm holders on
// both lanes, one waiter parks, and only the wait's expiry produces the
// refusal — never eager contact, never a hang.
func TestSmartEndOfChain429(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newSmartTestPool(t, func(c *config.Config) {
		c.SlotsPerAccount = 1
		c.QueueWait = 300 * time.Millisecond
	}, mock0, mock1)
	smartWarmLane(t, p, modelA, 0)
	smartWarmLane(t, p, modelA, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h1, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("holder 1: %v", err)
	}
	defer p.LeaseRelease(h1)
	h2, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("holder 2: %v", err)
	}
	defer p.LeaseRelease(h2)

	_, err = p.Acquire(ctx, modelA)
	if err == nil {
		t.Fatal("waiter acquire succeeded on a full pool, want end-of-chain 429")
	}
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("waiter err = %T %v, want *upstream.RateLimitError", err, err)
	}
	if !strings.Contains(rle.Body, "queue wait") {
		t.Errorf("waiter body = %q, want queue-wait wording", rle.Body)
	}
	if rle.RetryAfter <= 0 {
		t.Error("end-of-chain 429 carries no Retry-After hint")
	}
}

// TestSmartI5HeadPriority proves a same-lane quota requeue outranks fresh
// arrivals: the jailed request's lane continuity beats the queue — it
// grants before requests that parked while it slept, with no failover.
// (Without the own-lane retake, the requeue would park a second full wait
// and spill onward to lane #2.)
func TestSmartI5HeadPriority(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	var posts0 atomic.Int64
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && posts0.Add(1) == 1 {
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"status":"rate_limited","retryAfterMs":250}`)
			return
		}
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-i5","expiresAt":"2026-09-18T07:00:00.000Z"}`)
	}
	p := newSmartTestPool(t, func(c *config.Config) {
		c.SlotsPerAccount = 1
		c.QueueWait = 400 * time.Millisecond
	}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// The jailed arrival runs async: cold admits lane #1 instantly, the
	// 250ms jail is waited out on the lane (slot held), and the retry
	// consumes the held permit.
	aCh := make(chan acquireResult, 1)
	go func() {
		l, err := p.Acquire(ctx, modelA)
		aCh <- acquireResult{l, err}
	}()
	// Two fresh arrivals start mid-jail (after the refusal POST, before
	// the retry): both park behind the sleeper.
	eventually(t, "refusal POST lands", func() bool { return posts0.Load() == 1 })
	wCh := make(chan acquireResult, 2)
	go func() {
		l, err := p.Acquire(ctx, modelA)
		wCh <- acquireResult{l, err}
	}()
	eventually(t, "first fresh arrival parks", func() bool { return p.modelQueueDepth(modelA) == 1 })
	// Stagger the second arrival so its QUEUE_WAIT deadline lands safely
	// after the handoff cascade below (both deadlines still fire inside
	// the test's 5s windows).
	time.Sleep(100 * time.Millisecond)
	go func() {
		l, err := p.Acquire(ctx, modelA)
		wCh <- acquireResult{l, err}
	}()
	eventually(t, "second fresh arrival parks", func() bool { return p.modelQueueDepth(modelA) == 2 })

	// The requeue grants first: its lane continuity outranks the parked
	// queue (neither waiter can grant before this release — cap 1).
	var jailed *Lease
	select {
	case r := <-aCh:
		if r.err != nil {
			t.Fatalf("jailed acquire err = %v", r.err)
		}
		jailed = r.lease
	case <-time.After(5 * time.Second):
		t.Fatal("jailed acquire never retried its lane after the jail")
	}
	defer p.LeaseRelease(jailed)
	if jailed.Token != 0 {
		t.Errorf("jailed lease token = %d, want 0 (same-lane requeue, no failover)", jailed.Token)
	}
	if jailed.QueueWait < 250*time.Millisecond {
		t.Errorf("jailed lease QueueWait = %v, want >= 250ms (jail rides telemetry; cold admits instantly, no park)", jailed.QueueWait)
	}
	if n := mock1.RequestsSnapshot(); n != 0 {
		t.Errorf("account #2 requests = %d, want 0 (no failover walk)", n)
	}
	if got := posts0.Load(); got != 2 {
		t.Errorf("account #1 session POSTs = %d, want 2 (refusal + same-lane retry)", got)
	}

	// The parked fresh arrivals grant in head order: the first takes lane
	// #1's freed slot via handoff; the second outlives its own QUEUE_WAIT
	// with lane #1 held and scales out to lane #2 (single deadline from
	// enqueue, then spill-style scale-out — not a second full park).
	p.LeaseRelease(jailed)
	for i := range 2 {
		select {
		case r := <-wCh:
			if r.err != nil {
				t.Fatalf("fresh waiter %d err = %v", i, r.err)
			}
			defer p.LeaseRelease(r.lease)
			if want := i; r.lease.Token != want {
				t.Errorf("fresh waiter %d token = %d, want %d (head-first handoff, then scale-out on expiry)", i, r.lease.Token, want)
			}
			if r.lease.QueueWait <= 0 {
				t.Errorf("fresh waiter %d QueueWait = %v, want >0 (parked FIFO)", i, r.lease.QueueWait)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("fresh waiter %d never granted", i)
		}
	}
}
