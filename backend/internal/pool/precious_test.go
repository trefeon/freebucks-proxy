package pool

import (
	"context"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/testutil"
	"sync"
	"testing"
	"time"
)

// TestPreciousTwoAccountsSameModel is the MASQ I3 keeper: 2 accounts serving
// the same model hold 2+2 live-turn slots with one session each, and those
// sessions are precious — recovery/operator drops that are not terminal keep
// them (SessionEnds stays 0 and the next Acquire reuses the same instance),
// while a superseded session still drops.
func TestPreciousTwoAccountsSameModel(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newSmartTestPool(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueWait = 1500 * time.Millisecond
		c.QueueDepth = 16
	}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Four contenders over 2 accounts x cap 2: every lease grants, split 2/2
	// with one shared session per account.
	type result struct {
		lease *Lease
		err   error
	}
	burst := make(chan result, 4)
	var wg sync.WaitGroup
	for range 4 {
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
	for r := range burst {
		if r.err != nil {
			t.Fatalf("burst acquire: %v", r.err)
		}
		granted = append(granted, r.lease)
	}
	on0, on1 := 0, 0
	inst := map[int]string{}
	for _, l := range granted {
		switch l.Token {
		case 0:
			on0++
		case 1:
			on1++
		default:
			t.Fatalf("burst lease on account #%d, want #1/#2", l.Token+1)
		}
		inst[l.Token] = l.SessionInstanceID
	}
	if on0 != 2 || on1 != 2 {
		t.Fatalf("burst split #%d/#%d, want 2/2 (2 accounts x model = 4 slots)", on0, on1)
	}
	if got := mock0.SessionCreatesSnapshot(); got != 1 {
		t.Fatalf("account #1 creates = %d, want 1 (one session shared)", got)
	}
	if got := mock1.SessionCreatesSnapshot(); got != 1 {
		t.Fatalf("account #2 creates = %d, want 1 (one session shared)", got)
	}
	for _, l := range granted {
		p.LeaseRelease(l)
	}
	if got := mock0.SessionEndsSnapshot(); got != 0 {
		t.Fatalf("account #1 ends = %d, want 0 (precious, no drop under load)", got)
	}
	if got := mock1.SessionEndsSnapshot(); got != 0 {
		t.Fatalf("account #2 ends = %d, want 0 (precious, no drop under load)", got)
	}

	// A generic recovery invalidation on a precious session keeps it: the
	// next Acquire reuses the same instance with no new admission.
	p.InvalidateSession(0, inst[0])
	reuse, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("re-acquire after precious invalidate: %v", err)
	}
	if reuse.SessionInstanceID != inst[0] {
		t.Errorf("re-acquire instance = %q, want precious %q (session kept)", reuse.SessionInstanceID, inst[0])
	}
	p.LeaseRelease(reuse)
	if got := mock0.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("account #1 creates after precious invalidate = %d, want still 1", got)
	}

	// An operator drop on a precious session keeps it too: kept reports
	// the no-op so the dashboard can say so instead of claiming a drop.
	kept, err := p.DropTokenSession(ctx, 1)
	if err != nil {
		t.Fatalf("DropTokenSession on precious: %v", err)
	}
	if !kept {
		t.Error("DropTokenSession on precious session kept = false, want true (session kept, no re-admit)")
	}
	reuse1, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("re-acquire after precious drop: %v", err)
	}
	p.LeaseRelease(reuse1)
	if got := mock1.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("account #2 creates after precious drop = %d, want still 1", got)
	}

	// Load dropping to zero never closes a precious account: idle maintain
	// and poll passes end nothing, and the sessions stay reusable.
	p.lastActiveMu.Lock()
	p.lastActive = time.Now().Add(-2 * time.Hour)
	p.lastActiveMu.Unlock()
	p.maintainTick(context.Background())
	p.sessionPollTick(context.Background())
	if got := mock0.SessionEndsSnapshot(); got != 0 {
		t.Errorf("account #1 ends after idle = %d, want 0", got)
	}
	if got := mock1.SessionEndsSnapshot(); got != 0 {
		t.Errorf("account #2 ends after idle = %d, want 0", got)
	}
	after, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire after idle: %v", err)
	}
	if after.SessionInstanceID != inst[0] {
		t.Errorf("post-idle instance = %q, want precious %q", after.SessionInstanceID, inst[0])
	}

	// Terminal exception: a superseded session still drops so the next
	// request re-admits fresh. The mock serves a static instance id, so
	// rotate it first to tell the re-admission apart from the kept one.
	mock0.InstanceID = "inst-readmit-2"
	p.InvalidateLeaseSessionWithReason(after, session.ReasonSuperseded, 409)
	p.LeaseRelease(after)
	fresh, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire after superseded: %v", err)
	}
	defer p.LeaseRelease(fresh)
	if fresh.SessionInstanceID == inst[0] {
		t.Errorf("post-superseded instance = %q, want a fresh admission (superseded drops)", fresh.SessionInstanceID)
	}
	if got := mock0.SessionCreatesSnapshot(); got != 2 {
		t.Errorf("account #1 creates after superseded = %d, want 2 (re-admit)", got)
	}
}
