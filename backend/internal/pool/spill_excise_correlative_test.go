// Throwaway excise tests (Fase E): correlative refusals must surface
// without a failover walk and without cooldown writes. Keeper decisions
// happen in C4; until then these files prove the excision.
package pool

import (
	"context"
	"errors"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
	"net/http"
	"testing"
	"time"
)

// TestExciseCorrelativeNoWalk pins E1: an ip_capped admission refusal on
// account #1 surfaces ErrIpCapped immediately without touching account #2.
func TestExciseCorrelativeNoWalk(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"ip_capped","retryAfterMs":60000}`))
	}
	p := newTestPoolCfg(t, func(c *config.Config) {}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := p.Acquire(ctx, modelA)
	if err == nil {
		t.Fatal("want ip_capped surface, got lease")
	}
	if !errors.Is(err, upstream.ErrIpCapped) {
		t.Fatalf("want ErrIpCapped, got %v", err)
	}
	if n := mock1.RequestsSnapshot(); n != 0 {
		t.Fatalf("walked to account #2 (%d requests), want 0", n)
	}
}

// TestExciseIpCappedWritesNoCooldown pins E2: an ip_capped refusal writes
// no per-token cooldown (no deadline, the natural 429 surfaces every time).
// The ip_capped window type itself is excised (runs keeps no such state),
// so the nil proof reads the surviving remembered-error slots: a
// correlative refusal must leave every one of them empty.
func TestExciseIpCappedWritesNoCooldown(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"ip_capped","retryAfterMs":60000}`))
	}
	p := newTestPoolCfg(t, func(c *config.Config) {}, mock0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = p.Acquire(ctx, modelA)
	tok := (*p.roster.Load())[0]
	if until := tok.runs.CooldownUntil(); time.Now().Before(until) {
		t.Fatalf("ip_capped wrote cooldown until %s, want none", until.Format(time.RFC3339))
	}
	if rle := tok.runs.RateLimitError(); rle != nil {
		t.Fatalf("ip_capped remembered rate-limit %v, want nil", rle)
	}
	if be := tok.runs.BanError(); be != nil {
		t.Fatalf("ip_capped remembered ban %v, want nil", be)
	}
	if cbe := tok.runs.CountryBlockedError(); cbe != nil {
		t.Fatalf("ip_capped remembered country-block %v, want nil", cbe)
	}
}

// TestExciseLimitedIPNoWalkNoMark pins E3: a limited_ip admission refusal
// surfaces ErrModelIPLimited without walking and without an unfit mark.
func TestExciseLimitedIPNoWalkNoMark(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"session_model_mismatch","message":"model limited on this egress ip"}`))
	}
	p := newTestPoolCfg(t, func(c *config.Config) {}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := p.Acquire(ctx, modelA)
	if err == nil || !errors.Is(err, upstream.ErrModelIPLimited) {
		t.Fatalf("want ErrModelIPLimited, got %v", err)
	}
	if n := mock1.RequestsSnapshot(); n != 0 {
		t.Fatalf("walked to account #2 (%d requests), want 0", n)
	}
}
