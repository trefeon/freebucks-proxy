// CLI-spoof wire tests for chat handles, wallet consent, and the environment
// descriptor (vendor freebuff-catalog-agent.ts + codebuff-client.ts
// requestHeaders hook + freebuff-session-api.ts wallet header +
// client-environment.ts). Mock-upstream only; never live traffic.
package upstream

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// seedHeldCatalog installs a held catalog for the client's account without
// HTTP: the fetch schedule points an hour out so no refetch fires mid-test.
func seedHeldCatalog(t *testing.T, client *Client, rows []modelCatalogRow, fetchID string) {
	t.Helper()
	key := catalogAccountKey(client.token, client.baseURL)
	catalogMu.Lock()
	defer catalogMu.Unlock()
	catalogEntries[key] = &catalogEntry{
		catalog: &modelCatalog{
			Rows:    rows,
			FetchID: fetchID,
		},
		fetched:     true,
		nextFetchAt: time.Now().Add(time.Hour),
	}
}

// Chat in catalog mode runs as the row's handle with the fetch id and a
// device signature over the exact sent bytes; the signature verifies with
// the registered public key.
func TestChatHandleAndCompletionHeaders(t *testing.T) {
	fake := &spoofFake{
		catalogSeq: []string{catalogTestBody("deepseek/deepseek-v4-flash", "fbm1.chat-1", "fetch-1")},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Cleanup(resetModelCatalogsForTest)
	t.Cleanup(resetDevicesForTest)

	client, err := New("tok-chat-a", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := client.CreateSessionForModelWithClaim(ctx, "deepseek/deepseek-v4-flash", "cli:123e4567-e89b-42d3-a456-426614174000"); err != nil {
		t.Fatal(err)
	}
	rc, err := client.ChatCompletions(ctx, ChatOptions{Model: "deepseek/deepseek-v4-flash", RunID: "run-1"},
		[]byte(`{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if len(fake.chatHeaders) != 1 || len(fake.chatBodies) != 1 {
		t.Fatalf("chat requests = %d/%d, want 1/1", len(fake.chatHeaders), len(fake.chatBodies))
	}
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(fake.chatBodies[0]), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Model != "fbm1.chat-1" {
		t.Errorf("chat model = %q, want the catalog handle fbm1.chat-1", sent.Model)
	}
	h := fake.chatHeaders[0]
	if got := h.Get(CatalogFetchHeader); got != "fetch-1" {
		t.Errorf("%s = %q, want fetch-1", CatalogFetchHeader, got)
	}
	// The signature covers the exact bytes the fake received.
	var reg struct {
		PublicKey string `json:"publicKey"`
	}
	if err := json.Unmarshal([]byte(fake.deviceKeysBodies[0]), &reg); err != nil {
		t.Fatal(err)
	}
	pubRaw, err := base64.RawURLEncoding.DecodeString(reg.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	verifyDeviceHeaders(t, h, ed25519.PublicKey(pubRaw),
		http.MethodPost, "/api/v1/chat/completions", fake.chatBodies[0], "dk-1")
	// #106 still holds: no model/instance headers on the chat POST.
	for _, name := range []string{"x-freebuff-model", "x-freebuff-instance-id"} {
		if got := h.Get(name); got != "" {
			t.Errorf("%s = %q on the chat POST, want absent (#106)", name, got)
		}
	}
}

// Fallback chat (no catalog held) sends the raw id with no completion
// headers — today's exact shape.
func TestChatFallbackRawModel(t *testing.T) {
	fake := &spoofFake{} // no catalog → fallback
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Cleanup(resetModelCatalogsForTest)
	t.Cleanup(resetDevicesForTest)

	client, err := New("tok-chat-b", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"},
		[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(fake.chatBodies[0]), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Model != "m" {
		t.Errorf("chat model = %q, want the raw id passthrough", sent.Model)
	}
	for _, name := range []string{CatalogFetchHeader, DeviceKeyHeader, DeviceTimestampHeader, DeviceSignatureHeader} {
		if got := fake.chatHeaders[0].Get(name); got != "" {
			t.Errorf("%s = %q, want absent in fallback mode", name, got)
		}
	}
	if got := fake.deviceHits(); got != 0 {
		t.Errorf("device-keys hits = %d, want 0 (fallback never registers)", got)
	}
}

// A model the held catalog does not name goes out as the raw id (the server
// answers stale if it must), while the fetch + signature still ride the
// catalog context.
func TestChatUnknownModelRawIDWithCatalogHeaders(t *testing.T) {
	fake := &spoofFake{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Cleanup(resetModelCatalogsForTest)
	t.Cleanup(resetDevicesForTest)

	client, err := New("tok-chat-c", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	seedHeldCatalog(t, client,
		[]modelCatalogRow{{Key: "other/row", Handle: "fbm1.other"}},
		"fetch-9")
	seedDeviceForTest("tok-chat-c", srv.URL, ed25519.NewKeyFromSeed(fixedDeviceSeed()), "dk-9")
	rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "unknown/model", RunID: "r"},
		[]byte(`{"model":"unknown/model","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(fake.chatBodies[0]), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Model != "unknown/model" {
		t.Errorf("chat model = %q, want the raw id (no row names it)", sent.Model)
	}
	h := fake.chatHeaders[0]
	if got := h.Get(CatalogFetchHeader); got != "fetch-9" {
		t.Errorf("%s = %q, want fetch-9 (catalog held)", CatalogFetchHeader, got)
	}
	priv := ed25519.NewKeyFromSeed(fixedDeviceSeed())
	verifyDeviceHeaders(t, h, priv.Public().(ed25519.PublicKey),
		http.MethodPost, "/api/v1/chat/completions", fake.chatBodies[0], "dk-9")
}

// The admission spend cap expresses the operator-set standing consent:
// headless default "0", a number, or "session". Anything else normalizes to
// "0" — spend is never widened by a typo.
func TestWalletSpendLimitExpression(t *testing.T) {
	fake := &spoofFake{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Cleanup(resetModelCatalogsForTest)
	t.Cleanup(resetDevicesForTest)

	for _, tc := range []struct {
		name string
		set  string
		want string
	}{
		{"default zero", "", "0"},
		{"numeric cap", "5", "5"},
		{"session consent", "session", "session"},
		{"leading zeros canonicalize", "007", "7"},
		{"garbage falls back", "bogus", "0"},
		{"negative falls back", "-3", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := New("tok-wallet-"+tc.name, testConfig(srv.URL, nil))
			if err != nil {
				t.Fatal(err)
			}
			if tc.set != "" {
				client.SetWalletSpendLimit(tc.set)
			}
			if _, err := client.CreateSessionForModel(context.Background(), "deepseek/deepseek-v4-flash"); err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			got := fake.admitHeaders[len(fake.admitHeaders)-1].Get(WalletSpendLimitHeader)
			fake.mu.Unlock()
			if got != tc.want {
				t.Errorf("%s = %q, want %q", WalletSpendLimitHeader, got, tc.want)
			}
		})
	}
}

// The descriptor is the static headless shape: bucketed counts, fixed
// bucket names, no free text (no raw env values, paths, or names).
func TestClientEnvDescriptorShape(t *testing.T) {
	t.Cleanup(resetClientEnvForTest)
	resetClientEnvForTest()
	for _, name := range []string{"CI", "GITHUB_ACTIONS", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(name, "")
	}
	want := "v1;in=0;out=0;tp=none;term=0;ct=0;sz=0x0;ci=0;ssh=0;l=0;p=unknown;g=unknown;osc=na;tzo=0;px=none;tls=1;ca=0"
	if got := clientEnvDescriptor(); got != want {
		t.Errorf("descriptor = %q, want %q", got, want)
	}
}

// The proxy bucket never leaks the URL: loopback vs remote only.
func TestProxyEgressBucket(t *testing.T) {
	for raw, want := range map[string]string{
		"http://127.0.0.1:8080":              "loopback",
		"http://localhost:9":                 "loopback",
		"http://[::1]:8080":                  "loopback",
		"127.0.0.1:8080":                     "loopback",
		"http://proxy.corp:8080":             "remote",
		"socks5://user:pass@proxy.corp:1080": "remote",
		"garbage":                            "remote",
	} {
		if got := bucketProxyURL(raw); got != want {
			t.Errorf("bucketProxyURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

// Session calls (admission POST, poll GET) and ad legs (auction, first-party
// and legacy impressions) all carry the descriptor.
func TestSessionAndAdsCarryEnv(t *testing.T) {
	fake := &spoofFake{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Cleanup(resetModelCatalogsForTest)
	t.Cleanup(resetDevicesForTest)
	t.Cleanup(resetClientEnvForTest)

	client, err := New("tok-env-a", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := client.CreateSessionForModel(ctx, "deepseek/deepseek-v4-flash"); err != nil {
		t.Fatal(err)
	}
	// The probe answers status none: the idle-with-balance meter arrives
	// alongside ErrNoActiveSession.
	if _, err := client.ProbeAccount(ctx); err != nil && !errors.Is(err, ErrNoActiveSession) {
		t.Fatal(err)
	}
	if _, err := client.requestAdsForSurface(ctx, "gravity", "cli_chat", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.postFirstPartyImpression(ctx, "https://gravity.example/imp/1"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.postAdEvent(ctx, "/api/v1/ads/impression", map[string]any{"impUrl": "https://gravity.example/imp/1"}); err != nil {
		t.Fatal(err)
	}
	want := clientEnvDescriptor()
	check := func(name, got string) {
		t.Helper()
		if got != want {
			t.Errorf("%s %s = %q, want the descriptor %q", name, FreebuffEnvHeader, got, want)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.admitHeaders) != 1 {
		t.Fatalf("admissions = %d, want 1", len(fake.admitHeaders))
	}
	check("admission", fake.admitHeaders[0].Get(FreebuffEnvHeader))
	if len(fake.sessionHeaders) != 1 {
		t.Fatalf("session GETs = %d, want 1 (probe)", len(fake.sessionHeaders))
	}
	check("probe", fake.sessionHeaders[0].Get(FreebuffEnvHeader))
	if strings.Count(want, " ") > 0 || strings.Count(want, "/") > 0 {
		t.Errorf("descriptor %q carries free text", want)
	}
}
