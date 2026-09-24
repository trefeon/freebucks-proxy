package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/pool"
	"freebucks-proxy/backend/internal/testutil"
	"strings"
)

// newPooledTestServer wires the server pool-only: AUTH_TOKENS pool with the
// given client API keys. The pool's fixed tokens are "tok-0", "tok-1", ...
// one per mock upstream.
func newPooledTestServer(t *testing.T, apiKeys []string, mocks ...*testutil.MockUpstream) (*httptest.Server, *pool.Pool) {
	t.Helper()
	return newTestServerCfg(t, apiKeys, func(cfg *config.Config) {
		if len(mocks) > 0 {
			cfg.UpstreamBaseURL = mocks[0].URL()
		}
	}, mocks...)
}

func TestPooledModeAPIKeyUsesPool(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-h1", 1, `"choices":[{"index":0,"delta":{"content":"pooled"},"finish_reason":null}]`))
	ts, _ := newPooledTestServer(t, []string{"sk-hybrid"}, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": "Bearer sk-hybrid"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "pooled") {
		t.Errorf("stream missing content: %s", data)
	}
	if len(mock.RecordedChatHeaders) != 1 {
		t.Fatalf("upstream chat calls = %d, want 1", len(mock.RecordedChatHeaders))
	}
	if got := mock.RecordedChatHeaders[0].Get("Authorization"); got != "Bearer tok-0" {
		t.Errorf("upstream Authorization = %q, want %q (API key must route to the pool, not relay)", got, "Bearer tok-0")
	}
}

// TestHybridModeClientTokenRelayed: in hybrid mode a credential that does
// NOT match any API_KEYS entry is routed to the BRIDGE path — the client's
// token is relayed upstream verbatim.

func TestPooledModeMissingCredentialRejected(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newPooledTestServer(t, []string{"sk-hybrid"}, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "invalid_api_key") {
		t.Errorf("body missing invalid_api_key: %s", data)
	}
	if mock.SessionCreates != 0 || len(mock.StartedRuns) != 0 {
		t.Errorf("upstream contact = %d creates / %d runs, want 0/0 (missing credential rejected before pool)",
			mock.SessionCreates, len(mock.StartedRuns))
	}
}

// TestHybridModeOpenPooledWithoutAPIKeys: with no API_KEYS configured the
// hybrid instance keeps the historic open-pooled behavior — a request with
// no credential is served by the pool (401).

func TestPooledModeOpenWithoutAPIKeys(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-h3", 3, `"choices":[{"index":0,"delta":{"content":"open"},"finish_reason":null}]`))
	ts, _ := newPooledTestServer(t, nil, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (open pooled): %s", resp.StatusCode, data)
	}
	if len(mock.RecordedChatHeaders) != 1 {
		t.Fatalf("upstream chat calls = %d, want 1", len(mock.RecordedChatHeaders))
	}
	if got := mock.RecordedChatHeaders[0].Get("Authorization"); got != "Bearer tok-0" {
		t.Errorf("upstream Authorization = %q, want %q", got, "Bearer tok-0")
	}
}

// TestPooledRejectsUnknownCredential:
// pooled, the pre-hybrid behavior), a credential that does not match an API
// key is rejected 401 by the middleware.
// token.

func TestPooledRejectsUnknownCredential(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-h4", 4, `"choices":[{"index":0,"delta":{"content":"pooled"},"finish_reason":null}]`))
	ts, _ := newTestServerCfg(t, []string{"sk-lock"}, nil, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	// Non-matching credential → 401.
	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": "Bearer stray-token"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stray credential status = %d, want 401: %s", resp.StatusCode, data)
	}
	if mock.SessionCreates != 0 || len(mock.StartedRuns) != 0 {
		t.Errorf("upstream contact = %d creates / %d runs, want 0/0", mock.SessionCreates, len(mock.StartedRuns))
	}

	// Valid API key still reaches the pool.
	resp, data = doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": "Bearer sk-lock"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("api-key status = %d, want 200: %s", resp.StatusCode, data)
	}
	if len(mock.RecordedChatHeaders) != 1 {
		t.Fatalf("upstream chat calls = %d, want 1", len(mock.RecordedChatHeaders))
	}
	if got := mock.RecordedChatHeaders[0].Get("Authorization"); got != "Bearer tok-0" {
		t.Errorf("upstream Authorization = %q, want %q", got, "Bearer tok-0")
	}
}
