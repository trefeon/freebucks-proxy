package session

import (
	"context"
	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestStatusErrorTerminalRefusals pins the vendor af898dc taxonomy port:
// consent_required and the Desktop purchase-flow trio are terminal
// admission refusals (upstream cli/src/hooks/use-freebuff-session.ts
// nextDelayMs returns null = stop polling). They surface as plain
// *upstream.UpstreamError — no cooldown type, no retry — so the pool's
// classifyAndCooldown leaves the token alone and fails over.
func TestStatusErrorTerminalRefusals(t *testing.T) {
	t.Run("consent_required carries 409 plus spend", func(t *testing.T) {
		st := &upstream.SessionState{
			Status:        "consent_required",
			HTTPStatus:    http.StatusConflict,
			Message:       "",
			WalletConsent: &upstream.WalletConsent{Price: 5, WalletSpend: 2},
		}
		err := statusError("consent_required", st)
		if err == nil {
			t.Fatal("statusError = nil, want terminal refusal")
		}
		ue, ok := err.(*upstream.UpstreamError)
		if !ok {
			t.Fatalf("err type = %T, want *upstream.UpstreamError (no cooldown)", err)
		}
		if ue.Status != http.StatusConflict {
			t.Errorf("status = %d, want 409", ue.Status)
		}
		if !strings.Contains(ue.Body, "2") || !strings.Contains(ue.Body, "consent_required") {
			t.Errorf("body = %q, want spend plus code", ue.Body)
		}
		if ue.Retryable {
			t.Error("Retryable = true, want terminal (CLI retry:null)")
		}
	})

	for _, status := range []string{"purchase_claim_released", "purchase_in_use", "purchase_capacity", "premium_slot_taken"} {
		t.Run(status+" surfaces honest status", func(t *testing.T) {
			st := &upstream.SessionState{Status: status, HTTPStatus: http.StatusConflict, Message: "slot busy"}
			err := statusError(status, st)
			ue, ok := err.(*upstream.UpstreamError)
			if !ok {
				t.Fatalf("err type = %T, want *upstream.UpstreamError", err)
			}
			if ue.Status != http.StatusConflict || ue.Body != "slot busy" {
				t.Errorf("got %d %q, want 409 with upstream message", ue.Status, ue.Body)
			}
		})
	}

	t.Run("purchase without message gets default copy", func(t *testing.T) {
		err := statusError("purchase_capacity", &upstream.SessionState{Status: "purchase_capacity"})
		ue, ok := err.(*upstream.UpstreamError)
		if !ok {
			t.Fatalf("err type = %T, want *upstream.UpstreamError", err)
		}
		if ue.Status != http.StatusConflict {
			t.Errorf("status = %d, want 409 fallback", ue.Status)
		}
		if !strings.Contains(ue.Body, "purchase_capacity") {
			t.Errorf("body = %q, want status code name", ue.Body)
		}
	})
	t.Run("premium_slot_taken without message gets default copy", func(t *testing.T) {
		err := statusError("premium_slot_taken", &upstream.SessionState{Status: "premium_slot_taken"})
		ue, ok := err.(*upstream.UpstreamError)
		if !ok {
			t.Fatalf("err type = %T, want *upstream.UpstreamError", err)
		}
		if ue.Status != http.StatusConflict {
			t.Errorf("status = %d, want 409 fallback", ue.Status)
		}
		if !strings.Contains(ue.Body, "premium_slot_taken") {
			t.Errorf("body = %q, want status code name", ue.Body)
		}
	})
	t.Run("first_tab_discount_changed carries vendor copy", func(t *testing.T) {
		st := &upstream.SessionState{Status: "first_tab_discount_changed", HTTPStatus: http.StatusConflict}
		err := statusError("first_tab_discount_changed", st)
		if err == nil {
			t.Fatal("statusError = nil, want terminal refusal")
		}
		ue, ok := err.(*upstream.UpstreamError)
		if !ok {
			t.Fatalf("err type = %T, want *upstream.UpstreamError (no cooldown)", err)
		}
		if ue.Status != http.StatusConflict {
			t.Errorf("status = %d, want 409", ue.Status)
		}
		if !strings.Contains(ue.Body, "first-tab discount") || !strings.Contains(ue.Body, "first_tab_discount_changed") {
			t.Errorf("body = %q, want vendor copy plus code", ue.Body)
		}
		if ue.Retryable {
			t.Error("Retryable = true, want terminal (CLI retry:null)")
		}
	})
}

// TestRefreshTerminalRefusals drives the new statuses end to end through
// the admission loop: the manager returns the terminal error instead of
// looping, caching, or reporting an unknown status.
func TestRefreshTerminalRefusals(t *testing.T) {
	for _, tc := range []struct {
		status string
		body   string
		want   string
	}{
		{"consent_required", `{"status":"consent_required","walletConsent":{"price":5,"walletSpend":2},"freebucks":null}`, "consent_required"},
		{"first_tab_discount_changed", `{"status":"first_tab_discount_changed","accessTier":"full","freebucks":{"balance":10,"daily":{"limit":20,"spent":5,"remaining":15,"resetAt":"2030-01-01T00:00:00Z"},"prices":{"openai/gpt-5.6-luna":2}}}`, "first-tab discount"},
		{"purchase_in_use", `{"status":"purchase_in_use","message":"hour in use elsewhere"}`, "hour in use"},
		{"purchase_capacity", `{"status":"purchase_capacity"}`, "purchase_capacity"},
		{"purchase_claim_released", `{"status":"purchase_claim_released","message":"claim released"}`, "claim released"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mgr := newTestManager(t, mock)
			mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, tc.body)
					return
				}
				http.NotFound(w, r)
			}
			_, err := mgr.EnsureSession(context.Background())
			if err == nil {
				t.Fatalf("%s admitted, want terminal refusal", tc.status)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if _, ok := err.(*upstream.UpstreamError); !ok {
				t.Errorf("err type = %T, want *upstream.UpstreamError (no cooldown)", err)
			}
		})
	}
}

// TestEndSessionRefundSettled pins the receipt handler path: a DELETE
// returning freebucksRefund records lastRefund with no pending entry.
func TestEndSessionRefundSettled(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","freebucksRefund":1.5}`)
			return
		}
		http.NotFound(w, r)
	}
	if err := mgr.EndSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := mgr.Snapshot()
	if snap.LastRefund == nil || *snap.LastRefund != 1.5 {
		t.Errorf("LastRefund = %+v, want 1.5", snap.LastRefund)
	}
	if snap.PendingRefund != "" {
		t.Errorf("PendingRefund = %q, want empty (settled)", snap.PendingRefund)
	}
}

// TestEndSessionRefundPendingReplay pins the pending-refund store flow with
// replay semantics: a pending receipt parks the instance, a slotless
// EndSession replays its DELETE, and the settled replay records lastRefund
// (same amount on retry, per the wire comment).
func TestEndSessionRefundPendingReplay(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	deletes := 0
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes++
			w.Header().Set("Content-Type", "application/json")
			if deletes == 1 {
				_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","freebucksRefundPending":true}`)
			} else {
				_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","freebucksRefund":1.5}`)
			}
			return
		}
		http.NotFound(w, r)
	}
	if err := mgr.EndSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := mgr.Snapshot()
	if snap.PendingRefund == "" {
		t.Fatal("PendingRefund empty after pending receipt, want parked instance")
	}
	if snap.LastRefund != nil {
		t.Errorf("LastRefund = %v, want nil (unsettled)", *snap.LastRefund)
	}
	// Slotless teardown replays the parked DELETE and settles the same amount.
	if err := mgr.EndSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deletes != 2 {
		t.Errorf("deletes = %d, want 2 (release + replay)", deletes)
	}
	snap = mgr.Snapshot()
	if snap.PendingRefund != "" {
		t.Errorf("PendingRefund = %q, want cleared after settle", snap.PendingRefund)
	}
	if snap.LastRefund == nil || *snap.LastRefund != 1.5 {
		t.Errorf("LastRefund = %+v, want 1.5", snap.LastRefund)
	}
}

// TestRefreshRefundKeepsPendingOnError pins that a failed replay keeps the
// entry (a later EndSession retries) while a gone row clears it.
func TestRefreshRefundKeepsPendingOnError(t *testing.T) {
	t.Run("transport error keeps pending", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)
		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"ended","freebucksRefundPending":true}`)
				return
			}
			http.NotFound(w, r)
		}
		if err := mgr.EndSession(context.Background()); err != nil {
			t.Fatal(err)
		}
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"boom"}`)
		}
		if err := mgr.RefreshRefund(context.Background()); err == nil {
			t.Error("RefreshRefund 500 succeeded, want error")
		}
		if got := mgr.Snapshot().PendingRefund; got == "" {
			t.Error("PendingRefund cleared on failed replay, want kept")
		}
	})

	t.Run("gone row clears pending", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)
		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"ended","freebucksRefundPending":true}`)
				return
			}
			http.NotFound(w, r)
		}
		if err := mgr.EndSession(context.Background()); err != nil {
			t.Fatal(err)
		}
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"gone"}`)
		}
		if err := mgr.RefreshRefund(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := mgr.Snapshot().PendingRefund; got != "" {
			t.Errorf("PendingRefund = %q, want cleared (row gone)", got)
		}
	})
}

// TestRefreshRefundZeroSettles pins that a zero refund is a real receipt
// (CLI refreshRefund ?? 0): an ended replay without a freebucksRefund amount
// clears the parked instance and records lastRefund 0 (non-nil) instead of
// leaving the refund pending or unknown.
func TestRefreshRefundZeroSettles(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	deletes := 0
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.NotFound(w, r)
			return
		}
		deletes++
		w.Header().Set("Content-Type", "application/json")
		if deletes == 1 {
			_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","freebucksRefundPending":true}`)
		} else {
			_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123"}`)
		}
	}
	if err := mgr.EndSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := mgr.Snapshot().PendingRefund; got == "" {
		t.Fatal("PendingRefund empty after pending receipt, want parked instance")
	}
	if err := mgr.RefreshRefund(context.Background()); err != nil {
		t.Fatalf("RefreshRefund: %v", err)
	}
	snap := mgr.Snapshot()
	if snap.PendingRefund != "" {
		t.Errorf("PendingRefund = %q, want cleared after zero receipt", snap.PendingRefund)
	}
	if snap.LastRefund == nil {
		t.Fatal("LastRefund = nil after zero receipt, want non-nil 0 (a real receipt)")
	}
	if *snap.LastRefund != 0 {
		t.Errorf("LastRefund = %v, want 0", *snap.LastRefund)
	}
}

// TestPollRefundPendingRetained pins the between-polls holder: a refund
// receipt landing on a compact poll GET (ended + freebucksRefundPending,
// same parse as a DELETE receipt) parks the polled instance, and a later
// slotless EndSession replays its DELETE with the same instance id to
// settle — the balance is never mis-stated as unknown.
func TestPollRefundPendingRetained(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	var deleteIDs []string
	deletes := 0
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","freebucksRefundPending":true}`)
		case http.MethodDelete:
			deletes++
			deleteIDs = append(deleteIDs, r.Header.Get("x-freebuff-instance-id"))
			_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","freebucksRefund":1.5}`)
		default:
			http.NotFound(w, r)
		}
	}
	if err := mgr.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got := mgr.Snapshot().PendingRefund; got != "inst-abc-123" {
		t.Fatalf("PendingRefund = %q, want inst-abc-123 (poll receipt retained)", got)
	}
	if got := mgr.Snapshot().LastRefund; got != nil {
		t.Fatalf("LastRefund = %v, want nil (unsettled)", *got)
	}
	// The poll dropped the ended row, so this teardown is slotless: it
	// must replay the parked DELETE instead of stranding the settlement.
	if err := mgr.EndSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deletes != 1 {
		t.Fatalf("deletes = %d, want 1 (parked replay)", deletes)
	}
	if deleteIDs[0] != "inst-abc-123" {
		t.Errorf("replay DELETE instance = %q, want inst-abc-123 (same instance for receipt)", deleteIDs[0])
	}
	snap := mgr.Snapshot()
	if snap.PendingRefund != "" {
		t.Errorf("PendingRefund = %q, want cleared after settle", snap.PendingRefund)
	}
	if snap.LastRefund == nil || *snap.LastRefund != 1.5 {
		t.Errorf("LastRefund = %+v, want 1.5", snap.LastRefund)
	}
}

// TestPollRefundRetrySameAmount pins the vendor replay idempotence: every
// replay DELETE carries the same parked instance id, a still-pending retry
// keeps the entry, and the settled retry records the same amount the
// server returns for that instance.
func TestPollRefundRetrySameAmount(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	var deleteIDs []string
	deletes := 0
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","freebucksRefundPending":true}`)
		case http.MethodDelete:
			deletes++
			deleteIDs = append(deleteIDs, r.Header.Get("x-freebuff-instance-id"))
			if deletes == 1 {
				_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","freebucksRefundPending":true}`)
			} else {
				_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","freebucksRefund":2.5}`)
			}
		default:
			http.NotFound(w, r)
		}
	}
	if err := mgr.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got := mgr.Snapshot().PendingRefund; got != "inst-abc-123" {
		t.Fatalf("PendingRefund = %q, want inst-abc-123 (poll receipt retained)", got)
	}
	// First retry: server still computing — entry kept for the next replay.
	if err := mgr.RefreshRefund(context.Background()); err != nil {
		t.Fatalf("RefreshRefund retry 1: %v", err)
	}
	if got := mgr.Snapshot().PendingRefund; got != "inst-abc-123" {
		t.Errorf("PendingRefund = %q after still-pending retry, want kept", got)
	}
	if got := mgr.Snapshot().LastRefund; got != nil {
		t.Errorf("LastRefund = %v after still-pending retry, want nil", *got)
	}
	// Second retry: settled — same amount recorded, entry cleared.
	if err := mgr.RefreshRefund(context.Background()); err != nil {
		t.Fatalf("RefreshRefund retry 2: %v", err)
	}
	if deletes != 2 {
		t.Fatalf("deletes = %d, want 2 (two same-instance replays)", deletes)
	}
	for i, id := range deleteIDs {
		if id != "inst-abc-123" {
			t.Errorf("replay %d DELETE instance = %q, want inst-abc-123", i+1, id)
		}
	}
	snap := mgr.Snapshot()
	if snap.PendingRefund != "" {
		t.Errorf("PendingRefund = %q, want cleared after settle", snap.PendingRefund)
	}
	if snap.LastRefund == nil || *snap.LastRefund != 2.5 {
		t.Errorf("LastRefund = %+v, want 2.5 (same amount on retry)", snap.LastRefund)
	}
}
