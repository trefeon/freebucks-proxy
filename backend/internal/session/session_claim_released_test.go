package session

// Released-purchase wedge battery (token-1 deepseek, live 2026-10-01): a 409
// purchase_claim_released whose post-rotation retry ALSO 409s means the
// purchase itself is gone upstream — no fresh claim can admit, so the
// rotation budget is spent correctly and the honest 409 must surface (case
// A, account-side, operator action needed), while a 409-once-then-200 must
// still adopt on the rotated claim (case B). The manager additionally
// remembers the refusal per model for claimReleasedTTL so a hot loop fails
// fast instead of churning rotations + admission POSTs.

import (
	"context"
	"errors"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	releasedDeepseek = "deepseek/deepseek-v4-flash"
	releasedMimo     = "mimo/mimo-v2-5"
)

func releasedStatusBody() map[string]any {
	return map[string]any{"status": "purchase_claim_released", "message": "claim released"}
}

func releasedActiveBody() map[string]any {
	return map[string]any{"status": "active", "instanceId": "inst-released-1", "expiresAt": "2030-01-01T00:00:00Z"}
}

func asReleased409(t *testing.T, err error, wantMarker bool) {
	t.Helper()
	var ue *upstream.UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want a 409 UpstreamError", err)
	}
	if ue.Status != http.StatusConflict {
		t.Fatalf("ue.Status = %d, want 409", ue.Status)
	}
	if ue.Body == "" {
		t.Fatal("ue.Body empty, want the upstream refusal text")
	}
	// The live admission refusal carries the marker in the status-form
	// wire body; statusError surfaces st.Message instead, so a
	// message-only body is still the honest refusal. The fail-fast memory
	// always names the marker (its own static body).
	if wantMarker && !strings.Contains(strings.ToLower(ue.Body), claimReleasedMarker) {
		t.Fatalf("ue.Body = %q, want the purchase_claim_released marker", ue.Body)
	}
}

// TestAdmissionRotatesClaimOnReleasedOnceThenAdopts pins case B: a released
// first attempt rotates and the retry on the fresh claim is adopted.
func TestAdmissionRotatesClaimOnReleasedOnceThenAdopts(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)
	before := mgr.ensureClaim()

	var rec attemptRecorder
	n := 0
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected %s (want only admission POSTs)", r.Method)
			http.NotFound(w, r)
			return
		}
		rec.record(r.Header)
		n++
		if n == 1 {
			writeJSON(w, http.StatusConflict, releasedStatusBody())
			return
		}
		writeJSON(w, http.StatusOK, releasedActiveBody())
	}

	inst, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatalf("admission after released rotation failed: %v", err)
	}
	if inst != "inst-released-1" {
		t.Errorf("instance = %q, want inst-released-1 (fresh-claim retry adopted)", inst)
	}
	hdrs := rec.snapshot()
	if len(hdrs) != 2 {
		t.Fatalf("admission POSTs = %d, want exactly 2 (released 409 + one rotated retry)", len(hdrs))
	}
	first, second := hdrs[0].Get("x-freebuff-instance-id"), hdrs[1].Get("x-freebuff-instance-id")
	if first != before {
		t.Errorf("first POST claim = %q, want stale claim %q", first, before)
	}
	if second == "" || second == before {
		t.Fatalf("second POST claim = %q, want a fresh claim (rotation before retry)", second)
	}
	assertClaimShape(t, second)
	if got := mgr.ClaimIDForTest(); got != second {
		t.Errorf("manager claim = %q, want retried claim %q", got, second)
	}
}

// TestAdmissionReleasedAlwaysSurfacesAfterOneRetry pins case A (the live
// token-1 shape): every claim — stale AND rotated — is refused, so the
// retry also 409s and the honest error surfaces after exactly one rotation.
func TestAdmissionReleasedAlwaysSurfacesAfterOneRetry(t *testing.T) {
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
		writeJSON(w, http.StatusConflict, releasedStatusBody())
	}

	_, err := mgr.EnsureSession(context.Background())
	asReleased409(t, err, false) // live refusal surfaces the upstream message text
	hdrs := rec.snapshot()
	if len(hdrs) != 2 {
		t.Fatalf("admission POSTs = %d, want exactly 2 (initial + one rotated retry, never a loop)", len(hdrs))
	}
	first, second := hdrs[0].Get("x-freebuff-instance-id"), hdrs[1].Get("x-freebuff-instance-id")
	if first != before {
		t.Errorf("first POST claim = %q, want stale claim %q", first, before)
	}
	if second == "" || second == before {
		t.Fatalf("second POST claim = %q, want a fresh claim (exactly one retry rotation)", second)
	}
	// The retry's 409 retires the second claim too, so the manager holds a
	// third fresh identity: every observed release rotates (single-use
	// rule), while the admission itself fires exactly twice.
	if got := mgr.ClaimIDForTest(); got == before || got == second {
		t.Errorf("manager claim = %q, want a fresh claim distinct from both POSTs (%q, %q)", got, before, second)
	} else {
		assertClaimShape(t, got)
	}
}

// TestReleasedRefusalShortCircuitsHotLoop pins the fail-fast memory: after
// a case-A failure the next request surfaces the same honest 409 with zero
// new upstream POSTs and zero claim churn; past the TTL it re-probes live.
func TestReleasedRefusalShortCircuitsHotLoop(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)
	now := time.Now()
	mgr.now = func() time.Time { return now }

	var rec attemptRecorder
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.Header)
		writeJSON(w, http.StatusConflict, releasedStatusBody())
	}

	_, err := mgr.EnsureSession(context.Background())
	asReleased409(t, err, false) // live refusal surfaces the upstream message text
	if n := len(rec.snapshot()); n != 2 {
		t.Fatalf("first-request POSTs = %d, want 2 (initial + one rotated retry)", n)
	}
	afterFirst := mgr.ClaimIDForTest()

	_, err = mgr.EnsureSession(context.Background())
	asReleased409(t, err, true) // fail-fast memory names the marker in its own body
	if n := len(rec.snapshot()); n != 2 {
		t.Fatalf("second-request POSTs = %d, want still 2 (fail-fast memory, no re-probe)", n)
	}
	if got := mgr.ClaimIDForTest(); got != afterFirst {
		t.Errorf("claim after fail-fast = %q, want %q (no rotation churn)", got, afterFirst)
	}

	now = now.Add(claimReleasedTTL + time.Second)
	_, err = mgr.EnsureSession(context.Background())
	asReleased409(t, err, false) // window expired: the live refusal again
	if n := len(rec.snapshot()); n != 4 {
		t.Fatalf("post-TTL POSTs = %d, want 4 (window expired: live re-probe)", n)
	}
	if got := mgr.ClaimIDForTest(); got == afterFirst {
		t.Errorf("claim after re-probe = %q, want a fresh rotation", got)
	}
}

// TestReleasedMemoryIsPerModel pins the live token-1 shape: the deepseek
// purchase is remembered as released while other models keep admitting
// live on the same token — and the released model keeps failing fast.
func TestReleasedMemoryIsPerModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	var rec attemptRecorder
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.Header)
		if r.Header.Get("x-freebuff-model") == releasedDeepseek {
			writeJSON(w, http.StatusConflict, releasedStatusBody())
			return
		}
		writeJSON(w, http.StatusOK, releasedActiveBody())
	}

	if _, err := mgr.EnsureSessionForModel(context.Background(), releasedDeepseek); err == nil {
		t.Fatal("deepseek admission: want the released 409, got nil")
	}
	if n := len(rec.snapshot()); n != 2 {
		t.Fatalf("deepseek POSTs = %d, want 2 (initial + one rotated retry)", n)
	}

	inst, err := mgr.EnsureSessionForModel(context.Background(), releasedMimo)
	if err != nil {
		t.Fatalf("mimo admission on a deepseek-released token failed: %v", err)
	}
	if inst != "inst-released-1" {
		t.Errorf("mimo instance = %q, want inst-released-1 (live admission, unaffected)", inst)
	}
	if n := len(rec.snapshot()); n != 3 {
		t.Fatalf("POSTs after mimo = %d, want 3 (mimo admitted live)", n)
	}

	if _, err := mgr.EnsureSessionForModel(context.Background(), releasedDeepseek); err == nil {
		t.Fatal("deepseek re-admission: want the fail-fast 409, got nil")
	}
	if n := len(rec.snapshot()); n != 3 {
		t.Fatalf("POSTs after deepseek retry = %d, want still 3 (fail-fast, mimo slot untouched)", n)
	}
	if got := mgr.Snapshot().InstanceID; got != "inst-released-1" {
		t.Errorf("cached instance = %q, want inst-released-1 (mimo slot survived the fail-fast)", got)
	}
}

// TestReleasedSnapshotListsModels pins the operator signal: the snapshot
// names the released model while remembered and clears it past the TTL.
func TestReleasedSnapshotListsModels(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)
	now := time.Now()
	mgr.now = func() time.Time { return now }

	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusConflict, releasedStatusBody())
	}
	if _, err := mgr.EnsureSessionForModel(context.Background(), releasedDeepseek); err == nil {
		t.Fatal("want the released 409, got nil")
	}
	got := mgr.Snapshot().ReleasedModels
	if len(got) != 1 || got[0] != releasedDeepseek {
		t.Fatalf("ReleasedModels = %q, want [%q]", got, releasedDeepseek)
	}
	now = now.Add(claimReleasedTTL + time.Second)
	if got := mgr.Snapshot().ReleasedModels; len(got) != 0 {
		t.Fatalf("ReleasedModels after TTL = %q, want empty (window expired)", got)
	}
}

// TestReleasedErrorFormRecordedWithoutRotation pins the {"error":...} wire
// shape: it surfaces unchanged with NO rotation (the rotation contract
// stays on the status form) but is still remembered, so the next request
// fails fast with zero POSTs.
func TestReleasedErrorFormRecordedWithoutRotation(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)
	before := mgr.ensureClaim()

	var rec attemptRecorder
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.Header)
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "purchase_claim_released",
			"message": "purchase gone",
		})
	}

	_, err := mgr.EnsureSession(context.Background())
	asReleased409(t, err, true) // raw error body carries the marker
	if n := len(rec.snapshot()); n != 1 {
		t.Fatalf("error-form POSTs = %d, want 1 (no rotation retry on this shape)", n)
	}
	if got := mgr.ClaimIDForTest(); got != before {
		t.Errorf("claim after error-form = %q, want stable %q (no rotation here)", got, before)
	}

	_, err = mgr.EnsureSession(context.Background())
	asReleased409(t, err, true) // fail-fast memory names the marker in its own body
	if n := len(rec.snapshot()); n != 1 {
		t.Fatalf("POSTs after remembered error-form = %d, want still 1 (fail-fast)", n)
	}
}
