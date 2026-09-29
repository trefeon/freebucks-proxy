package server_test

// Debug-log coverage for the server request flow: the error trace must carry
// the failed run's attribution (the lease is released before the error
// returns), and the client-visible 5xx path must log at ERROR.

import (
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"freebuff-proxy/backend/internal/logring"
	"freebuff-proxy/backend/internal/testutil"
)

// TestChatTraceErrorCarriesFailedRunAttribution pins the error-trace
// attribution: the single chat attempt refuses with the incident shape
// (2026-09-21T07:05:05Z 400 runId Not Running) and fails fast, so no
// success path is involved — the "chat trace" error line must still carry
// the failed run's run_id + trace_session_id + attempts, matching the ok path
// field-for-field.
func TestChatTraceErrorCarriesFailedRunAttribution(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RunIDs = []string{"run-0001"}

	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"runId Not Running: run-0001"}`)
	}
	_, ring, logger := debugRing(t)
	ts, _ := newTestServerWithLogger(t, nil, logger, ring, mock)

	resp, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (run-invalid turn fails fast as upstream_unavailable)", resp.StatusCode)
	}
	if got := chatCalls.Load(); got != 1 {
		t.Fatalf("upstream chat calls = %d, want exactly 1 (single attempt, no retry)", got)
	}

	var trace logring.Entry
	eventually(t, "error chat trace with attempts=1", func() bool {
		for _, e := range ring.Recent(400) {
			if e.Message == "chat trace" && entryField(e, "status") == "error" && entryField(e, "attempts") == "1" {
				trace = e
				return true
			}
		}
		return false
	})
	// The trace names the run that served (and failed) the single attempt.
	if got := entryField(trace, "run_id"); got != "run-0001" {
		t.Errorf("trace run_id = %q, want the failed attempt's run run-0001", got)
	}
	if got := entryField(trace, "trace_session_id"); got == "" {
		t.Error("trace trace_session_id empty, want the failed run's session id")
	}
	if got := entryField(trace, "error"); got == "" {
		t.Error("trace error class empty, want the failure bucket")
	}
	// Single-attempt fail-fast: no retry ever fires, so the trace carries
	// no retried marker.
	if got := entryField(trace, "retried"); got != "" {
		t.Errorf("trace retried = %q, want empty (no in-request retry exists)", got)
	}
	// Sentinel-classified refusals (run-invalid) carry no upstream HTTP
	// status, so the attempt appends none: attempts=1 with no
	// statuses_seen is the correct render, not a gap.
	if got := entryField(trace, "statuses_seen"); got != "" {
		t.Errorf("trace statuses_seen = %q, want empty (run-invalid records no per-attempt status)", got)
	}
}

// TestRequestFailedFiveHundredLogsError pins the level policy for the
// client-visible 5xx path: a 5xx surface must log "request failed" at ERROR
// (4xx refusals stay WARN, routine 429 churn stays INFO).
func TestRequestFailedFiveHundredLogsError(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}
	_, ring, logger := debugRing(t)
	ts, _ := newTestServerWithLogger(t, nil, logger, ring, mock)

	resp, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode < 500 || resp.StatusCode > 599 {
		t.Fatalf("status = %d, want a 5xx surface", resp.StatusCode)
	}
	eventually(t, "ERROR request failed entry", func() bool {
		for _, e := range ring.Recent(400) {
			if e.Message == "request failed" && e.Level == "ERROR" {
				return true
			}
		}
		return false
	})
}
