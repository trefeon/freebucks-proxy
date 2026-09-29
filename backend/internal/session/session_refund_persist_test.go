package session

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
)

// TestRefundSurvivesReload pins the P0 spend-correctness gap: a parked
// pending refund (freebucksRefundPending receipt) must survive a process
// restart through the session store, so a boot replay can settle it with
// the same instance id. Today pendingRefund/lastRefund are memory-only and
// the parked instance id is lost on restart.
func TestRefundSurvivesReload(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	mgr, key := newPersistTestManager(t, mock, store)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","freebucksRefundPending":true}`)
			return
		}
		http.NotFound(w, r)
	}
	if err := mgr.EndSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	parked := mgr.Snapshot().PendingRefund
	if parked == "" {
		t.Fatal("PendingRefund empty after pending receipt, want parked instance")
	}

	// Simulate a container restart: fresh manager, same store + token key.
	// Memory is empty; the store must still hold the parked entry.
	mgr2, key2 := newPersistTestManager(t, mock, store)
	if key2 != key {
		t.Fatalf("reloaded key = %q, want %q", key2, key)
	}
	if got := mgr2.Snapshot().PendingRefund; got != parked {
		t.Fatalf("reloaded PendingRefund = %q, want parked %q", got, parked)
	}
}

// TestStoreKeepsParkedRefundAcrossSessionWrites pins the row lifecycle: a
// live-session save carries a parked refund over, and a session invalidation
// (nil save) downgrades to a refund-only entry instead of dropping the
// parked instance id.
func TestStoreKeepsParkedRefundAcrossSessionWrites(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	const key = "tokhash"
	store.SaveRefund(key, "inst-parked-9", time.Now(), nil)

	// A later admission commit must not clobber the parked entry.
	store.Save(key, activeSlot("inst-live-1", "m"))
	if pending, _, _ := store.LoadRefund(key); pending != "inst-parked-9" {
		t.Fatalf("LoadRefund after live save = %q, want inst-parked-9", pending)
	}
	if cs := store.Load(key); cs == nil || cs.instanceID != "inst-live-1" {
		t.Fatalf("Load after live save = %+v, want live inst-live-1", cs)
	}

	// Invalidating the session keeps the parked entry as a refund-only row.
	store.Save(key, nil)
	if pending, _, _ := store.LoadRefund(key); pending != "inst-parked-9" {
		t.Fatalf("LoadRefund after nil save = %q, want inst-parked-9", pending)
	}
	if cs := store.Load(key); cs != nil && cs.instanceID != "" {
		t.Fatalf("Load after nil save = %+v, want no live session", cs)
	}

	// Clearing the tracking on a refund-only row deletes the row.
	store.SaveRefund(key, "", time.Time{}, nil)
	if pending, _, last := store.LoadRefund(key); pending != "" || last != nil {
		t.Fatalf("LoadRefund after clear = (%q, %+v), want empty", pending, last)
	}
}

// TestRefundBootReplaySettles pins the boot sweep: a pre-seeded parked entry
// replays exactly one same-instance DELETE on boot and settles, clearing
// both memory and the store row. The fresh manager restores the parked entry
// from disk on first Snapshot, then RefreshRefund replays it — the same path
// the pool boot sweep drives per slot through RefreshTokenRefund.
func TestRefundBootReplaySettles(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	_, key := newPersistTestManager(t, mock, store)
	// Pre-restart park, as a pre-restart release would have written it.
	store.SaveRefund(key, "inst-parked-1", time.Now(), nil)

	// Fresh manager, empty memory: Snapshot restores the parked entry from
	// disk, RefreshRefund replays it.
	mgr, _ := newPersistTestManager(t, mock, store)
	deletes := 0
	var replayedInstance string
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.NotFound(w, r)
			return
		}
		deletes++
		replayedInstance = r.Header.Get("x-freebuff-instance-id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-parked-1","freebucksRefund":2.25}`)
	}
	if got := mgr.Snapshot().PendingRefund; got != "inst-parked-1" {
		t.Fatalf("reloaded PendingRefund = %q, want parked inst-parked-1", got)
	}
	if err := mgr.RefreshRefund(context.Background()); err != nil {
		t.Fatalf("RefreshRefund: %v", err)
	}
	if deletes != 1 {
		t.Fatalf("deletes = %d, want 1 (exactly one boot replay)", deletes)
	}
	if replayedInstance != "inst-parked-1" {
		t.Errorf("replayed instance = %q, want parked inst-parked-1 (same-instance replay)", replayedInstance)
	}
	snap := mgr.Snapshot()
	if snap.PendingRefund != "" {
		t.Errorf("PendingRefund = %q, want cleared after settle", snap.PendingRefund)
	}
	if snap.LastRefund == nil || *snap.LastRefund != 2.25 {
		t.Errorf("LastRefund = %+v, want 2.25", snap.LastRefund)
	}
	// The disk entry settles too: a later restart sees nothing parked.
	if pending, _, _ := store.LoadRefund(key); pending != "" {
		t.Errorf("stored PendingRefund = %q, want cleared after settle", pending)
	}
	mgr3, _ := newPersistTestManager(t, mock, store)
	if got := mgr3.Snapshot().PendingRefund; got != "" {
		t.Errorf("post-settle reload PendingRefund = %q, want empty", got)
	}
}

// TestRefundBootReplayGoneClears pins that a boot replay against a row the
// upstream already dropped clears the parked entry instead of keeping it
// (same gone-row rule as the dashboard trigger).
func TestRefundBootReplayGoneClears(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	_, key := newPersistTestManager(t, mock, store)
	store.SaveRefund(key, "inst-parked-gone", time.Now(), nil)

	mgr, _ := newPersistTestManager(t, mock, store)
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"gone"}`)
	}
	if got := mgr.Snapshot().PendingRefund; got != "inst-parked-gone" {
		t.Fatalf("reloaded PendingRefund = %q, want parked inst-parked-gone", got)
	}
	if err := mgr.RefreshRefund(context.Background()); err != nil {
		t.Fatalf("RefreshRefund: %v", err)
	}
	if got := mgr.Snapshot().PendingRefund; got != "" {
		t.Errorf("PendingRefund = %q, want cleared (row gone)", got)
	}
	if pending, _, _ := store.LoadRefund(key); pending != "" {
		t.Errorf("stored PendingRefund = %q, want cleared (row gone)", pending)
	}
}

// TestRefundBootReplayGraceExpiredRowStillReplays pins that a parked refund
// replays even when its session row is long past the grace window: the store
// trims the dead row to a refund-only entry (Load drops the session but keeps
// the parked instance), and the fresh-load + RefreshRefund path still
// settles it with one same-instance DELETE.
func TestRefundBootReplayGraceExpiredRowStillReplays(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	_, key := newPersistTestManager(t, mock, store)
	// Dead live row (grace closed an hour ago) plus a parked refund.
	store.Save(key, &cachedState{
		status:            "active",
		instanceID:        "inst-old",
		model:             "m",
		expiresAt:         time.Now().Add(-2 * time.Hour),
		gracePeriodEndsAt: time.Now().Add(-time.Hour),
	})
	store.SaveRefund(key, "inst-parked-grace", time.Now(), nil)

	mgr, _ := newPersistTestManager(t, mock, store)
	deletes := 0
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.NotFound(w, r)
			return
		}
		deletes++
		if got := r.Header.Get("x-freebuff-instance-id"); got != "inst-parked-grace" {
			t.Errorf("replayed instance = %q, want parked inst-parked-grace", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-parked-grace","freebucksRefund":1.5}`)
	}
	if got := mgr.Snapshot().PendingRefund; got != "inst-parked-grace" {
		t.Fatalf("reloaded PendingRefund = %q, want parked inst-parked-grace (refund survives grace trim)", got)
	}
	if err := mgr.RefreshRefund(context.Background()); err != nil {
		t.Fatalf("RefreshRefund: %v", err)
	}
	if deletes != 1 {
		t.Fatalf("deletes = %d, want 1 (exactly one grace-expired replay)", deletes)
	}
	if got := mgr.Snapshot().PendingRefund; got != "" {
		t.Errorf("PendingRefund = %q, want cleared after settle", got)
	}
}

// TestStoreRemoveDowngradesToRefundOnly pins the Remove lifecycle: dropping a
// live row that carries a parked refund keeps the refund (downgrade to a
// refund-only entry) instead of stranding its settlement, while a mismatched
// expected instance leaves the row untouched and a refund-less row deletes.
func TestStoreRemoveDowngradesToRefundOnly(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	live := func(inst string) *cachedState {
		expiry := time.Now().Add(time.Hour)
		return &cachedState{status: "active", instanceID: inst, model: "m", expiresAt: expiry, gracePeriodEndsAt: expiry.Add(graceWindow)}
	}
	store.Save("k", live("inst-live"))
	store.SaveRefund("k", "inst-parked-1", time.Now(), nil)

	// Mismatched expected instance: no clobber of the newer row.
	store.Remove("k", "inst-other")
	if got := store.Load("k"); got == nil || got.instanceID != "inst-live" {
		t.Fatalf("Load after mismatched Remove = %+v, want live inst-live row kept", got)
	}

	// Matching invalidation: live fields drop, the parked refund survives.
	// Load reports no resumable session (nil — a sessionless row holds
	// nothing to adopt); the refund survives via LoadRefund for the boot
	// replay below.
	store.Remove("k", "inst-live")
	if got := store.Load("k"); got != nil {
		t.Fatalf("Load after matching Remove = %+v, want nil (nothing resumable)", got)
	}
	if pending, _, _ := store.LoadRefund("k"); pending != "inst-parked-1" {
		t.Errorf("LoadRefund after Remove = %q, want parked inst-parked-1", pending)
	}

	// Refund-less row: Remove deletes outright.
	store.Save("plain", live("inst-plain"))
	store.Remove("plain", "inst-plain")
	if got := store.Load("plain"); got != nil {
		t.Errorf("Load after refund-less Remove = %+v, want nil (deleted)", got)
	}
}
