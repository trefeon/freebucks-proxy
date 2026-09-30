package server_test

import (
	"freebuff-proxy/backend/internal/logring"
	"freebuff-proxy/backend/internal/testutil"
	"io"
	"net/http"
	"testing"
)

// Transparency: the chat trace names the wire shape and the usage source so
// a 0-token row is debuggable (upstream sent nothing) instead of ambiguous.
func ompTwoTools() string {
	return `[{"type":"function","function":{"name":"bash","description":"Run","parameters":{"type":"object","properties":{"command":{"type":"string"},"i":{"type":"string"}}}}},` +
		`{"type":"function","function":{"name":"read","description":"Read","parameters":{"type":"object","properties":{"path":{"type":"string"},"i":{"type":"string"}}}}}]`
}

func usageSSE(t *testing.T, withUsage bool) func(http.ResponseWriter, *http.Request) {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-tr1", 1,
			`"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}},"finish_reason":null}]`)))
		term := `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`
		if withUsage {
			term = `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":60,"completion_tokens":20,"total_tokens":80}`
		}
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-tr1", 1, term)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
}

func findTrace(entries []logring.Entry, status string) *logring.Entry {
	for i := range entries {
		if entries[i].Message == "chat trace" && entryField(entries[i], "status") == status {
			return &entries[i]
		}
	}
	return nil
}

func TestTraceCarriesWireShapeAndUsageSource(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = usageSSE(t, true)
	_, ring, logger := debugRing(t)
	ts, _ := newTestServerWithLogger(t, nil, logger, ring, mock)
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":true,"tools":` + ompTwoTools() + `}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var trace *logring.Entry
	eventually(t, "ok chat trace", func() bool {
		trace = findTrace(ring.Recent(400), "ok")
		return trace != nil
	})
	if got := entryField(*trace, "tools"); got != "2" {
		t.Errorf("trace tools = %q, want 2 (client-declared count)", got)
	}
	if got := entryField(*trace, "floor_only"); got != "true" {
		t.Errorf("trace floor_only = %q, want true (OMP-family wire)", got)
	}
	if got := entryField(*trace, "total"); got != "80" {
		t.Errorf("trace total = %q, want 80 (upstream usage)", got)
	}
	if got := entryField(*trace, "usage"); got != "" {
		t.Errorf("trace usage = %q, want empty (usage observed)", got)
	}
}

func TestTraceMarksUsageAbsent(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = usageSSE(t, false)
	_, ring, logger := debugRing(t)
	ts, _ := newTestServerWithLogger(t, nil, logger, ring, mock)
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":true,"tools":` + ompTwoTools() + `}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var trace *logring.Entry
	eventually(t, "ok chat trace", func() bool {
		trace = findTrace(ring.Recent(400), "ok")
		return trace != nil
	})
	if got := entryField(*trace, "usage"); got != "absent" {
		t.Errorf("trace usage = %q, want absent (no usage block observed)", got)
	}
	if got := entryField(*trace, "total"); got != "" {
		t.Errorf("trace total = %q, want empty (nothing observed)", got)
	}
}

func TestTraceOmitsFloorOnlyForPlainClients(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = usageSSE(t, true)
	_, ring, logger := debugRing(t)
	ts, _ := newTestServerWithLogger(t, nil, logger, ring, mock)
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(chatBody(modelA)), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var trace *logring.Entry
	eventually(t, "ok chat trace", func() bool {
		trace = findTrace(ring.Recent(400), "ok")
		return trace != nil
	})
	if got := entryField(*trace, "tools"); got != "0" {
		t.Errorf("trace tools = %q, want 0 (no client tools)", got)
	}
	if got := entryField(*trace, "floor_only"); got != "" {
		t.Errorf("trace floor_only = %q, want empty (plain client)", got)
	}
}
