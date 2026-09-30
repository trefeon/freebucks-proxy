package session

// Dead-claim wedge tests: a 409 admission_attempt_closed means the purchase
// start the persisted claim refers to is dead upstream — re-POSTing the same
// claim 409s forever, across restarts via the persisted claim row. Refresh
// rotates to a fresh claim and retries the admission exactly once per request
// (bounded by the claimRetried flag shared with purchase_claim_released), and
// a retry that still fails surfaces unchanged.

import (
	"context"
	"errors"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// attemptRecorder captures every admission POST's headers in arrival order.
// The mock bypasses its own create ledger when SessionHandler is set, so the
// wedge tests record the wire themselves (claim + attempt headers per try).
type attemptRecorder struct {
	mu   sync.Mutex
	hdrs []http.Header
}

func (r *attemptRecorder) record(h http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hdrs = append(r.hdrs, h.Clone())
}

func (r *attemptRecorder) snapshot() []http.Header {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]http.Header(nil), r.hdrs...)
}

// assertAttemptHeaders pins the persisted-claim wire shape per attempt: the
// claim as x-freebuff-instance-id with the multi-session attempt headers,
// desktop-attempt-id carrying the claim's uuid suffix (sessionAttemptSuffix).
func assertAttemptHeaders(t *testing.T, h http.Header, claim string) {
	t.Helper()
	if got := h.Get("x-freebuff-instance-id"); got != claim {
		t.Errorf("x-freebuff-instance-id = %q, want claim %q", got, claim)
	}
	if got := h.Get("x-freebuff-multi-session"); got != "1" {
		t.Errorf("x-freebuff-multi-session = %q, want 1", got)
	}
	if got := h.Get("x-freebuff-purchase-continuity"); got != "1" {
		t.Errorf("x-freebuff-purchase-continuity = %q, want 1", got)
	}
	if want := strings.TrimPrefix(claim, claimPrefix); h.Get("x-freebuff-desktop-attempt-id") != want {
		t.Errorf("x-freebuff-desktop-attempt-id = %q, want uuid suffix %q", h.Get("x-freebuff-desktop-attempt-id"), want)
	}
}

// closedThenActive serves one dead-start 409 before admitting: the shape of
// the live wedge (stale persisted claim first, fresh claim after rotation).
func closedThenActive(rec *attemptRecorder, t *testing.T) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected %s (want only admission POSTs)", r.Method)
			http.NotFound(w, r)
			return
		}
		rec.record(r.Header)
		if len(rec.snapshot()) == 1 {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":   "admission_attempt_closed",
				"message": "The referenced admission attempt is closed.",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "active",
			"instanceId": "inst-rotated",
			"expiresAt":  "2030-01-01T00:00:00Z",
		})
	}
}

// TestAdmissionRotatesClaimOnAttemptClosed pins the wedge recovery: the first
// attempt carries the stale persisted claim and 409s, the retry carries a
// DIFFERENT claim with consistent attempt headers and is adopted.
func TestAdmissionRotatesClaimOnAttemptClosed(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	stale := mgr.ensureClaim()
	assertClaimShape(t, stale)

	var rec attemptRecorder
	mock.SessionHandler = closedThenActive(&rec, t)

	inst, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatalf("admission after attempt-closed rotation failed: %v", err)
	}
	if inst != "inst-rotated" {
		t.Errorf("instance = %q, want inst-rotated (fresh-claim retry adopted)", inst)
	}
	hdrs := rec.snapshot()
	if len(hdrs) != 2 {
		t.Fatalf("admission POSTs = %d, want exactly 2 (stale 409 + one rotated retry)", len(hdrs))
	}
	first := hdrs[0].Get("x-freebuff-instance-id")
	if first != stale {
		t.Errorf("first POST claim = %q, want stale persisted claim %q", first, stale)
	}
	second := hdrs[1].Get("x-freebuff-instance-id")
	if second == "" || second == stale {
		t.Fatalf("second POST claim = %q, want a fresh claim (rotation before retry)", second)
	}
	assertClaimShape(t, second)
	assertAttemptHeaders(t, hdrs[0], first)
	assertAttemptHeaders(t, hdrs[1], second)
	if got := mgr.ClaimIDForTest(); got != second {
		t.Errorf("manager claim = %q, want retried claim %q", got, second)
	}
}

// TestAdmissionAttemptClosedRetryStill409Surfaces pins the bound: when the
// rotated retry also 409s, the error surfaces unchanged after exactly 2
// upstream hits — never a loop.
func TestAdmissionAttemptClosedRetryStill409Surfaces(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	before := mgr.ensureClaim()
	var rec attemptRecorder
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected %s (want only admission POSTs)", r.Method)
			http.NotFound(w, r)
			return
		}
		rec.record(r.Header)
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "admission_attempt_closed",
			"message": "The referenced admission attempt is closed.",
		})
	}

	_, err := mgr.EnsureSession(context.Background())
	var ue *upstream.UpstreamError
	if !errors.As(err, &ue) || ue.Status != http.StatusConflict {
		t.Fatalf("err = %v, want the 409 UpstreamError surfaced unchanged", err)
	}
	if !strings.Contains(strings.ToLower(ue.Body), admissionAttemptClosedMarker) {
		t.Errorf("err body = %q, want the admission_attempt_closed marker", ue.Body)
	}
	hdrs := rec.snapshot()
	if len(hdrs) != 2 {
		t.Fatalf("admission POSTs = %d, want exactly 2 (initial + one rotated retry, never a loop)", len(hdrs))
	}
	second := hdrs[1].Get("x-freebuff-instance-id")
	if second == "" || second == before {
		t.Errorf("second POST claim = %q, want a fresh claim (exactly one rotation)", second)
	}
	if got := mgr.ClaimIDForTest(); got != second {
		t.Errorf("manager claim = %q, want retried claim %q", got, second)
	}
}

// TestAttemptClosedRotationPersistsAcrossRestart pins the store handoff: the
// rotated claim is persisted, so a rebuilt manager rejoins on the fresh
// identity instead of re-POSTing the dead one.
func TestAttemptClosedRotationPersistsAcrossRestart(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	mgr, key := newPersistTestManager(t, mock, store)

	stale := mgr.ensureClaim()
	var rec attemptRecorder
	mock.SessionHandler = closedThenActive(&rec, t)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	rotated := mgr.ClaimIDForTest()
	if rotated == "" || rotated == stale {
		t.Fatalf("claim after attempt-closed = %q, want a fresh claim (stale was %q)", rotated, stale)
	}
	if got := store.LoadClaim(key); got != rotated {
		t.Fatalf("stored claim = %q, want rotated %q (restarts must adopt the fresh identity)", got, rotated)
	}

	mgr2, _ := newPersistTestManager(t, mock, store)
	if got := mgr2.ensureClaim(); got != rotated {
		t.Errorf("restarted claim = %q, want persisted rotated %q (relaunch handoff)", got, rotated)
	}
}
