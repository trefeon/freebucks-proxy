package runs

import (
	"freebucks-proxy/backend/internal/upstream"
	"testing"
	"time"
)

// TestModelRateLimitRememberAndIsolate pins the per-model refusal memory:
// a remembered refusal serves its own model only, never a blanket park.
func TestModelRateLimitRememberAndIsolate(t *testing.T) {
	m := &RunManager{}
	m.RememberModelRateLimit("model-a", &upstream.RateLimitError{
		Status:     "rate_limited",
		RetryAfter: time.Hour,
		Body:       "quota",
	})
	if got := m.ModelRateLimit("model-a"); got == nil {
		t.Fatal("ModelRateLimit(model-a) = nil, want remembered refusal")
	}
	if got := m.ModelRateLimit("model-b"); got != nil {
		t.Fatalf("ModelRateLimit(model-b) = %v, want nil (per-model memory, not a blanket park)", got)
	}
	if got := m.ModelRateLimit(""); got != nil {
		t.Fatalf("ModelRateLimit(\"\") = %v, want nil", got)
	}
}

// TestModelRateLimitOpaqueNeverParks pins that refusals without an expiry
// signal are retried live next time, never parked.
func TestModelRateLimitOpaqueNeverParks(t *testing.T) {
	m := &RunManager{}
	m.RememberModelRateLimit("model-a", &upstream.RateLimitError{Status: "rate_limited", Body: "opaque"})
	if got := m.ModelRateLimit("model-a"); got != nil {
		t.Fatalf("ModelRateLimit = %v, want nil (opaque refusal without RetryAfter/ResetAt)", got)
	}
	// Past windows are dead too.
	m.RememberModelRateLimit("model-a", &upstream.RateLimitError{
		Status:  "rate_limited",
		ResetAt: time.Now().Add(-time.Hour),
	})
	if got := m.ModelRateLimit("model-a"); got != nil {
		t.Fatalf("ModelRateLimit = %v, want nil (past ResetAt)", got)
	}
	// A future ResetAt alone (no RetryAfter) parks until the reset.
	m.RememberModelRateLimit("model-a", &upstream.RateLimitError{
		Status:  "rate_limited",
		ResetAt: time.Now().Add(time.Hour),
	})
	if got := m.ModelRateLimit("model-a"); got == nil {
		t.Fatal("ModelRateLimit = nil, want refusal parked until ResetAt")
	}
	// Empty model and nil refusal are no-ops, never panics.
	m.RememberModelRateLimit("", &upstream.RateLimitError{Status: "rate_limited", RetryAfter: time.Hour})
	m.RememberModelRateLimit("model-a", nil)
}

// TestModelRateLimitExpiryAndOverwrite pins the window lifecycle: a short
// jail serves during its window, lazy-expires on read, and a second
// Remember for the same model overwrites the first.
func TestModelRateLimitExpiryAndOverwrite(t *testing.T) {
	m := &RunManager{}
	m.RememberModelRateLimit("model-a", &upstream.RateLimitError{Status: "rate_limited", RetryAfter: 100 * time.Millisecond})
	if got := m.ModelRateLimit("model-a"); got == nil {
		t.Fatal("ModelRateLimit = nil during the window, want remembered refusal")
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.ModelRateLimit("model-a") != nil {
		if time.Now().After(deadline) {
			t.Fatal("ModelRateLimit never expired after a 100ms window")
		}
		time.Sleep(20 * time.Millisecond)
	}

	m.RememberModelRateLimit("model-a", &upstream.RateLimitError{Status: "rate_limited", RetryAfter: time.Hour, Body: "first"})
	m.RememberModelRateLimit("model-a", &upstream.RateLimitError{Status: "rate_limited", RetryAfter: 2 * time.Hour, Body: "second"})
	got := m.ModelRateLimit("model-a")
	if got == nil {
		t.Fatal("ModelRateLimit = nil after overwrite, want second refusal")
	}
	if got.Body != "second" || got.RetryAfter != 2*time.Hour {
		t.Fatalf("ModelRateLimit = %+v, want the overwriting second refusal", got)
	}
}

// TestModelRateLimitCopiesCallerValue pins that the caller's struct is
// copied, never stored: walk errors may be single-flight-shared, so a later
// mutation (e.g. another walk's Model tag) must not corrupt the memory.
func TestModelRateLimitCopiesCallerValue(t *testing.T) {
	m := &RunManager{}
	rle := &upstream.RateLimitError{Status: "rate_limited", RetryAfter: time.Hour, Model: "model-a"}
	m.RememberModelRateLimit("model-a", rle)
	rle.Model = "mutated"
	rle.RetryAfter = time.Second
	got := m.ModelRateLimit("model-a")
	if got == nil {
		t.Fatal("ModelRateLimit = nil, want remembered refusal")
	}
	if got.Model != "model-a" || got.RetryAfter != time.Hour {
		t.Fatalf("ModelRateLimit = %+v, want the values at Remember time (copy, not alias)", got)
	}
}

// TestModelRateLimitClearCooldowns pins that success/unlock clears the
// per-model memory alongside the blanket state.
func TestModelRateLimitClearCooldowns(t *testing.T) {
	m := &RunManager{}
	m.RememberModelRateLimit("model-a", &upstream.RateLimitError{Status: "rate_limited", RetryAfter: time.Hour})
	m.ClearCooldowns()
	if got := m.ModelRateLimit("model-a"); got != nil {
		t.Fatalf("ModelRateLimit after ClearCooldowns = %v, want nil", got)
	}
}
