// glm_referral_test.go — issue #183: referral-gated model (z-ai/glm-5.2)
// entitlement gating (quota fallback removed: no local pre-refusal or
// fallback; upstream refusals surface honestly). Only tokens with verified
// GLM entitlement are admitted for GLM 5.2 sessions.
package pool

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
)

// TestUnentitledPoolTokenGlmRefusalWithoutFallback verifies that a LIVE
// upstream refusal for z-ai/glm-5.2 surfaces as an honest 429 after the
// admission is attempted — the pool never pre-refuses unentitled tokens
// locally (quota fallback removed). (ADR-0027: admit like unmetered until
// upstream refuses.)
func TestUnentitledPoolTokenGlmRefusalWithoutFallback(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimit = true // live upstream 429 on session admission

	p := newTestPool(t, mock)

	before := mock.RequestCount()
	_, err := p.Acquire(context.Background(), "z-ai/glm-5.2")
	if err == nil {
		t.Fatal("Acquire(z-ai/glm-5.2) succeeded against a refusing upstream, want 429 rate-limit error")
		return
	}
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("err = %v, want *upstream.RateLimitError (honest upstream refusal)", err)
	}
	if strings.Contains(rle.Body, "referral entitlement required") {
		t.Errorf("rle.Body = %q, want live refusal, not the retired local pre-gate", rle.Body)
	}
	if after := mock.RequestCount(); after <= before {
		t.Errorf("upstream requests = %d, want > %d (admission attempted before refusing)", after, before)
	}
}

// TestEntitledPoolTokenWithGlmPromo verifies that a token with an active
// GlmPromo block is permitted to open a z-ai/glm-5.2 session without falling back.
func TestEntitledPoolTokenWithGlmPromo(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	var glmCreates atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-freebuff-model") == "z-ai/glm-5.2" {
			glmCreates.Add(1)
		}
		expiresAt := time.Now().Add(30 * time.Minute).UTC().Format("2006-01-02T15:04:05.000Z07:00")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-glm-123","model":"z-ai/glm-5.2","expiresAt":"`+expiresAt+`","glmPromo":{"dailySessions":2,"endsAt":"2099-01-01T00:00:00Z"}}`)
	}

	p := newTestPool(t, mock)

	// Seed token 0 with active GlmPromo
	toks := p.roster.Load()
	(*toks)[0].session.Invalidate()
	// Run a probe that populates GlmPromo
	mock.GlmPromo = map[string]any{
		"dailySessions": 2,
		"endsAt":        "2099-01-01T00:00:00Z",
	}
	_, _ = p.ProbeToken(context.Background(), 0)

	lease, err := p.Acquire(context.Background(), "z-ai/glm-5.2")
	if err != nil {
		t.Fatalf("Acquire(z-ai/glm-5.2) failed: %v", err)
	}
	defer p.LeaseRelease(lease)

	if lease.Model != "z-ai/glm-5.2" {
		t.Errorf("lease.Model = %q, want z-ai/glm-5.2", lease.Model)
	}
	if glmCreates.Load() == 0 {
		t.Error("glmCreates = 0, want GLM 5.2 session create for entitled token")
	}
}
