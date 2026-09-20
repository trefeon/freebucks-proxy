package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/pool"
	"freebucks-proxy/backend/internal/testutil"
)

// newHybridTestServer wires the server in hybrid mode: AUTH_TOKENS pool +
// BRIDGE_ENABLED (the default), with the given client API keys. The pool's
// fixed tokens are "tok-0", "tok-1", ... one per mock upstream.
func newHybridTestServer(t *testing.T, apiKeys []string, mocks ...*testutil.MockUpstream) (*httptest.Server, *pool.Pool) {
	t.Helper()
	return newTestServerCfg(t, apiKeys, func(cfg *config.Config) {
		cfg.BridgeEnabled = true
		// Bridge entries build their upstream client from the POOL config
		// (AcquireBridge → bridgeEntryFor → upstream.New(token, p.cfg)),
		// so the pool config must point at the mock for bridge-path tests.
		if len(mocks) > 0 {
			cfg.UpstreamBaseURL = mocks[0].URL()
		}
	}, mocks...)
}

// TestHybridModeAPIKeyUsesPool: in hybrid mode a credential matching an
// API_KEYS entry is routed to the POOLED path — the upstream sees the pool
// token, never the client's API key.
func TestHybridModeAPIKeyUsesPool(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-h1", 1, `"choices":[{"index":0,"delta":{"content":"pooled"},"finish_reason":null}]`))
	ts, _ := newHybridTestServer(t, []string{"sk-hybrid"}, mock)
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
func TestHybridModeClientTokenRelayed(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-h2", 2, `"choices":[{"index":0,"delta":{"content":"bridged"},"finish_reason":null}]`))
	ts, _ := newHybridTestServer(t, []string{"sk-hybrid"}, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": "Bearer client-tok-hyb"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "bridged") {
		t.Errorf("stream missing content: %s", data)
	}
	if len(mock.RecordedChatHeaders) != 1 {
		t.Fatalf("upstream chat calls = %d, want 1", len(mock.RecordedChatHeaders))
	}
	if got := mock.RecordedChatHeaders[0].Get("Authorization"); got != "Bearer client-tok-hyb" {
		t.Errorf("upstream Authorization = %q, want %q (non-API-key credential relayed as bridge token)", got, "Bearer client-tok-hyb")
	}
}

// TestHybridModeMissingCredentialRejected: in hybrid mode with API_KEYS
// configured, a request with no credential is rejected 401 — exactly like
// pure pooled mode — before any pool or upstream contact.
func TestHybridModeMissingCredentialRejected(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newHybridTestServer(t, []string{"sk-hybrid"}, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "missing_bearer_token") {
		t.Errorf("body missing missing_bearer_token: %s", data)
	}
	if mock.SessionCreates != 0 || len(mock.StartedRuns) != 0 {
		t.Errorf("upstream contact = %d creates / %d runs, want 0/0 (missing credential rejected before pool)",
			mock.SessionCreates, len(mock.StartedRuns))
	}
}

// TestHybridModeOpenPooledWithoutAPIKeys: with no API_KEYS configured the
// hybrid instance keeps the historic open-pooled behavior — a request with
// no credential is served by the pool (not 401, not bridge).
func TestHybridModeOpenPooledWithoutAPIKeys(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-h3", 3, `"choices":[{"index":0,"delta":{"content":"open"},"finish_reason":null}]`))
	ts, _ := newHybridTestServer(t, nil, mock)
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

// TestPooledLockdownRejectsBridgeTokens: with BRIDGE_ENABLED=false (pure
// pooled, the pre-hybrid behavior), a credential that does not match an API
// key is rejected 401 by the middleware — it is never treated as a bridge
// token.
func TestPooledLockdownRejectsBridgeTokens(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-h4", 4, `"choices":[{"index":0,"delta":{"content":"pooled"},"finish_reason":null}]`))
	ts, _ := newTestServerCfg(t, []string{"sk-lock"}, func(cfg *config.Config) {
		cfg.BridgeEnabled = false
	}, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	// Non-matching credential → 401 (no bridge relay in pure pooled).
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

// TestHybridModeAnthropicRoutesByCredential: the hybrid routing rule holds
// on the Anthropic surface — API key → pooled, other credential → bridge.
func TestHybridModeAnthropicRoutesByCredential(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"ok"}}`)
	ts, _ := newHybridTestServer(t, []string{"sk-hyb"}, mock)
	msgURL := ts.URL + "/v1/messages"
	body := `{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`

	// API key → pooled (upstream sees the pool token).
	resp, data := doJSON(t, http.MethodPost, msgURL, []byte(body), map[string]string{"anthropic-api-key": "sk-hyb"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("api-key status = %d, want 200: %s", resp.StatusCode, data)
	}
	if len(mock.RecordedChatHeaders) != 1 {
		t.Fatalf("upstream chat calls after api-key = %d, want 1", len(mock.RecordedChatHeaders))
	}
	if got := mock.RecordedChatHeaders[0].Get("Authorization"); got != "Bearer tok-0" {
		t.Errorf("api-key upstream Authorization = %q, want %q", got, "Bearer tok-0")
	}

	// Client token → bridge (relayed upstream).
	resp, data = doJSON(t, http.MethodPost, msgURL, []byte(body), map[string]string{"anthropic-api-key": "client-tok-an"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client-token status = %d, want 200: %s", resp.StatusCode, data)
	}
	if len(mock.RecordedChatHeaders) != 2 {
		t.Fatalf("upstream chat calls after client-token = %d, want 2", len(mock.RecordedChatHeaders))
	}
	if got := mock.RecordedChatHeaders[1].Get("Authorization"); got != "Bearer client-tok-an" {
		t.Errorf("client-token upstream Authorization = %q, want %q", got, "Bearer client-tok-an")
	}
}

// TestHybridModeHealthzReportsMode pins the healthz "mode" field in hybrid
// mode.
func TestHybridModeHealthzReportsMode(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newHybridTestServer(t, []string{"sk-hyb"}, mock)

	resp, data := doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("healthz not JSON: %v: %s", err, data)
	}
	if out.Mode != "hybrid" {
		t.Errorf("healthz mode = %q, want hybrid", out.Mode)
	}
}

// TestHybridModeDashboardShowsBothSurfaces verifies the /admin/api/tokens
// payload in hybrid mode carries the pooled table AND live bridge cards
// (the two-section dashboard).
func TestHybridModeDashboardShowsBothSurfaces(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-h5", 5, `"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]`))
	ts, _ := newHybridTestServer(t, []string{"sk-hyb"}, mock)
	cookie := loginCookie(t, ts, config.DefaultAdminToken)

	// Create a bridge entry via a client-token request.
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), map[string]string{"Authorization": "Bearer client-tok-dash"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bridge chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	// Tokens API: mode hybrid, show_bridge true, bridge cards present AND
	// pooled rows present — both dashboard sections populated.
	tokensReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/api/tokens", nil)
	tokensReq.Header.Set("Cookie", cookie)
	tokensResp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}).Do(tokensReq)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tokensResp.Body.Close() }()
	var tokens map[string]any
	if err := json.NewDecoder(tokensResp.Body).Decode(&tokens); err != nil {
		t.Fatal(err)
	}

	if mode, ok := tokens["mode"].(string); !ok || mode != "hybrid" {
		t.Errorf("tokens mode = %v, want hybrid", tokens["mode"])
	}
	if sb, ok := tokens["show_bridge"].(bool); !ok || !sb {
		t.Errorf("tokens show_bridge = %v, want true", tokens["show_bridge"])
	}
	if cards, ok := tokens["bridge_token_cards"].([]any); !ok || len(cards) < 1 {
		t.Errorf("tokens bridge_token_cards = %v, want >= 1 entry", tokens["bridge_token_cards"])
	}
	if toks, ok := tokens["tokens"].([]any); !ok || len(toks) < 1 {
		t.Errorf("tokens (pooled) = %v, want >= 1 row in hybrid", tokens["tokens"])
	}

	// Overview carries the same dual-surface view.
	ovReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/api/overview", nil)
	ovReq.Header.Set("Cookie", cookie)
	ovResp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}).Do(ovReq)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ovResp.Body.Close() }()
	var overview map[string]any
	if err := json.NewDecoder(ovResp.Body).Decode(&overview); err != nil {
		t.Fatal(err)
	}
	if mode, ok := overview["mode"].(string); !ok || mode != "hybrid" {
		t.Errorf("overview mode = %v, want hybrid", overview["mode"])
	}
	if cards, ok := overview["bridge_token_cards"].([]any); !ok || len(cards) < 1 {
		t.Errorf("overview bridge_token_cards = %v, want >= 1 entry", overview["bridge_token_cards"])
	}
}

// TestHybridModeDashboardModeSwitch verifies the pooled<->hybrid mode
// transitions: from hybrid, "pooled" disables the bridge relay
// (BRIDGE_ENABLED=0 persisted to .env); from pure pooled, "hybrid"
// re-enables it (BRIDGE_ENABLED=1).
func TestHybridModeDashboardModeSwitch(t *testing.T) {
	t.Chdir(t.TempDir())
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServerCfg(t, []string{"sk-hyb"}, func(c *config.Config) {
		c.AdminToken = config.DefaultAdminToken
		c.BridgeEnabled = true
	}, mock)
	cookie := loginCookie(t, ts, config.DefaultAdminToken)

	// hybrid -> pooled: bridge relay disabled.
	resp := postJSON(t, ts.URL, cookie, "/admin/mode", `{"mode":"pooled"}`)
	pooledBody := bodyOf(t, resp)
	if !strings.Contains(pooledBody, "Switched to pooled mode") {
		t.Errorf("hybrid->pooled response = %q", pooledBody)
	}
	env, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "BRIDGE_ENABLED=0") {
		t.Errorf(".env after hybrid->pooled = %q, want BRIDGE_ENABLED=0", env)
	}

	// pooled -> hybrid: bridge relay re-enabled.
	resp = postJSON(t, ts.URL, cookie, "/admin/mode", `{"mode":"hybrid"}`)
	hybridBody := bodyOf(t, resp)
	if !strings.Contains(hybridBody, "Switched to hybrid mode") {
		t.Errorf("pooled->hybrid response = %q", hybridBody)
	}
	env, err = os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "BRIDGE_ENABLED=1") {
		t.Errorf(".env after pooled->hybrid = %q, want BRIDGE_ENABLED=1", env)
	}
}
