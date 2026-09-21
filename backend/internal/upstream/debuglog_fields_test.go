package upstream

import (
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/logring"
	"freebucks-proxy/backend/internal/testutil"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestUpstreamErrorLogCarriesReqIDStatusClass pins the debug-log contract
// for classified upstream errors: the `upstream response` line must carry
// the threaded req_id, the status, and the classification, and the
// `upstream rate limit classified` line must fire exactly once per response
// (do() classifies; parseSessionResponse reuses the pure matrix so the
// ledger line is never doubled).
func TestUpstreamErrorLogCarriesReqIDStatusClass(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	const upstreamBody = `{"error":"free_mode_rate_limited","message":"wait 1 minute","retryAfterMs":60000}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer srv.Close()

	client, err := New("tok-0", &config.Config{UpstreamBaseURL: srv.URL, CostMode: "free"})
	if err != nil {
		t.Fatal(err)
	}

	ring := logring.NewHandler(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}), 200)
	orig := slog.Default()
	slog.SetDefault(slog.New(ring))
	t.Cleanup(func() { slog.SetDefault(orig) })

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	req = req.WithContext(withReqID(req.Context(), "req-test-123"))
	resp, cancel, cerr := client.do(req, 5*time.Second)
	if cerr == nil {
		t.Fatal("expected a classified error from do() on >=400")
		return
	}
	defer cancel()
	_ = resp.Body.Close()

	entries := ring.Recent(200)
	fields := entryFields(entries, "upstream response")
	if fields == nil {
		t.Fatal("no `upstream response` line captured")
	}
	joined := strings.Join(fields, " ")
	for _, want := range []string{
		"method=POST",
		"path=/api/v1/chat/completions",
		"status=429",
		"class=RateLimitError",
		"req_id=req-test-123",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("`upstream response` missing %q in %s", want, joined)
		}
	}

	// Exactly-once classification: one ledger line, one ledger count.
	var classified int
	for _, e := range entries {
		if e.Message == "upstream rate limit classified" {
			classified++
			if got := strings.Join(e.Fields, " "); !strings.Contains(got, "req_id=req-test-123") {
				t.Errorf("classification line missing req_id in %s", got)
			}
		}
	}
	if classified != 1 {
		t.Errorf("`upstream rate limit classified` fired %d times, want exactly 1", classified)
	}
	if events := client.RateLimitEvents(); events["free_mode_rate_limited"] != 1 {
		t.Errorf("RateLimitEvents[free_mode_rate_limited] = %d, want 1 (all: %v)", events["free_mode_rate_limited"], events)
	}
}
