package pool

import (
	"context"
	"freebucks-proxy/backend/internal/testutil"
	"testing"
)

// TestRemoveTokenSeamlessUnderInflight proves that RemoveTokenAt and
// RemoveLastToken succeed immediately even while requests are in flight
// (InflightCount > 0), parking the retired token in p.retired until the
// lease releases and drains cleanly.
func TestRemoveTokenSeamlessUnderInflight(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mock2 := testutil.NewMock()
	defer mock2.Close()

	p := newTestPool(t, mock0, mock1, mock2)

	ctx := context.Background()
	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lease == nil || lease.entry == nil {
		t.Fatal("lease nil or missing entry")
	}

	// Verify that inflight count is active on the leased token.
	if got := lease.entry.runs.InflightCount(); got <= 0 {
		t.Fatalf("InflightCount = %d, want > 0 while holding lease", got)
	}

	// Remove a token at middle index while in flight: MUST succeed immediately without "active requests in flight"!
	if err := p.RemoveTokenAt(1); err != nil {
		t.Fatalf("RemoveTokenAt(1) failed while in-flight: %v", err)
	}

	// Remove the last token while in flight: MUST succeed immediately!
	if err := p.RemoveLastToken(); err != nil {
		t.Fatalf("RemoveLastToken() failed while in-flight: %v", err)
	}

	// Roster now has exactly 1 token left.
	roster := *p.roster.Load()
	if len(roster) != 1 {
		t.Fatalf("roster len = %d, want 1", len(roster))
	}

	// Releasing the held lease must succeed cleanly without panicking.
	p.LeaseRelease(lease)
	if got := lease.entry.runs.InflightCount(); got != 0 {
		t.Errorf("InflightCount after release = %d, want 0", got)
	}
}
