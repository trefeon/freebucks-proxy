// Throwaway excise tests (Fase E, E4): opaque 429s pass through without
// cooldown writes, while bans keep their terminal quarantine.
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

// TestExciseOpaque429WritesNoCooldown pins E4: a 429 carrying no upstream
// retry signal writes no cooldown (passthrough, no bounded backoff).
func TestExciseOpaque429WritesNoCooldown(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"some_future_code"}`))
	}
	p := newTestPoolCfg(t, func(c *config.Config) {}, mock0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = p.Acquire(ctx, modelA)
	tok := (*p.roster.Load())[0]
	if until := tok.runs.CooldownUntil(); time.Now().Before(until) {
		t.Fatalf("opaque 429 wrote cooldown until %s, want passthrough", until.Format(time.RFC3339))
	}
}

// TestExciseBanStillQuarantined is the E4 keeper: a ban still marks the
// account terminal and surfaces ErrBanned.
func TestExciseBanStillQuarantined(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock0.SetBan(true)
	p := newTestPoolCfg(t, func(c *config.Config) {}, mock0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := p.Acquire(ctx, modelA)
	if err == nil || !errors.Is(err, upstream.ErrBanned) {
		t.Fatalf("want ErrBanned, got %v", err)
	}
	if q := (*p.roster.Load())[0].quarantine.Load(); q == nil {
		t.Fatal("ban did not quarantine, want terminal quarantine")
	}
}
