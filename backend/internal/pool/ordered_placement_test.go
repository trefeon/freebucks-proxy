package pool

// ordered_placement_test.go — POOL_ORDERED_PLACEMENT behavior keepers
// (acquire_route.go scanWarmFree): the knob routes a request to the LOWEST
// roster index that can serve it, while the default keeps today's warm-lane
// grant byte-identical and the seat invariant (never rotate an account's
// single upstream seat out from under an in-flight turn, seat.go) holds.
// All short-tier safe: no test parks a wait.

import (
	"context"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
	"testing"
	"time"
)

// TestOrderedPlacementPrefersLowestIndexLane: with the knob on, an earlier
// eligible lane that is COLD for the model wins over a later WARM lane — the
// cold lane creates its session inline instead of leaving the earlier account
// idle while the later one serves everything.
func TestOrderedPlacementPrefersLowestIndexLane(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newSmartTestPool(t, func(c *config.Config) {
		c.PoolOrderedPlacement = true
	}, mock0, mock1)
	// Account #1 stays cold; account #2 is warm for the model.
	smartWarmLane(t, p, modelA, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer p.LeaseRelease(lease)
	if lease.Token != 0 {
		t.Fatalf("lease on account #%d, want #1 (lowest eligible lane wins over the warm #2)", lease.Token+1)
	}
	if got := mock0.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("cold account #1 session creates = %d, want 1 (admitted inline)", got)
	}
	if got := mock1.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("warm account #2 session creates = %d, want 1 (warm-up only, never served)", got)
	}
}

// TestOrderedPlacementOffKeepsWarmGrant pins today's default: with the knob
// off the arrival scan still grants on the later WARM lane, never touching
// the earlier cold account. This is the byte-identical contract the operator
// keeps until POOL_ORDERED_PLACEMENT is flipped on.
func TestOrderedPlacementOffKeepsWarmGrant(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	// newSmartTestPool's hand-built config leaves PoolOrderedPlacement false
	// (production Load defaults it false too).
	p := newSmartTestPool(t, nil, mock0, mock1)
	smartWarmLane(t, p, modelA, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer p.LeaseRelease(lease)
	if lease.Token != 1 {
		t.Fatalf("lease on account #%d, want #2 (warm-lane grant wins with the knob off)", lease.Token+1)
	}
	if got := mock0.SessionCreatesSnapshot(); got != 0 {
		t.Errorf("cold account #1 session creates = %d, want 0 (never touched)", got)
	}
	if got := mock0.RequestsSnapshot(); got != 0 {
		t.Errorf("cold account #1 requests = %d, want 0 (never touched)", got)
	}
}

// TestOrderedPlacementSkipsBusySeatToNextAccount: the ordered preference
// never rotates the seat out from under an in-flight turn (seat.go). Account
// #1 already serves model A (its seat is held) and has a free model-B slot,
// but it is cold for model B — preferring it would rotate its single session
// and supersede the live model-A turn. The warm account #2 takes model B, so
// two models run on two accounts concurrently.
func TestOrderedPlacementSkipsBusySeatToNextAccount(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newSmartTestPool(t, func(c *config.Config) {
		c.PoolOrderedPlacement = true
	}, mock0, mock1)
	smartWarmLane(t, p, modelB, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	leaseA, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("modelA acquire: %v", err)
	}
	defer p.LeaseRelease(leaseA)
	if leaseA.Token != 0 {
		t.Fatalf("modelA lease on account #%d, want #1", leaseA.Token+1)
	}

	leaseB, err := p.Acquire(ctx, modelB)
	if err != nil {
		t.Fatalf("modelB acquire: %v", err)
	}
	defer p.LeaseRelease(leaseB)
	if leaseB.Token != 1 {
		t.Fatalf("modelB lease on account #%d, want #2 (account #1's seat is held by the live modelA turn)", leaseB.Token+1)
	}
	// Account #1 paid exactly one session (the modelA admission): the modelB
	// request never rotated it away from the in-flight turn.
	if got := mock0.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("account #1 session creates = %d, want 1 (held seat not rotated)", got)
	}
	if got := mock1.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("account #2 session creates = %d, want 1 (warm-up only)", got)
	}
}

// TestOrderedPlacementRotatesIdleEarlierLaneInPlace: once the earlier
// account's seat drains, the ordered preference is free to rotate it — the
// same "idle rotates, busy defers" rule seat.go enforces. Account #1's
// model-A session is idle, so the model-B request switches it in place
// rather than borrowing the warm account #2.
func TestOrderedPlacementRotatesIdleEarlierLaneInPlace(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newSmartTestPool(t, func(c *config.Config) {
		c.PoolOrderedPlacement = true
	}, mock0, mock1)
	smartWarmLane(t, p, modelB, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	leaseA, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("modelA acquire: %v", err)
	}
	if leaseA.Token != 0 {
		t.Fatalf("modelA lease on account #%d, want #1", leaseA.Token+1)
	}
	p.LeaseRelease(leaseA) // seat idle: the rotation is safe again

	leaseB, err := p.Acquire(ctx, modelB)
	if err != nil {
		t.Fatalf("modelB acquire: %v", err)
	}
	defer p.LeaseRelease(leaseB)
	if leaseB.Token != 0 {
		t.Fatalf("modelB lease on account #%d, want #1 (idle earlier lane rotates in place)", leaseB.Token+1)
	}
	// Two creates on account #1: the modelA admission then the in-place
	// modelB switch. Account #2 served nothing beyond its warm-up.
	if got := mock0.SessionCreatesSnapshot(); got != 2 {
		t.Errorf("account #1 session creates = %d, want 2 (modelA admit + modelB switch)", got)
	}
	if got := mock1.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("account #2 session creates = %d, want 1 (warm-up only, never served)", got)
	}
}
