package pool

import (
	"context"
	"freebucks-proxy/backend/internal/session"
	"freebucks-proxy/backend/internal/testutil"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// TestReplayPendingRefundsSettlesParkedEntry pins the pool boot sweep: a
// refund parked pre-restart (persisted in the session store) replays exactly
// once through the single-flight RefreshTokenRefund path with the same
// instance id and settles.
func TestReplayPendingRefundsSettlesParkedEntry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	toks := p.roster.Load()
	client := (*toks)[0].client
	store := session.NewStore(filepath.Join(t.TempDir(), "state.json"))

	// Simulate a container restart: fresh store-backed manager, empty
	// memory; the store holds the pre-restart parked entry.
	mgr := session.NewManagerWithStore(client, store)
	(*toks)[0].session = mgr
	store.SaveRefund(client.TokenKey(), "inst-parked-7", time.Now(), nil)

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
		_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-parked-7","freebucksRefund":3.5}`)
	}
	replayed, err := p.ReplayPendingRefunds(context.Background())
	if err != nil {
		t.Fatalf("ReplayPendingRefunds: %v", err)
	}
	if replayed != 1 {
		t.Fatalf("replayed = %d, want 1 (one parked entry)", replayed)
	}
	if deletes != 1 {
		t.Fatalf("deletes = %d, want 1 (exactly one same-instance replay)", deletes)
	}
	if replayedInstance != "inst-parked-7" {
		t.Errorf("replayed instance = %q, want parked inst-parked-7", replayedInstance)
	}
	snap := mgr.Snapshot()
	if snap.PendingRefund != "" {
		t.Errorf("PendingRefund = %q, want cleared after settle", snap.PendingRefund)
	}
	if snap.LastRefund == nil || *snap.LastRefund != 3.5 {
		t.Errorf("LastRefund = %+v, want 3.5", snap.LastRefund)
	}
}

// TestReplayPendingRefundsIdleNoop pins the steady state: with nothing
// parked the sweep issues no upstream traffic.
func TestReplayPendingRefundsIdleNoop(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected upstream call during idle sweep")
		http.NotFound(w, r)
	}
	replayed, err := p.ReplayPendingRefunds(context.Background())
	if err != nil {
		t.Fatalf("ReplayPendingRefunds: %v", err)
	}
	if replayed != 0 {
		t.Errorf("replayed = %d, want 0 (nothing parked)", replayed)
	}
}

// TestReplayPendingRefundsPartialError pins the sweep's warn-and-continue
// rule: when one slot's replay fails the entry stays parked (persisted) for
// the next trigger, the remaining slots still settle, and the sweep reports
// how many replays ran plus the first error.
func TestReplayPendingRefundsPartialError(t *testing.T) {
	mockFail := testutil.NewMock()
	defer mockFail.Close()
	mockOK := testutil.NewMock()
	defer mockOK.Close()
	p := newTestPool(t, mockFail, mockOK)
	toks := p.roster.Load()
	if len(*toks) != 2 {
		t.Fatalf("roster slots = %d, want 2", len(*toks))
	}
	store := session.NewStore(filepath.Join(t.TempDir(), "state.json"))
	keys := make([]string, 2)
	for i := range *toks {
		client := (*toks)[i].client
		(*toks)[i].session = session.NewManagerWithStore(client, store)
		keys[i] = client.TokenKey()
	}
	store.SaveRefund(keys[0], "inst-parked-fail", time.Now(), nil)
	store.SaveRefund(keys[1], "inst-parked-ok", time.Now(), nil)

	mockFail.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	}
	mockOK.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-parked-ok","freebucksRefund":1.0}`)
	}
	replayed, err := p.ReplayPendingRefunds(context.Background())
	if err == nil {
		t.Error("ReplayPendingRefunds err = nil, want the failed slot's error")
	}
	if replayed != 1 {
		t.Errorf("replayed = %d, want 1 (failed slot stays parked, ok slot settles)", replayed)
	}
	if got := (*toks)[0].session.Snapshot().PendingRefund; got != "inst-parked-fail" {
		t.Errorf("slot 0 PendingRefund = %q, want still parked inst-parked-fail", got)
	}
	if pending, _, _ := store.LoadRefund(keys[0]); pending != "inst-parked-fail" {
		t.Errorf("stored slot 0 PendingRefund = %q, want still parked", pending)
	}
	if got := (*toks)[1].session.Snapshot().PendingRefund; got != "" {
		t.Errorf("slot 1 PendingRefund = %q, want cleared after settle", got)
	}
}
