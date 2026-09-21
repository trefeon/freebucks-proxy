package session

// Seat-gate tests for the pre-emptive re-admit (pool/seat.go): upstream keeps
// ONE session per account for CLI/web clients, so an admission rotates
// active_instance_id and 409s every completion still in flight on the old
// row. The rotation must therefore never start while another turn holds the
// seat, and the request that does start one must be served by the instance it
// lands instead of riding the row it just superseded.

import (
	"context"
	"freebucks-proxy/backend/internal/testutil"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// seatWindowMock serves a session whose expiry sits inside the re-admit lead
// window (10s left against the test's 60s lead) and numbers each created
// instance inst-1, inst-2, ...
func seatWindowMock(creates *atomic.Int32) *testutil.MockUpstream {
	mock := testutil.NewMock()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusOK, map[string]any{
				"status":     "active",
				"instanceId": "inst-1",
				"expiresAt":  time.Now().Add(30 * time.Minute).Format(time.RFC3339),
			})
			return
		}
		id := "inst-1"
		if creates.Add(1) >= 2 {
			id = "inst-2"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "active",
			"instanceId": id,
			"expiresAt":  time.Now().Add(10 * time.Second).Format(time.RFC3339),
		})
	}
	return mock
}

// TestReAdmitDeferredWhileSeatBusy pins the gate: while the seat is in use the
// trigger is skipped ENTIRELY — no upstream create, no rotation — and the
// request rides the cached instance, so no concurrent turn can be superseded.
// Freeing the seat lets the next request rotate.
func TestReAdmitDeferredWhileSeatBusy(t *testing.T) {
	var creates atomic.Int32
	mock := seatWindowMock(&creates)
	defer mock.Close()

	m := newTestSession(t, mock)
	m.SetReAdmitLead(time.Minute)
	var busy atomic.Bool
	busy.Store(true)
	m.SetReAdmitGate(func() bool { return !busy.Load() })

	first, err := m.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != "inst-1" {
		t.Fatalf("first instance = %q, want inst-1", first)
	}
	// Every call inside the lead window while the seat is busy rides the
	// cached instance — a deferred trigger must not even reach upstream.
	for i := range 3 {
		got, err := m.EnsureSession(context.Background())
		if err != nil {
			t.Fatalf("EnsureSession #%d: %v", i+2, err)
		}
		if got != "inst-1" {
			t.Fatalf("EnsureSession #%d = %q, want inst-1 (ride the held seat)", i+2, got)
		}
	}
	if n := creates.Load(); n != 1 {
		t.Fatalf("session creates with the seat busy = %d, want 1 (re-admit deferred, no rotation)", n)
	}

	// Seat idle: the trigger fires and THIS request is served by the new
	// instance. Dispatching it on the row it just superseded is exactly the
	// live 2026-09-21 failure (two in-flight turns 409'd 21s and 46s after a
	// lead trigger).
	busy.Store(false)
	got, err := m.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "inst-2" {
		t.Fatalf("EnsureSession with an idle seat = %q, want inst-2 (dispatch on the new instance)", got)
	}
	if n := creates.Load(); n != 2 {
		t.Fatalf("session creates = %d, want 2", n)
	}
}
