package server_test

import (
	"encoding/json"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/server"
	"freebuff-proxy/backend/internal/testutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- bridge mode ---

// newBridgeTestServer wires the server in bridge mode (no AUTH_TOKENS): the
// pool has no fixed tokens and lazily-created per-client-token clients talk
// to the given mock upstream.
func newBridgeTestServer(t *testing.T, mock *testutil.MockUpstream) (*httptest.Server, *pool.Pool) {
	t.Helper()
	cfg := &config.Config{
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		DashboardEnabled:   true,
		AdminToken:         "123456",
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, p, reg, nil, nil, "")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, p
}

func TestBridgeModeRequiresClientToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newBridgeTestServer(t, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	// No Authorization header: 401 missing_bearer_token, nothing upstream.
	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "missing_bearer_token") {
		t.Errorf("body missing missing_bearer_token: %s", data)
	}
	if mock.SessionCreates != 0 || len(mock.StartedRuns) != 0 {
		t.Errorf("upstream contact = %d creates / %d runs, want 0/0 (missing token rejected before pool)",
			mock.SessionCreates, len(mock.StartedRuns))
	}

	// An empty Bearer value is also rejected.
	resp, data = doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": "Bearer  "})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("empty bearer status = %d, want 401: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "missing_bearer_token") {
		t.Errorf("empty bearer body missing missing_bearer_token: %s", data)
	}
}

func TestBridgeModeRelaysClientToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-b1", 1, `"choices":[{"index":0,"delta":{"content":"bridged"},"finish_reason":null}]`))
	ts, _ := newBridgeTestServer(t, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": "Bearer client-tok-abc"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "bridged") {
		t.Errorf("stream missing content: %s", data)
	}

	// The upstream saw the CLIENT's token, not a proxy-configured one.
	if len(mock.RecordedChatHeaders) != 1 {
		t.Fatalf("upstream chat calls = %d, want 1", len(mock.RecordedChatHeaders))
	}
	if got := mock.RecordedChatHeaders[0].Get("Authorization"); got != "Bearer client-tok-abc" {
		t.Errorf("upstream Authorization = %q, want %q", got, "Bearer client-tok-abc")
	}

	// A second request with the same token reuses the entry: no new run.
	resp2, data2 := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": "Bearer client-tok-abc"})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second request status = %d, want 200: %s", resp2.StatusCode, data2)
	}
	if got := len(mock.StartedRuns); got != 1 {
		t.Errorf("started runs = %d, want 1 (entry reused across requests)", got)
	}
}

func TestBridgeModeXAPIKey(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-b2", 2, `"choices":[{"index":0,"delta":{"content":"keyed"},"finish_reason":null}]`))
	ts, _ := newBridgeTestServer(t, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), map[string]string{"x-api-key": "client-tok-xyz"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "keyed") {
		t.Errorf("stream missing content: %s", data)
	}
	if got := mock.RecordedChatHeaders[0].Get("Authorization"); got != "Bearer client-tok-xyz" {
		t.Errorf("upstream Authorization = %q, want %q (x-api-key relayed as bearer)", got, "Bearer client-tok-xyz")
	}
}

func TestBridgeModeModelsAndHealthz(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newBridgeTestServer(t, mock)

	// /v1/models and /healthz need no header in bridge mode.
	resp, data := doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200: %s", resp.StatusCode, data)
	}
	resp, data = doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		BridgeTokens int `json:"bridge_tokens"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("healthz is not JSON: %v: %s", err, data)
	}
	if out.BridgeTokens != 0 {
		t.Errorf("bridge_tokens = %d, want 0 (no chat requests yet)", out.BridgeTokens)
	}

	// After a bridged chat the healthz counter reflects the cached entry.
	doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), map[string]string{"Authorization": "Bearer client-tok-h"})
	resp, data = doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("healthz is not JSON: %v: %s", err, data)
	}
	if out.BridgeTokens != 1 {
		t.Errorf("bridge_tokens = %d, want 1 after a chat", out.BridgeTokens)
	}
}

// TestBridgeModeHealthzReportsMode pins the healthz "mode" field in pure
// bridge mode.
func TestBridgeModeHealthzReportsMode(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newBridgeTestServer(t, mock)
	resp, data := doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("healthz is not JSON: %v: %s", err, data)
	}
	if out.Mode != "bridge" {
		t.Errorf("mode = %q, want bridge", out.Mode)
	}
}

// RequestsServed counts successful upstream chats in bridge mode too (the
// metrics page reads it, so it must not stay 0 when no AUTH_TOKENS exist).
func TestBridgeRequestsServedCounter(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-b2", 1, `"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]`))
	ts, p := newBridgeTestServer(t, mock)
	chatURL := ts.URL + "/v1/chat/completions"
	for range 3 {
		resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": "Bearer client-tok"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d: %s", resp.StatusCode, data)
		}
	}
	if got := p.PoolSnapshot().RequestsServed; got != 3 {
		t.Fatalf("RequestsServed = %d, want 3 (bridge chats must count)", got)
	}
}

// TestBridgeTokenLockUnlockAdmin drives the admin lock/unlock lifecycle for
// bridge tokens via the dashboard API (#187): lock prevents AcquireBridge
// and unlock restores it.
func TestBridgeTokenLockUnlockAdmin(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, p := newBridgeTestServer(t, mock)

	// Login to get a session cookie for dashboard endpoints.
	// The default AdminToken is "123456" (config.DefaultAdminToken).
	cookie := loginCookie(t, ts, "123456")

	// First, create a bridge entry by sending a chat request.
	resp, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions",
		chatBody(modelA), map[string]string{"Authorization": "Bearer bridge-lock-test-token"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pre-lock bridge chat status = %d, want 200", resp.StatusCode)
	}

	// Verify we have a bridge entry.
	if count := p.BridgeCount(); count != 1 {
		t.Fatalf("BridgeCount = %d after chat, want 1", count)
	}

	// Get the key hash for the bridge entry.
	snaps := p.BridgeSnapshot()
	if len(snaps) != 1 {
		t.Fatalf("BridgeSnapshot len = %d, want 1", len(snaps))
	}
	key := snaps[0].Key

	// Lock the bridge token.
	lockResp, lockBody := doDashboardPost(t, ts.URL+"/admin/bridge-tokens/"+key+"/lock", cookie)
	if lockResp.StatusCode != http.StatusOK {
		t.Fatalf("lock status = %d, want 200: %s", lockResp.StatusCode, lockBody)
	}
	if !strings.Contains(lockBody, "locked") {
		t.Errorf("lock response missing 'locked': %s", lockBody)
	}

	// Verify snapshot reports locked.
	snaps = p.BridgeSnapshot()
	if len(snaps) != 1 || !snaps[0].Locked {
		t.Fatal("BridgeSnapshot().Locked = false after LockBridgeEntry")
	}

	// Bridge chat should fail: the entry is locked.
	resp, _ = doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions",
		chatBody(modelA), map[string]string{"Authorization": "Bearer bridge-lock-test-token"})
	if resp.StatusCode == http.StatusOK {
		t.Fatal("post-lock bridge chat status = 200, want non-200")
	}

	// Unlock.
	unlockResp, unlockBody := doDashboardPost(t, ts.URL+"/admin/bridge-tokens/"+key+"/unlock", cookie)
	if unlockResp.StatusCode != http.StatusOK {
		t.Fatalf("unlock status = %d, want 200: %s", unlockResp.StatusCode, unlockBody)
	}
	if !strings.Contains(unlockBody, "unlocked") {
		t.Errorf("unlock response missing 'unlocked': %s", unlockBody)
	}

	// Verify snapshot reports unlocked.
	snaps = p.BridgeSnapshot()
	if len(snaps) != 1 || snaps[0].Locked {
		t.Fatal("BridgeSnapshot().Locked = true after UnlockBridgeEntry")
	}

	// Bridge chat should succeed again.
	resp, _ = doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions",
		chatBody(modelA), map[string]string{"Authorization": "Bearer bridge-lock-test-token"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-unlock bridge chat status = %d, want 200", resp.StatusCode)
	}
}

// TestBridgeTokenLockNotFound verifies that locking/unlocking a nonexistent
// bridge key returns an error (#187).
func TestBridgeTokenLockNotFound(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newBridgeTestServer(t, mock)
	cookie := loginCookie(t, ts, "123456")

	fakeKey := "00000000000000000000000000000000" // 32 hex chars
	lockResp, lockBody := doDashboardPost(t, ts.URL+"/admin/bridge-tokens/"+fakeKey+"/lock", cookie)
	if lockResp.StatusCode == http.StatusOK {
		t.Fatalf("lock nonexistent key returned 200: %s", lockBody)
	}

	unlockResp, unlockBody := doDashboardPost(t, ts.URL+"/admin/bridge-tokens/"+fakeKey+"/unlock", cookie)
	if unlockResp.StatusCode == http.StatusOK {
		t.Fatalf("unlock nonexistent key returned 200: %s", unlockBody)
	}
}

// TestBridgeTokenDashboardOverview verifies that bridge tokens appear in
// the /admin/api/overview response when in bridge mode (#187).
func TestBridgeTokenDashboardOverview(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newBridgeTestServer(t, mock)
	cookie := loginCookie(t, ts, "123456")

	// Create a bridge entry.
	resp, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions",
		chatBody(modelA), map[string]string{"Authorization": "Bearer bridge-dash-token"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bridge chat status = %d, want 200", resp.StatusCode)
	}

	// Fetch overview (GET endpoint).
	overviewReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/api/overview", nil)
	overviewReq.Header.Set("Cookie", cookie)
	overviewResp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}).Do(overviewReq)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = overviewResp.Body.Close() }()
	var overview map[string]any
	if err := json.NewDecoder(overviewResp.Body).Decode(&overview); err != nil {
		t.Fatal(err)
	}

	if mode, ok := overview["mode"].(string); !ok || mode != "bridge" {
		t.Errorf("overview mode = %v, want bridge", overview["mode"])
	}
	if count, ok := overview["bridge_tokens"].(float64); !ok || count < 1 {
		t.Errorf("overview bridge_tokens = %v, want >= 1", overview["bridge_tokens"])
	}
	if cards, ok := overview["bridge_token_cards"].([]any); !ok || len(cards) < 1 {
		t.Errorf("overview bridge_token_cards = %v, want >= 1 entry", overview["bridge_token_cards"])
	}
}

// TestBridgeTokenDashboardTokens verifies that bridge tokens appear in
// the /admin/api/tokens response when in bridge mode (#187).
func TestBridgeTokenDashboardTokens(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newBridgeTestServer(t, mock)
	cookie := loginCookie(t, ts, "123456")

	// Create a bridge entry.
	resp, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions",
		chatBody(modelA), map[string]string{"Authorization": "Bearer bridge-tokens-api"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bridge chat status = %d, want 200", resp.StatusCode)
	}

	// Fetch tokens (GET endpoint).
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

	if mode, ok := tokens["mode"].(string); !ok || mode != "bridge" {
		t.Errorf("tokens mode = %v, want bridge", tokens["mode"])
	}
	if cards, ok := tokens["bridge_token_cards"].([]any); !ok || len(cards) < 1 {
		t.Errorf("tokens bridge_token_cards = %v, want >= 1 entry", tokens["bridge_token_cards"])
	}
}

// TestBridgeTokenHealthz verifies that bridge entries appear in the
// /healthz response (#187).
func TestBridgeTokenHealthz(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newBridgeTestServer(t, mock)

	// Create a bridge entry.
	resp, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions",
		chatBody(modelA), map[string]string{"Authorization": "Bearer bridge-healthz-token"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bridge chat status = %d, want 200", resp.StatusCode)
	}

	// Fetch healthz.
	healthResp, healthBody := doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if healthResp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", healthResp.StatusCode)
	}
	var health map[string]any
	if err := json.Unmarshal(healthBody, &health); err != nil {
		t.Fatal(err)
	}

	// Check bridge_tokens count.
	if count, ok := health["bridge_tokens"].(float64); !ok || count < 1 {
		t.Errorf("healthz bridge_tokens = %v, want >= 1", health["bridge_tokens"])
	}

	// Check bridge_entries array.
	entries, ok := health["bridge_entries"].([]any)
	if !ok || len(entries) < 1 {
		t.Fatalf("healthz bridge_entries = %v, want >= 1 entry", health["bridge_entries"])
	}
	entry, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatalf("healthz bridge_entries[0] = %T, want map", entries[0])
	}
	if locked, ok := entry["locked"].(bool); !ok {
		t.Errorf("healthz bridge_entries[0].locked missing or wrong type")
	} else if locked {
		t.Errorf("healthz bridge_entries[0].locked = true, want false")
	}
}
