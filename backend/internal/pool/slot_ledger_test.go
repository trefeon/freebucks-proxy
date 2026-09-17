package pool

// MASQ slot-ledger tests: per (account, model) live-turn slots with FIFO
// queues. Slot gating is unconditional (SLOTS_PER_ACCOUNT=0 disables it);
// these tests prove the ledger path.

import (
	"context"
	"errors"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
	"net/http"
	"strings"
	"testing"
	"time"
)

// newSmartTestPool wires mocks through newTestPoolCfg with explicit MASQ
// routing knobs (hand-built configs bypass Load, so every knob under test
// is set here, never inferred).
func newSmartTestPool(t *testing.T, mut func(*config.Config), mocks ...*testutil.MockUpstream) *Pool {
	t.Helper()
	return newTestPoolCfg(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueWait = 30 * time.Second
		c.QueueDepth = 16
		if mut != nil {
			mut(c)
		}
	}, mocks...)
}

func smartEntry(p *Pool, idx int) *tokenEntry {
	return (*p.roster.Load())[idx]
}

// TestSlotRacingCapWaits proves the cap-2 hard wall on one token:
// three racing acquires never produce a third live turn — the third parks
// until a release wakes it.
func TestSlotRacingCapWaits(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newSmartTestPool(t, nil, mock)
	entry := smartEntry(p, 0)

	first, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.slotLive(slotKey{entry: entry, model: modelA}); got != 2 {
		t.Fatalf("live slots = %d, want 2", got)
	}

	type result struct {
		lease *Lease
		err   error
	}
	thirdCh := make(chan result, 1)
	go func() {
		lease, err := p.Acquire(context.Background(), modelA)
		thirdCh <- result{lease, err}
	}()
	eventually(t, "third acquire parks", func() bool { return p.slotQueued(slotKey{entry: entry, model: modelA}) == 1 })
	if got := p.slotLive(slotKey{entry: entry, model: modelA}); got != 2 {
		t.Fatalf("live slots while parked = %d, want 2 (never a third live turn)", got)
	}
	select {
	case r := <-thirdCh:
		t.Fatalf("third acquire returned early: lease=%v err=%v", r.lease, r.err)
	case <-time.After(150 * time.Millisecond):
	}
	if got := p.slotLive(slotKey{entry: entry, model: modelA}); got != 2 {
		t.Fatalf("live slots after wait = %d, want 2", got)
	}
	p.LeaseRelease(first)
	var third *Lease
	select {
	case r := <-thirdCh:
		if r.err != nil {
			t.Fatalf("third acquire err = %v", r.err)
		}
		if r.lease.Token != 0 {
			t.Errorf("third lease token = %d, want 0", r.lease.Token)
		}
		if r.lease.routeSlot == nil {
			t.Error("third lease carries no live-turn slot")
		}
		third = r.lease
	case <-time.After(5 * time.Second):
		t.Fatal("third acquire never woke after release")
	}
	if got := p.slotLive(slotKey{entry: entry, model: modelA}); got != 2 {
		t.Errorf("live slots after grant = %d, want 2", got)
	}
	p.LeaseRelease(second)
	p.LeaseRelease(third)
	eventually(t, "slots drain", func() bool { return p.slotLive(slotKey{entry: entry, model: modelA}) == 0 })
}

// TestSlotFIFOOrder proves waiters are granted in arrival order. The
// per-model election gate serializes same-model contenders before they
// reach the slot queue (as with the create/chat gates today), so the test
// stages arrivals: A parks, the holder releases, A is granted, then B
// arrives and must wait behind A — B is granted only after A releases.
func TestSlotFIFOOrder(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newSmartTestPool(t, func(c *config.Config) { c.SlotsPerAccount = 1 }, mock)
	entry := smartEntry(p, 0)

	holder, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		lease *Lease
		err   error
	}
	aCh := make(chan result, 1)
	go func() {
		lease, err := p.Acquire(context.Background(), modelA)
		aCh <- result{lease, err}
	}()
	eventually(t, "waiter A parks", func() bool { return p.slotQueued(slotKey{entry: entry, model: modelA}) == 1 })

	p.LeaseRelease(holder)
	var leaseA *Lease
	select {
	case r := <-aCh:
		if r.err != nil {
			t.Fatalf("waiter A err = %v", r.err)
		}
		leaseA = r.lease
	case <-time.After(5 * time.Second):
		t.Fatal("waiter A never granted")
	}

	bCh := make(chan result, 1)
	go func() {
		lease, err := p.Acquire(context.Background(), modelA)
		bCh <- result{lease, err}
	}()
	eventually(t, "waiter B parks behind A", func() bool { return p.slotQueued(slotKey{entry: entry, model: modelA}) == 1 })
	select {
	case r := <-bCh:
		t.Fatalf("waiter B granted before A released: %v %v", r.lease, r.err)
	default:
	}
	p.LeaseRelease(leaseA)
	select {
	case r := <-bCh:
		if r.err != nil {
			t.Fatalf("waiter B err = %v", r.err)
		}
		p.LeaseRelease(r.lease)
	case <-time.After(5 * time.Second):
		t.Fatal("waiter B never granted after A released")
	}
	eventually(t, "slots drain", func() bool { return p.slotLive(slotKey{entry: entry, model: modelA}) == 0 })
}

// TestSlotQueueWaitExpiry proves a parked waiter fails over with the
// existing 429 shape once QUEUE_WAIT elapses.
func TestSlotQueueWaitExpiry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newSmartTestPool(t, func(c *config.Config) {
		c.SlotsPerAccount = 1
		c.QueueWait = 120 * time.Millisecond
	}, mock)
	entry := smartEntry(p, 0)

	holder, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	defer p.LeaseRelease(holder)

	_, err = p.Acquire(context.Background(), modelA)
	if err == nil {
		t.Fatal("waiter acquire succeeded, want queue-wait expiry")
	}
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("expiry err = %T %v, want *upstream.RateLimitError", err, err)
	}
	if !strings.Contains(rle.Body, "queue wait") {
		t.Errorf("expiry body = %q, want queue-wait wording", rle.Body)
	}
	if rle.RetryAfter <= 0 {
		t.Error("expiry 429 carries no Retry-After hint")
	}
	if got := p.slotLive(slotKey{entry: entry, model: modelA}); got != 1 {
		t.Errorf("live slots after expiry = %d, want 1", got)
	}
	p.LeaseRelease(holder)
}

// TestSlotQueueOverflow429 proves a full queue fails over at once
// with the existing 429 shape while the parked waiter still waits.
func TestSlotQueueOverflow429(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newSmartTestPool(t, func(c *config.Config) {
		c.SlotsPerAccount = 1
		c.QueueDepth = 1
	}, mock)
	entry := smartEntry(p, 0)

	holder, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		lease *Lease
		err   error
	}
	parkedCh := make(chan result, 1)
	go func() {
		lease, err := p.Acquire(context.Background(), modelA)
		parkedCh <- result{lease, err}
	}()
	eventually(t, "waiter parks", func() bool { return p.slotQueued(slotKey{entry: entry, model: modelA}) == 1 })

	// The overflow contender asks for the SAME model: lanes are keyed per
	// (account, model), so a different model would take its own empty lane
	// instead of contending this full one. The same-model contender finds
	// the lane full (live 1 + 1 waiter at depth 1) and must fail over at
	// once.
	_, err = p.Acquire(context.Background(), modelA)
	if err == nil {
		t.Fatal("overflow acquire succeeded, want immediate 429")
	}
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("overflow err = %T %v, want *upstream.RateLimitError", err, err)
	}
	if !strings.Contains(rle.Body, "queue full") {
		t.Errorf("overflow body = %q, want queue-full wording", rle.Body)
	}
	select {
	case r := <-parkedCh:
		t.Fatalf("parked waiter returned early: %v %v", r.lease, r.err)
	default:
	}

	p.LeaseRelease(holder)
	select {
	case r := <-parkedCh:
		if r.err != nil {
			t.Fatalf("parked waiter err = %v", r.err)
		}
		p.LeaseRelease(r.lease)
	case <-time.After(5 * time.Second):
		t.Fatal("parked waiter never granted after release")
	}
}

// TestSlotSignals unit-proves the gate: fast-path grant, timeout and
// overflow reasons, and caller-ctx expiry.
func TestSlotSignals(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newSmartTestPool(t, nil, mock)
	entry := smartEntry(p, 0)
	ctx := context.Background()

	held, parked, err := p.slotAcquire(ctx, slotKey{entry: entry, model: modelA}, 1, 1, 16, 30*time.Second)
	if err != nil || parked || held == nil {
		t.Fatalf("fast path = %v/%v/%v, want permit/no-park/nil", held, parked, err)
	}
	// Timeout signal.
	_, parked, err = p.slotAcquire(ctx, slotKey{entry: entry, model: modelA}, 1, 1, 16, 60*time.Millisecond)
	var qerr *slotQueueExhaustedError
	if !errors.As(err, &qerr) || qerr.Reason != "timeout" {
		t.Fatalf("wait expiry = %v, want timeout queue-exhausted", err)
	}
	if !parked {
		t.Error("expiry parked = false, want true (the caller waited)")
	}
	// Overflow signal: one parked waiter fills depth 1.
	type wresult struct {
		permit *slotPermit
		err    error
	}
	waiting := make(chan wresult, 1)
	go func() {
		permit, _, err := p.slotAcquire(ctx, slotKey{entry: entry, model: modelA}, 1, 1, 1, 30*time.Second)
		waiting <- wresult{permit, err}
	}()
	eventually(t, "gate waiter parks", func() bool { return p.slotQueued(slotKey{entry: entry, model: modelA}) == 1 })
	_, parked, err = p.slotAcquire(ctx, slotKey{entry: entry, model: modelA}, 1, 1, 1, 30*time.Second)
	if !errors.As(err, &qerr) || qerr.Reason != "full" {
		t.Fatalf("overflow = %v, want full queue-exhausted", err)
	}
	if parked {
		t.Error("overflow parked = true, want false (immediate failover)")
	}
	// Caller ctx expiry surfaces ctx.Err, never the queue signal.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, _, err = p.slotAcquire(cancelled, slotKey{entry: entry, model: modelA}, 1, 1, 16, 30*time.Second)
	if !errors.Is(err, context.Canceled) || slotIsQueueExhausted(err) {
		t.Fatalf("cancelled wait = %v, want context.Canceled", err)
	}
	held.Release()
	select {
	case wr := <-waiting:
		if wr.err != nil {
			t.Fatalf("parked waiter err = %v", wr.err)
		}
		if wr.permit == nil {
			t.Fatal("parked waiter granted no permit")
		}
		wr.permit.Release()
	case <-time.After(5 * time.Second):
		t.Fatal("parked waiter never granted")
	}
	eventually(t, "slots drain", func() bool { return p.slotLive(slotKey{entry: entry, model: modelA}) == 0 })
}

// TestSlotDrainParityNoPressure proves the no-pressure drain follows the
// MASQ strict positional spill order: account #1 drains before #2, so
// every cold acquire lands on #1 and all admissions happen there.
func TestSlotDrainParityNoPressure(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newSmartTestPool(t, nil, mock0, mock1)

	const n = 6
	got := make([]int, n)
	for i := range n {
		toks := p.roster.Load()
		(*toks)[0].session.Invalidate()
		(*toks)[1].session.Invalidate()
		lease, err := p.Acquire(context.Background(), modelA)
		if err != nil {
			t.Fatal(err)
		}
		got[i] = lease.Token
		p.LeaseRelease(lease)
	}
	for i, tok := range got {
		if tok != 0 {
			t.Errorf("acquire %d token = %d, want 0 (strict spill drains #1 first)", i, tok)
		}
	}
	if mock0.SessionCreates != 6 || mock1.SessionCreates != 0 {
		t.Errorf("session creates = %d/%d, want 6/0 (strict spill drains #1 first)", mock0.SessionCreates, mock1.SessionCreates)
	}
}

// TestSlotHotStickiness proves the smart path keeps hot-session-first
// reuse: successive acquires land on the live session.
func TestSlotHotStickiness(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newSmartTestPool(t, nil, mock0, mock1)

	for i := range 4 {
		lease, err := p.Acquire(context.Background(), modelA)
		if err != nil {
			t.Fatal(err)
		}
		if lease.Token != 0 {
			t.Errorf("acquire %d token = %d, want 0 (hot reuse)", i, lease.Token)
		}
		p.LeaseRelease(lease)
	}
	if mock1.SessionCreates != 0 {
		t.Errorf("cold token session creates = %d, want 0", mock1.SessionCreates)
	}
}

// TestSlotAllCappedDegrades proves total exhaustion still surfaces
// the honest 429 bucket (the rank degrades to the legacy order so the loop
// records every reason).
func TestSlotAllCappedDegrades(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newSmartTestPool(t, nil, mock0, mock1)

	rle := &upstream.RateLimitError{
		Status: "rate_limited", Model: modelA, RetryAfter: time.Minute,
		Limit: 10, RecentCount: 10, Body: "session quota exhausted for model",
	}
	p.CooldownTokenRateLimit(0, rle)
	p.CooldownTokenRateLimit(1, rle)
	_, err := p.Acquire(context.Background(), modelA)
	if err == nil {
		t.Fatal("acquire succeeded on a fully capped pool")
	}
	var got *upstream.RateLimitError
	if !errors.As(err, &got) {
		t.Fatalf("exhausted err = %T %v, want *upstream.RateLimitError", err, err)
	}
}

// TestSlotTransientHookFires proves a transport-transient admission
// failure on account #1 still serves the request on the next account
// in strict index order.
func TestSlotTransientHookFires(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newSmartTestPool(t, nil, mock0, mock1)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	defer p.LeaseRelease(lease)
	if lease.Token != 1 {
		t.Fatalf("lease token = %d, want 1 (spill past the failing account)", lease.Token)
	}
}

// TestSlotParamsFloors pins the defensive defaults for hand-built
// configs that bypass Load (nil and zero configs): a zero SlotsPerAccount
// is unlimited (the acquire hook skips slot gating entirely).
func TestSlotParamsFloors(t *testing.T) {
	cap, depth, wait := slotParams(nil)
	if cap != 2 || depth != 16 || wait != 30*time.Second {
		t.Errorf("nil params = %d/%d/%v, want 2/16/30s", cap, depth, wait)
	}
	cap, depth, wait = slotParams(&config.Config{})
	if cap != 0 || depth != 0 || wait != 30*time.Second {
		t.Errorf("zero params = %d/%d/%v, want 0/0/30s (unlimited)", cap, depth, wait)
	}
	cap, depth, wait = slotParams(&config.Config{SlotsPerAccount: 3, QueueDepth: 5, QueueWait: 7 * time.Second})
	if cap != 3 || depth != 5 || wait != 7*time.Second {
		t.Errorf("explicit params = %d/%d/%v, want 3/5/7s", cap, depth, wait)
	}
}

// TestSlotLedgerTwoSlotsThirdParks is the MASQ R2 keeper: 3 concurrent
// requests for the SAME model on one account (cap 2 per (account, model))
// grant 2 leases immediately and park the third FIFO; freeing one slot
// grants the waiter with QueueWait>0 on the same account. The second
// account is never contacted.
func TestSlotLedgerTwoSlotsThirdParks(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newSmartTestPool(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueWait = 2 * time.Second
		c.QueueDepth = 16
	}, mock0, mock1)
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
	for i, l := range []*Lease{m1, m2} {
		if l.Token != 0 {
			t.Fatalf("lease %d on account #%d, want #1", i+1, l.Token+1)
		}
		if l.QueueWait != 0 {
			t.Fatalf("lease %d parked (%v), want zero parks (slots free)", i+1, l.QueueWait)
		}
	}
	if got := mock0.SessionCreatesSnapshot(); got != 1 {
		t.Fatalf("account #1 session creates = %d, want 1 (one session shared)", got)
	}
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
	if n := mock1.RequestsSnapshot(); n != 0 {
		t.Fatalf("account #2 touched (%d requests) while #1 still queued, want 0", n)
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

// TestSlotLedgerPerModelIsolation is the MASQ R2 isolation keeper: lanes
// are keyed per (account, model), so 2 turns of modelA plus 2 turns of
// modelB run together on account #1 (4 live turns, zero parks) while
// account #2 sees no contact at all.
func TestSlotLedgerPerModelIsolation(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newSmartTestPool(t, func(c *config.Config) {
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
	for i := range 2 {
		l, err := p.Acquire(ctx, modelA)
		if err != nil {
			t.Fatalf("modelA lease %d: %v", i, err)
		}
		held = append(held, l)
	}
	for i := range 2 {
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
	entry := smartEntry(p, 0)
	if got := p.slotLive(slotKey{entry: entry, model: modelA}); got != 2 {
		t.Fatalf("modelA lane live = %d, want 2", got)
	}
	if got := p.slotLive(slotKey{entry: entry, model: modelB}); got != 2 {
		t.Fatalf("modelB lane live = %d, want 2", got)
	}
	if n := mock1.RequestsSnapshot(); n != 0 {
		t.Fatalf("account #2 touched (%d requests), want 0", n)
	}
}
