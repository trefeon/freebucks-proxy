package server_test

import (
	"freebucks-proxy/backend/internal/testutil"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// TestChatRunInvalidRetriesWithFreshRun pins the retry-once contract of
// upstream.ErrRunInvalid ("the agent run is gone. Rotate the run and retry
// once.") end to end. Live defect (2026-09-21T07:05:05Z): a resumed run whose
// FINISH raced the request made upstream answer the chat with
// `400 {"message":"runId Not Running: …"}`; the classified ErrRunInvalid
// surfaced to the client as a bare 502 upstream_unavailable with no retry
// (attempts=1) even though the dead run had already been invalidated.
//
// The mock fails the FIRST chat with that exact 400 and serves a normal SSE
// stream on the SECOND, so the assertions cover the whole recovery: 200 to
// the client, exactly two chat calls, and a SECOND agent-run START — the
// retry must acquire a fresh run instead of re-adopting the dead one (the
// second chat body carries the new run id, proving no reuse).
func TestChatRunInvalidRetriesWithFreshRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Two distinct run ids so START #2 is observable in the chat body.
	mock.RunIDs = []string{"run-0001", "run-0002"}

	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		if chatCalls.Add(1) == 1 {
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
	_, ring, logger := debugRing(t)
	ts, _ := newTestServerWithLogger(t, nil, logger, ring, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (run-invalid must rotate the run and retry once): %s", resp.StatusCode, data)
	}
	if got := chatCalls.Load(); got != 2 {
		t.Errorf("upstream chat calls = %d, want exactly 2 (failed attempt + one retry)", got)
	}
	started := mock.StartedRunsSnapshot()
	if len(started) != 2 {
		t.Errorf("agent-run STARTs = %d, want 2 (the retry must START a fresh run, not reuse the invalidated one): %v",
			len(started), started)
	}
	// Only the RUN rotates: the cached session must survive the retry
	// (re-admitting would burn a second daily session slot).
	if got := mock.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("session creates = %d, want 1 (run-invalid recovery rotates the run, not the session)", got)
	}
	bodies := mock.RecordedChatBodiesSnapshot()
	if len(bodies) != 2 {
		t.Fatalf("recorded chat bodies = %d, want 2", len(bodies))
	}
	if !strings.Contains(bodies[0], `"run_id":"run-0001"`) {
		t.Errorf("first chat body missing the dead run id: %s", bodies[0])
	}
	if !strings.Contains(bodies[1], `"run_id":"run-0002"`) {
		t.Errorf("retry chat body did not carry the fresh run id: %s", bodies[1])
	}

	// The trace must attribute BOTH attempts (a two-attempt request reading
	// attempts=1 was the other half of the defect) and mark the retry.
	var retried, backoff, status string
	eventually(t, "chat trace with attempts=2", func() bool {
		for _, e := range ring.Recent(400) {
			if e.Message == "chat trace" && entryField(e, "attempts") == "2" {
				retried, backoff, status = entryField(e, "retried"), entryField(e, "backoff_ms"), entryField(e, "status")
				return true
			}
		}
		return false
	})
	if retried != "true" || backoff != "0" {
		t.Errorf("trace retry fields = retried=%q backoff_ms=%q, want retried=true backoff_ms=0", retried, backoff)
	}
	if status != "ok" {
		t.Errorf("trace status = %q, want ok (the retry succeeded)", status)
	}
}
