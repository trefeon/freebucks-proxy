package server

// Chat-trace token attribution: an acquire-time 429 (nil lease, pool fully
// cooling) must still log token=<binding 1-based> + rate_tokens=<csv> with
// error=rate_limited; an egress refusal (model_ip_limited, no token
// involved) must keep TOKEN —; a post-acquire 429 keeps the serving lease's
// token. A successful chat must land one record in the usage ring.

import (
	"bytes"
	"encoding/json"
	"freebucks-proxy/backend/internal/logring"
	"freebucks-proxy/backend/internal/pool"
	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const traceAttrModel = "deepseek/deepseek-v4-flash"

func traceAttrBody() []byte {
	return []byte(`{"model":"` + traceAttrModel + `","messages":[{"role":"user","content":"ping"}],"stream":true}`)
}

func newTraceAttrStack(t *testing.T, mocks ...*testutil.MockUpstream) (*Server, *pool.Pool, *logring.Handler, *httptest.Server) {
	t.Helper()
	ring := logring.NewHandler(slog.NewTextHandler(io.Discard, nil), 500)
	srv, p := newTestServerStack(t, nil, mocks, nil, slog.New(ring), ring)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, p, ring, ts
}

func postTraceAttrChat(t *testing.T, ts *httptest.Server) (int, []byte) {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(traceAttrBody()))
	if err != nil {
		t.Fatalf("POST chat: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read chat body: %v", err)
	}
	return resp.StatusCode, data
}

// lastChatTrace returns the most recent "chat trace" ring entry's fields.
func lastChatTrace(t *testing.T, ring *logring.Handler) []string {
	t.Helper()
	var last []string
	for _, e := range ring.Recent(500) {
		if e.Message == "chat trace" {
			last = e.Fields
		}
	}
	if last == nil {
		t.Fatal("no \"chat trace\" entry in the ring")
	}
	return last
}

func traceField(fields []string, key string) (string, bool) {
	for _, f := range fields {
		if k, v, ok := strings.Cut(f, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// All tokens cooling: the trace names the shortest-window token (token 2,
// 5m < 30m) plus the full limited set, with error=rate_limited — no more
// bare TOKEN — on pool exhaustion.
func TestChatTraceAcquireRateLimitNamesBindingToken(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	_, p, ring, ts := newTraceAttrStack(t, mock0, mock1)

	p.CooldownTokenRateLimit(0, &upstream.RateLimitError{Body: "rate limit", RetryAfter: 30 * time.Minute})
	p.CooldownTokenRateLimit(1, &upstream.RateLimitError{Body: "rate limit", RetryAfter: 5 * time.Minute})

	if status, body := postTraceAttrChat(t, ts); status != http.StatusTooManyRequests {
		t.Fatalf("chat status = %d, want 429: %s", status, body)
	}
	fields := lastChatTrace(t, ring)
	if got, _ := traceField(fields, "token"); got != "2" {
		t.Errorf("trace token = %q, want \"2\" (shortest-window binding): %v", got, fields)
	}
	if got, _ := traceField(fields, "rate_tokens"); got != "1,2" {
		t.Errorf("trace rate_tokens = %q, want \"1,2\": %v", got, fields)
	}
	if got, _ := traceField(fields, "error"); got != "rate_limited" {
		t.Errorf("trace error = %q, want \"rate_limited\": %v", got, fields)
	}
}

// Post-acquire upstream 429: the lease was released before the return, but
// / the limited set was never enumerated on this path).
func TestChatTracePostAcquire429KeepsLeaseToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = http.StatusTooManyRequests
	mock.ChatErrorBody = `{"error":"free_mode_rate_limited","message":"wait 30 minutes","retryAfterMs":1800000}`
	_, _, ring, ts := newTraceAttrStack(t, mock)

	if status, body := postTraceAttrChat(t, ts); status != http.StatusTooManyRequests {
		t.Fatalf("chat status = %d, want 429: %s", status, body)
	}
	fields := lastChatTrace(t, ring)
	if got, _ := traceField(fields, "token"); got != "1" {
		t.Errorf("trace token = %q, want \"1\" (serving lease): %v", got, fields)
	}
	if got, _ := traceField(fields, "error"); got != "rate_limited" {
		t.Errorf("trace error = %q, want \"rate_limited\": %v", got, fields)
	}
	if got, ok := traceField(fields, "rate_tokens"); ok {
		t.Errorf("post-acquire 429 trace carries rate_tokens=%q, want absent: %v", got, fields)
	}
}

// One successful chat lands exactly one record in the dashboard usage ring.
func TestSuccessfulChatReachesUsageRing(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testUsageChunk("chatcmpl-u1")
	srv, _, _, ts := newTraceAttrStack(t, mock)

	if status, body := postTraceAttrChat(t, ts); status != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", status, body)
	}
	rec := httptest.NewRecorder()
	srv.dash.APIHandler("usage")(rec, httptest.NewRequest(http.MethodGet, "/admin/api/usage", nil))
	var out struct {
		Entries []struct {
			Model string `json:"model"`
			Total int64  `json:"total"`
			ReqID string `json:"req_id"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("usage response is not valid JSON: %v (body %q)", err, rec.Body.String())
	}
	if len(out.Entries) != 1 {
		t.Fatalf("usage entries = %d, want 1: %s", len(out.Entries), rec.Body.String())
	}
	if out.Entries[0].Model != traceAttrModel || out.Entries[0].Total != 366 || out.Entries[0].ReqID == "" {
		t.Errorf("usage entry = %+v, want model=%s total=366 with req_id", out.Entries[0], traceAttrModel)
	}
}
