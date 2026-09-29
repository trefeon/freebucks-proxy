package server_test

import (
	"freebuff-proxy/backend/internal/testutil"
	"net/http"
	"strings"
	"testing"
)

// TestProberBypass: 9Router-style external healthcheck probers must be served
// without triggering upstream session admission.
func TestProberBypass(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)
	defer ts.Close()

	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}]}`
	hdr := map[string]string{"Content-Type": "application/json", "User-Agent": "9router-healthcheck/1.0"}
	resp, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), hdr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := mock.SessionCreatesSnapshot(); got != 0 {
		t.Fatalf("session creates = %d, want 0 (prober must not trigger admission)", got)
	}

	// Narrowness: the identical body from a genuine client UA must still admit.
	genuine := map[string]string{"Content-Type": "application/json", "User-Agent": "genuine-client/1.0"}
	resp, _ = doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), genuine)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("genuine status = %d, want 200", resp.StatusCode)
	}
	if got := mock.SessionCreatesSnapshot(); got != 1 {
		t.Fatalf("session creates after genuine = %d, want 1 (genuine requests unaffected)", got)
	}
}

// TestProberBypassStream serves the SSE shape without admission: the chunk
// carries the probe model, an "ok" delta, and a terminal [DONE] sentinel.
func TestProberBypassStream(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)
	defer ts.Close()

	body := `{"model":"` + modelA + `","stream":true,"messages":[{"role":"user","content":"ping"}]}`
	hdr := map[string]string{"Content-Type": "application/json", "User-Agent": "9router-healthcheck/1.0"}
	resp, raw := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), hdr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(string(raw), `"ok"`) || !strings.Contains(string(raw), "data: [DONE]") {
		t.Errorf("SSE body missing ok delta or [DONE] sentinel:\n%s", raw)
	}
	if got := mock.SessionCreatesSnapshot(); got != 0 {
		t.Fatalf("session creates = %d, want 0 (stream probe must not trigger admission)", got)
	}
}

// TestProberBypassFallThrough: a prober UA alone never bypasses. Tools or a
// multi-message turn fall through to normal admission even from a prober UA.
func TestProberBypassFallThrough(t *testing.T) {
	hdr := map[string]string{"Content-Type": "application/json", "User-Agent": "9router-healthcheck/1.0"}

	cases := map[string]string{
		"tools":         `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"tools":[]}`,
		"multi-message": `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"},{"role":"user","content":"hi"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			// Fresh stack per case: the pool reuses a live session, so a
			// shared server would admit only the first fall-through.
			mock := testutil.NewMock()
			defer mock.Close()
			ts, _ := newTestServer(t, nil, mock)
			defer ts.Close()
			resp, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), hdr)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (must fall through to admission)", resp.StatusCode)
			}
			if got := mock.SessionCreatesSnapshot(); got != 1 {
				t.Errorf("session creates = %d, want 1 (prober UA with %s must admit)", got, name)
			}
		})
	}
}
