package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"freebucks-proxy/backend/internal/testutil"
)

// TestPollUntilActiveConfirmsAdmission: the post-admission confirm
// (CLI parity: admit → GET-poll-until-active → START → N×chat → FINISH).
// A queued row keeps polling, an active row returns at once, and any other
// settled status returns immediately — only queued can still become active
// without a fresh admission.
func TestPollUntilActiveConfirmsAdmission(t *testing.T) {
	t.Run("queued then active returns active", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		var calls atomic.Int32
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if calls.Add(1) < 3 {
				_, _ = io.WriteString(w, `{"status":"queued","instanceId":"inst-1"}`)
				return
			}
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1","expiresAt":"2030-01-01T00:00:00Z"}`)
		}
		client, err := New("tok-a", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		st, err := client.PollUntilActive(context.Background(), "inst-1", 10*time.Second)
		if err != nil {
			t.Fatalf("PollUntilActive: %v", err)
		}
		if st.Status != "active" {
			t.Errorf("status = %q, want active", st.Status)
		}
		if got := calls.Load(); got != 3 {
			t.Errorf("session polls = %d, want 3 (2 queued + confirm)", got)
		}
	})

	t.Run("already active returns in one poll", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		var calls atomic.Int32
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1","expiresAt":"2030-01-01T00:00:00Z"}`)
		}
		client, err := New("tok-b", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		st, err := client.PollUntilActive(context.Background(), "inst-1", 10*time.Second)
		if err != nil {
			t.Fatalf("PollUntilActive: %v", err)
		}
		if st.Status != "active" {
			t.Errorf("status = %q, want active", st.Status)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("session polls = %d, want 1 (no confirm loop needed)", got)
		}
	})

	t.Run("settled non-queued status returns immediately", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		var calls atomic.Int32
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-1"}`)
		}
		client, err := New("tok-c", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		st, err := client.PollUntilActive(context.Background(), "inst-1", 10*time.Second)
		if err != nil {
			t.Fatalf("PollUntilActive: %v", err)
		}
		if st.Status != "ended" {
			t.Errorf("status = %q, want ended (returned as-is for the caller to re-admit)", st.Status)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("session polls = %d, want 1 (ended never becomes active)", got)
		}
	})

	t.Run("budget exhaustion wraps deadline", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		var calls atomic.Int32
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"queued","instanceId":"inst-1"}`)
		}
		client, err := New("tok-d", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		last, err := client.PollUntilActive(context.Background(), "inst-1", 300*time.Millisecond)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
		if last == nil || last.Status != "queued" {
			t.Errorf("last state = %+v, want the queued row observed before the budget ran out", last)
		}
		if got := calls.Load(); got < 1 {
			t.Errorf("session polls = %d, want >= 1", got)
		}
	})

	t.Run("cancelled context aborts", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"queued","instanceId":"inst-1"}`)
		}
		client, err := New("tok-e", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = client.PollUntilActive(ctx, "inst-1", 10*time.Second)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})

	t.Run("non-positive budget defaults to 30s bound", func(t *testing.T) {
		if defaultPollActiveBudget != 30*time.Second {
			t.Errorf("defaultPollActiveBudget = %v, want 30s (post-admission confirm bound)", defaultPollActiveBudget)
		}
	})
}
