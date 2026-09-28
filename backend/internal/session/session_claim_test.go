package session

// G4 purchase-claim lifecycle tests: one claim id per token lifetime,
// persisted in the unified store, re-POSTed on every admission, rotated
// only after an explicit upstream DELETE of the old row or a dead-claim
// terminal (use-freebuff-session.ts:576,1148-1203).

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
)

// claimShape pins the wire shape the manager mints: cli:<RFC4122-v4 UUID>,
// the same identity the upstream client sends as x-freebuff-instance-id.
func assertClaimShape(t *testing.T, claim string) {
	t.Helper()
	if !strings.HasPrefix(claim, "cli:") {
		t.Fatalf("claim = %q, want cli:<uuid> prefix", claim)
	}
	uuid := strings.TrimPrefix(claim, "cli:")
	if len(uuid) != 36 {
		t.Fatalf("claim uuid = %q, want 36-char v4 UUID", uuid)
	}
}

// TestAdmissionPostsSameClaimOnWire pins the end-to-end G4 behavior: two
// admissions (invalidate + re-admit, no DELETE) carry the IDENTICAL
// x-freebuff-instance-id on the wire — the mock records every admission
// POST's headers, so a per-admission fresh mint would show two distinct
// claims here.
func TestAdmissionPostsSameClaimOnWire(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr.Invalidate()
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	claims := mock.SessionCreateClaimsSnapshot()
	if len(claims) != 2 {
		t.Fatalf("admission claims = %q, want exactly 2 POSTs", claims)
	}
	if claims[0] == "" || claims[0] != claims[1] {
		t.Errorf("admission claims = %q, want the same cli:<uuid> on both POSTs", claims)
	}
	if got := mgr.ClaimIDForTest(); got != claims[0] {
		t.Errorf("manager claim = %q, want the wired claim %q", got, claims[0])
	}
}

// TestAdmissionPostsFreshClaimAfterDelete pins the wire side of
// DELETE-before-rotate: after EndSession DELETEs the row, the next
// admission POST carries a DIFFERENT claim.
func TestAdmissionPostsFreshClaimAfterDelete(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mgr.EndSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	claims := mock.SessionCreateClaimsSnapshot()
	if len(claims) != 2 {
		t.Fatalf("admission claims = %q, want exactly 2 POSTs", claims)
	}
	if claims[0] == "" || claims[0] == claims[1] {
		t.Errorf("admission claims = %q, want a fresh claim after the DELETE", claims)
	}
}

// TestClaimStableAcrossAdmissions pins the rejoin rule: invalidating (no
// DELETE) and re-admitting keeps the SAME claim — the next POST rejoins on
// it instead of minting a fresh purchase identity.
func TestClaimStableAcrossAdmissions(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := mgr.ClaimIDForTest()
	assertClaimShape(t, first)

	mgr.Invalidate()
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := mgr.ClaimIDForTest(); got != first {
		t.Errorf("claim after invalidate+re-admit = %q, want stable %q (rejoin re-POSTs it)", got, first)
	}
	if mock.SessionCreates != 2 {
		t.Errorf("creates = %d, want 2 (re-admission still POSTs, on the same claim)", mock.SessionCreates)
	}
}

// TestClaimPersistedAcrossRestart pins the relaunch handoff: a fresh manager
// on the same store rejoins on the persisted claim without minting, and the
// claim survives a session invalidation as a claim-only row (Load reports
// no session while LoadClaim still answers).
func TestClaimPersistedAcrossRestart(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	mgr, key := newPersistTestManager(t, mock, store)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	claim := mgr.ClaimIDForTest()
	assertClaimShape(t, claim)
	if got := store.LoadClaim(key); got != claim {
		t.Fatalf("stored claim = %q, want %q", got, claim)
	}

	// Invalidate (no DELETE): the session row drops but the claim stays.
	mgr.Invalidate()
	if got := store.Load(key); got != nil {
		t.Errorf("store.Load after invalidate = %+v, want nil (no session to resume)", got)
	}
	if got := store.LoadClaim(key); got != claim {
		t.Errorf("stored claim after invalidate = %q, want %q (claim outlives the row)", got, claim)
	}

	// Fresh manager, same store: rejoins on the persisted claim.
	mgr2, _ := newPersistTestManager(t, mock, store)
	if got := mgr2.ensureClaim(); got != claim {
		t.Errorf("restarted claim = %q, want persisted %q (relaunch handoff)", got, claim)
	}
	if _, err := mgr2.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := mgr2.ClaimIDForTest(); got != claim {
		t.Errorf("claim after restart re-admit = %q, want %q", got, claim)
	}
}

// TestClaimRotatesAfterExplicitDelete pins DELETE-before-rotate: EndSession
// DELETEs the live row, so the next admission rejoins on a FRESH claim.
func TestClaimRotatesAfterExplicitDelete(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	mgr, key := newPersistTestManager(t, mock, store)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := mgr.ClaimIDForTest()

	if err := mgr.EndSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	rotated := mgr.ClaimIDForTest()
	assertClaimShape(t, rotated)
	if rotated == first {
		t.Fatalf("claim after explicit DELETE = %q, want a fresh claim (old row is gone)", rotated)
	}
	if got := store.LoadClaim(key); got != rotated {
		t.Errorf("stored claim after DELETE = %q, want rotated %q", got, rotated)
	}

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := mgr.ClaimIDForTest(); got != rotated {
		t.Errorf("claim after re-admit = %q, want rotated %q (stable until the next DELETE)", got, rotated)
	}
}

// TestClaimRotatesOnSuperseded pins the taken-over terminal: a superseded
// admission drops the row AND retires the claim, so the loop's re-admit
// rejoins fresh instead of re-POSTing the dead identity.
func TestClaimRotatesOnSuperseded(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)
	mock.SessionSequence = []string{"superseded", "active"}

	before := mgr.ensureClaim()
	assertClaimShape(t, before)
	inst, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inst == "" {
		t.Fatal("want active instance after superseded → re-admit")
	}
	if got := mgr.ClaimIDForTest(); got == before {
		t.Errorf("claim after superseded = %q, want rotated (taken-over claim is dead)", got)
	}
	if mock.SessionCreates != 2 {
		t.Errorf("creates = %d, want 2 (superseded → re-admit on the fresh claim)", mock.SessionCreates)
	}
}

// TestClaimStableOnEndedRecreate pins the complement: ended/none rows are
// simply gone (not taken over), so the re-admit rejoins on the SAME claim.
func TestClaimStableOnEndedRecreate(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)
	mock.SessionSequence = []string{"ended", "active"}

	before := mgr.ensureClaim()
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := mgr.ClaimIDForTest(); got != before {
		t.Errorf("claim after ended → re-admit = %q, want stable %q", got, before)
	}
}

// TestClaimRotatesOnPurchaseClaimReleased pins the single-use rule (CLI
// claimRetired): a released claim rotates so the next admission does not
// re-POST the retired identity into another 409 loop.
func TestClaimRotatesOnPurchaseClaimReleased(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected %s (want only the admission POST)", r.Method)
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusConflict, map[string]any{
			"status":  "purchase_claim_released",
			"message": "claim released",
		})
	}

	before := mgr.ensureClaim()
	_, err := mgr.EnsureSession(context.Background())
	if err == nil {
		t.Fatal("want purchase_claim_released refusal, got nil")
	}
	var ue *upstream.UpstreamError
	if !errors.As(err, &ue) || ue.Status != http.StatusConflict {
		t.Fatalf("err = %v, want 409 UpstreamError", err)
	}
	if got := mgr.ClaimIDForTest(); got == before {
		t.Errorf("claim after purchase_claim_released = %q, want rotated (single-use claim retired)", got)
	}
}

// TestClaimStableOnRateLimited pins that a quota refusal is NOT a claim
// death: the token cools down and the next admission rejoins on the same
// claim.
func TestClaimStableOnRateLimited(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)
	mock.RateLimit = true

	before := mgr.ensureClaim()
	if _, err := mgr.EnsureSession(context.Background()); err == nil {
		t.Fatal("want rate_limited refusal, got nil")
	}
	if got := mgr.ClaimIDForTest(); got != before {
		t.Errorf("claim after rate_limited = %q, want stable %q", got, before)
	}
}
