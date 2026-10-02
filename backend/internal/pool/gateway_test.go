package pool

import (
	"context"
	"freebuff-proxy/backend/internal/testutil"
	"testing"
	"time"
)

// TestGatewayBreakerOpenSkipsLane: a lane whose breaker opened (3
// consecutive refusals) is skipped with no upstream contact; the walk
// grants the next lane. Green-path pools (closed breakers) never hit it.
func TestGatewayBreakerOpenSkipsLane(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newSmartTestPool(t, nil, mock0, mock1)
	smartWarmLane(t, p, modelA, 0)
	for range breakerFailThreshold {
		p.breakerReg().For(modelA, 0).RecordFailure()
	}
	wantCreates := mock0.SessionCreatesSnapshot()
	wantRequests := mock0.RequestsSnapshot()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire with lane 0 breaker open: %v", err)
	}
	defer p.LeaseRelease(lease)
	if lease.Token != 1 {
		t.Fatalf("lease on account #%d, want #2 (open-breaker lane skipped)", lease.Token+1)
	}
	if got := mock0.SessionCreatesSnapshot(); got != wantCreates {
		t.Errorf("skipped account session creates = %d, want %d (warm-up only, never served)", got, wantCreates)
	}
	if got := mock0.RequestsSnapshot(); got != wantRequests {
		t.Errorf("skipped account requests = %d, want %d (no upstream contact past the gate)", got, wantRequests)
	}
}

// TestGatewayClosedBreakerGrantsHead: closed breakers never divert the
// walk — the warm head lane grants exactly as before.
func TestGatewayClosedBreakerGrantsHead(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newSmartTestPool(t, nil, mock0, mock1)
	smartWarmLane(t, p, modelA, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer p.LeaseRelease(lease)
	if lease.Token != 0 {
		t.Fatalf("lease on account #%d, want #1 (closed breaker = no diversion)", lease.Token+1)
	}
}

// TestGatewayGrantClosesBreaker: one granted lease resets the lane's
// consecutive count, so a recovering lane stops tripping the gate.
func TestGatewayGrantClosesBreaker(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newSmartTestPool(t, nil, mock0, mock1)
	for range breakerFailThreshold - 1 {
		p.breakerReg().For(modelA, 0).RecordFailure()
	}
	smartWarmLane(t, p, modelA, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer p.LeaseRelease(lease)
	if got := p.breakerReg().For(modelA, 0).State(); got != "closed" {
		t.Errorf("lane breaker = %q after grant, want closed (success resets)", got)
	}
}

// TestGatewayAliasExpansion: a virtual model resolves through the compiled
// table before registry lookup; unknown aliases stay registry errors.
func TestGatewayAliasExpansion(t *testing.T) {
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	p := newSmartTestPool(t, nil, mock)
	if err := p.SetAliases(map[string]string{"virtual-cheap": modelA}); err != nil {
		t.Fatalf("SetAliases: %v", err)
	}
	smartWarmLane(t, p, modelA, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := p.Acquire(ctx, "virtual-cheap")
	if err != nil {
		t.Fatalf("acquire via alias: %v", err)
	}
	defer p.LeaseRelease(lease)
	if lease.Token != 0 {
		t.Errorf("alias lease on account #%d, want #1", lease.Token+1)
	}
	if err := p.SetAliases(map[string]string{"a": "b", "b": "a"}); err == nil {
		t.Error("SetAliases(cycle) succeeded, want load-time rejection (table unchanged)")
	}
	if _, err := p.Acquire(ctx, "virtual-unknown"); err == nil {
		t.Error("acquire(unknown alias) succeeded, want the registry miss (no new error shape)")
	}
}

// TestGatewayCheapestPolicyReorders: with the cheapest policy and lane
// signals, the routed order puts the cheap lane first and the arrival
// scan grants it. (A warm dear lane still wins over a cold cheap one —
// reusing a live session beats a fresh admission; the policy orders the
// walk, warmth orders the scan.)
func TestGatewayCheapestPolicyReorders(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newSmartTestPool(t, nil, mock0, mock1)
	smartWarmLane(t, p, modelA, 1)
	p.SetRoutePolicy(RouteCheapest)
	p.SetRouteStats(func(model string) []LaneStat {
		return []LaneStat{
			{Account: 0, Cost: 5.0, HasCost: true},
			{Account: 1, Cost: 1.0, HasCost: true},
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer p.LeaseRelease(lease)
	if lease.Token != 1 {
		t.Fatalf("lease on account #%d, want #2 (cheapest policy reorders)", lease.Token+1)
	}
}

// TestGatewayFallbackDefaultIsStrict: without opt-in the routed order is
// the strict order — the policy layer adds nothing on green paths.
func TestGatewayFallbackDefaultIsStrict(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newSmartTestPool(t, nil, mock0, mock1)
	got := p.routeOrder(modelA, []int{0, 1})
	if len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Errorf("routeOrder(fallback) = %v, want strict identity", got)
	}
	if resolved := p.resolveModel(modelA); resolved != modelA {
		t.Errorf("resolveModel(no table) = %q, want identity", resolved)
	}
}
