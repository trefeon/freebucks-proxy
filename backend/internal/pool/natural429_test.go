package pool

import (
	"context"
	"errors"
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// TestNatural429RequeuesNoParkNoFailover is the MASQ I5 keeper: a short
// natural quota jail on admission (429 rate_limited, 400ms window) is
// waited out on the SAME lane — the lease lands on account #1, account #2
// sees zero contact, and no cooldown is remembered. A failover walk would
// grant on #2; a cooldown write would park #1.
func TestNatural429RequeuesNoParkNoFailover(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	// One natural 429 on the first session POST, then the healthy active
	// shape (mirrors the mock default body).
	var creates atomic.Int64
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && creates.Add(1) == 1 {
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"status":"rate_limited","limit":3,"recentCount":3,"period":"pacific_day","resetAt":"2026-08-12T07:00:00.000Z","retryAfterMs":400}`)
			return
		}
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-abc-123","expiresAt":"2026-09-18T07:00:00.000Z"}`)
	}
	p := newSmartTestPool(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueWait = 2 * time.Second
		c.QueueDepth = 16
	}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire through 400ms quota jail: %v", err)
	}
	defer p.LeaseRelease(lease)
	elapsed := time.Since(start)

	if lease.Token != 0 {
		t.Errorf("lease token = %d, want 0 (same-lane requeue, no failover)", lease.Token)
	}
	if elapsed < 400*time.Millisecond {
		t.Errorf("acquire took %v, want >= 400ms (jail waited out, not skipped)", elapsed)
	}
	if n := mock1.RequestsSnapshot(); n != 0 {
		t.Errorf("account #2 requests = %d, want 0 (no failover walk)", n)
	}
	entry0 := (*p.roster.Load())[0]
	if until := entry0.runs.CooldownUntil(); !until.IsZero() {
		t.Errorf("account #1 CooldownUntil = %v, want zero (no cooldown write)", until)
	}
	if rle := entry0.runs.RateLimitError(); rle != nil {
		t.Errorf("account #1 remembered %v, want no rate-limit memory", rle)
	}
	if got := mock0.SessionEndsSnapshot(); got != 0 {
		t.Errorf("account #1 ends = %d, want 0 (session kept for the retry)", got)
	}
	if lease.QueueWait < 300*time.Millisecond {
		t.Errorf("lease QueueWait = %v, want >= 300ms (jail wait rides telemetry)", lease.QueueWait)
	}

	t.Run("ban still quarantines and advances", func(t *testing.T) {
		b0 := testutil.NewMock()
		t.Cleanup(b0.Close)
		b1 := testutil.NewMock()
		t.Cleanup(b1.Close)
		b0.Ban = true
		bp := newSmartTestPool(t, func(c *config.Config) { c.QueueWait = 300 * time.Millisecond }, b0, b1)
		bctx, bcancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer bcancel()

		// A banned head lane quarantines and the walk advances: the
		// healthy account serves.
		bl, err := bp.Acquire(bctx, modelA)
		if err != nil {
			t.Fatalf("acquire past banned head: %v", err)
		}
		if bl.Token != 1 {
			t.Errorf("lease token = %d, want 1 (banned head advanced)", bl.Token)
		}
		bp.LeaseRelease(bl)
		if snaps := bp.Snapshot(); !snaps[0].Quarantined {
			t.Errorf("account #1 quarantined = false, want true (terminal ban kept)")
		}

		// Fully banned pool: both lanes are attempted (the walk still
		// advances past a ban) and the ban surfaces, never a requeue.
		c0 := testutil.NewMock()
		t.Cleanup(c0.Close)
		c1 := testutil.NewMock()
		t.Cleanup(c1.Close)
		c0.Ban = true
		c1.Ban = true
		cp := newSmartTestPool(t, func(c *config.Config) { c.QueueWait = 300 * time.Millisecond }, c0, c1)
		if _, err := cp.Acquire(bctx, modelA); !errors.Is(err, upstream.ErrBanned) {
			t.Errorf("all-banned acquire = %v, want ErrBanned", err)
		}
		// The ban short-circuits before a session create, so advance is
		// proven by request counts: both lanes attempted.
		if n := c0.RequestsSnapshot(); n == 0 {
			t.Error("account #1 requests = 0, want >= 1 (head attempted)")
		}
		if n := c1.RequestsSnapshot(); n == 0 {
			t.Error("account #2 requests = 0, want >= 1 (ban advanced the lane)")
		}
		snaps := cp.Snapshot()
		if !snaps[0].Quarantined || !snaps[1].Quarantined {
			t.Errorf("quarantined = [%v %v], want both true", snaps[0].Quarantined, snaps[1].Quarantined)
		}
	})
}
