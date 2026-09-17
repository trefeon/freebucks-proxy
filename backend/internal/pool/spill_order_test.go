package pool

import (
	"context"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
	"testing"
)

// TestStrictOrderDrainsAccountOneFirst is the MASQ C1 keeper: strict
// positional order drains account #1 before touching #2. Three accounts
// with cap 2 per (account, model) and no queueing (QUEUE_DEPTH=0, spill
// at once on a full lane) grant 6 held leases as token [0,0,1,1,2,2]
// with zero park wait on every lease. A precious holder stays at its own
// index: no re-rank, no head boost.
func TestStrictOrderDrainsAccountOneFirst(t *testing.T) {
	mocks := []*testutil.MockUpstream{testutil.NewMock(), testutil.NewMock(), testutil.NewMock()}
	for _, m := range mocks {
		t.Cleanup(m.Close)
	}
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueDepth = 0
	}, mocks...)

	ctx := context.Background()
	var leases []*Lease
	for range 6 {
		lease, err := p.Acquire(ctx, modelA)
		if err != nil {
			for _, l := range leases {
				p.LeaseRelease(l)
			}
			t.Fatalf("acquire: %v", err)
		}
		leases = append(leases, lease)
	}
	got := make([]int, len(leases))
	for i, l := range leases {
		got[i] = l.Token
		if l.QueueWait != 0 {
			t.Errorf("lease %d queue wait = %v, want 0 (no parking with depth 0)", i, l.QueueWait)
		}
	}
	for _, l := range leases {
		p.LeaseRelease(l)
	}
	want := []int{0, 0, 1, 1, 2, 2}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lease order = %v, want %v (strict index drain)", got, want)
		}
	}
	t.Run("reorder follows new index", func(t *testing.T) {
		if err := p.SwapTokens(0, 2); err != nil {
			t.Fatalf("SwapTokens(0, 2): %v", err)
		}
		var moved []*Lease
		for range 6 {
			lease, err := p.Acquire(ctx, modelA)
			if err != nil {
				for _, l := range moved {
					p.LeaseRelease(l)
				}
				t.Fatalf("acquire after reorder: %v", err)
			}
			moved = append(moved, lease)
		}
		regot := make([]int, len(moved))
		for i, l := range moved {
			regot[i] = l.Token
		}
		for _, l := range moved {
			p.LeaseRelease(l)
		}
		for i := range want {
			if regot[i] != want[i] {
				t.Fatalf("order after reorder = %v, want %v (visits follow the new index, not the account)", regot, want)
			}
		}
	})
}
