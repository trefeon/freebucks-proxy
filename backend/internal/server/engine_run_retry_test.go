package server_test

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"freebucks-proxy/backend/internal/testutil"
)

// TestChatRunInvalidFailsFastSingleAttempt pins the fail-fast rule for
// upstream.ErrRunInvalid (400 runId Not Running — e.g. a resumed run whose
// FINISH raced the request, live 2026-09-21T07:05:05Z): the turn surfaces
// the refusal with exactly ONE upstream chat attempt and invalidates the
// dead run, so the NEXT turn STARTs fresh. No in-request rotate-and-retry —
// the poll resyncs and the next turn mints its own run.
func TestChatRunInvalidFailsFastSingleAttempt(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Two distinct run ids so the second turn's fresh START is observable
	// in the chat body.
	mock.RunIDs = []string{"run-0001", "run-0002"}

	var chatCalls atomic.Int32
	refuse := atomic.Bool{}
	refuse.Store(true)
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		if refuse.Load() {
			// Upstream's refusal for a run whose FINISH already landed.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"message":"runId Not Running: run-0001"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-runrr1", 1,
			`"choices":[{"index":0,"delta":{"content":"recovered"},"finish_reason":"stop"}]`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (run-invalid turn fails fast): %s", resp.StatusCode, data)
	}
	if got := chatCalls.Load(); got != 1 {
		t.Fatalf("upstream chat calls = %d, want exactly 1 (no in-request retry)", got)
	}
	if got := mock.StartedRunsSnapshot(); len(got) != 1 {
		t.Fatalf("agent-run STARTs = %d, want 1 (the failed turn mints nothing more): %v", len(got), got)
	}
	// Only the RUN dies: the cached session must survive the failed turn
	// (re-admitting would burn a second daily session slot).
	if got := mock.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("session creates = %d, want 1 (run-invalid drops the run, not the session)", got)
	}
	bodies := mock.RecordedChatBodiesSnapshot()
	if len(bodies) != 1 {
		t.Fatalf("recorded chat bodies = %d, want 1", len(bodies))
	}
	if !strings.Contains(bodies[0], `"run_id":"run-0001"`) {
		t.Errorf("failed chat body missing the dead run id: %s", bodies[0])
	}

	// The dead run was invalidated: the next turn STARTs a FRESH run and
	// succeeds — the poll resyncs without reusing run-0001.
	refuse.Store(false)
	resp2, data2 := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second turn status = %d, want 200: %s", resp2.StatusCode, data2)
	}
	if got := chatCalls.Load(); got != 2 {
		t.Errorf("upstream chat calls = %d, want 2 (1 per turn — no in-request retry)", got)
	}
	started := mock.StartedRunsSnapshot()
	if len(started) != 2 {
		t.Fatalf("agent-run STARTs = %d, want 2 (next turn STARTs fresh): %v", len(started), started)
	}
	bodies = mock.RecordedChatBodiesSnapshot()
	if len(bodies) != 2 {
		t.Fatalf("recorded chat bodies = %d, want 2", len(bodies))
	}
	if !strings.Contains(bodies[1], `"run_id":"run-0002"`) {
		t.Errorf("second turn chat body did not carry the fresh run id: %s", bodies[1])
	}
}
