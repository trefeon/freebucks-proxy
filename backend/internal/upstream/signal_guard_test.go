package upstream

import (
	"context"
	"encoding/json"
	"freebucks-proxy/backend/internal/config"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordedReq captures one outbound request's method, path, body and headers
// so the signal-guard tests can assert the client's exact wire headers. The
// guard pins the no-accidental-proxy-signal header policy: the vendor's own
// CLI is the blessed convergence, and the proxy must reproduce its header set
// exactly rather than inventing proxy-identifying headers of its own.
type recordedReq struct {
	method string
	path   string
	body   string
	header http.Header
}

// recordingUpstream is a scriptable codebuff.com stand-in that records every
// request it serves (headers + body) alongside the testutil mock, but is
// self-contained so the guard tests never route through shared mock fields a
// sibling slice may be editing. It answers each route with the minimal valid
// wire response the client needs.
type recordingUpstream struct {
	srv *httptest.Server
	mu  sync.Mutex
	req []recordedReq
}

func newRecordingUpstream() *recordingUpstream {
	u := &recordingUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(u.handle))
	return u
}

func (u *recordingUpstream) URL() string { return u.srv.URL }
func (u *recordingUpstream) Close()      { u.srv.Close() }

func (u *recordingUpstream) handle(w http.ResponseWriter, r *http.Request) {
	rawBody, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	u.mu.Lock()
	u.req = append(u.req, recordedReq{
		method: r.Method,
		path:   r.URL.Path,
		body:   string(rawBody),
		header: r.Header.Clone(),
	})
	u.mu.Unlock()

	switch {
	case r.URL.Path == "/api/v1/chat/completions" && r.Method == http.MethodPost:
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `data: {"id":"x","object":"chat.completion.chunk","choices":[]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	case r.URL.Path == "/api/v1/freebuff/session/admission" && r.Method == http.MethodPost:
		writeBodyJSON(w, 200, `{"status":"active","instanceId":"inst-1","expiresAt":"2030-01-01T00:00:00Z"}`)
	case r.URL.Path == "/api/v1/freebuff/session" && r.Method == http.MethodGet:
		writeBodyJSON(w, 200, `{"status":"active","instanceId":"inst-1","expiresAt":"2030-01-01T00:00:00Z"}`)
	case r.URL.Path == "/api/v1/freebuff/session" && r.Method == http.MethodDelete:
		writeBodyJSON(w, 200, `{"status":"ended"}`)
	case r.URL.Path == "/api/v1/ads" && r.Method == http.MethodPost:
		writeBodyJSON(w, 200, `{"ads":[{"impUrl":"https://gravity.example/imp/1"}],"provider":"gravity"}`)
	case (r.URL.Path == "/api/v1/ads/impression" || r.URL.Path == "/api/v1/ads/click") && r.Method == http.MethodPost:
		writeBodyJSON(w, 200, `{"ok":true}`)
	case r.URL.Path == "/api/v1/agent-runs" && r.Method == http.MethodPost:
		var payload struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(rawBody, &payload)
		if payload.Action == "START" {
			writeBodyJSON(w, 200, `{"runId":"run-0001"}`)
		} else {
			writeBodyJSON(w, 200, `{"ok":true}`)
		}
	default:
		writeBodyJSON(w, 404, `{"error":"not found"}`)
	}
}

// snapshot returns a locked copy of the recorded requests.
func (u *recordingUpstream) snapshot() []recordedReq {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]recordedReq(nil), u.req...)
}

// writeBodyJSON writes a verbatim JSON body with a Content-Type header.
func writeBodyJSON(w http.ResponseWriter, status int, raw string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, raw)
}

// TestSignalGuardNoProxySignalHeaders pins that NO request the client builds
// for any path carries cf-worker or cf-ray (proxy-identifying headers that
// would betray the gateway as a reverse proxy). Every request-construction
// path is exercised end-to-end through the real client methods.
func TestSignalGuardNoProxySignalHeaders(t *testing.T) {
	srv := newRecordingUpstream()
	defer srv.Close()
	client, err := New("tok-a", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := client.CreateSession(ctx); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := client.CreateSessionForModel(ctx, "deepseek/deepseek-v4-flash"); err != nil {
		t.Fatalf("CreateSessionForModel: %v", err)
	}
	if _, err := client.GetSession(ctx, "inst-1"); err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if _, err := client.ProbeAccount(ctx); err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}
	if _, err := client.EndSession(ctx, "inst-1"); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if _, err := client.StartRun(ctx, "agent-1"); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := client.FinishRun(ctx, "run-0001", "completed", 1, nil, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	rc, err := client.ChatCompletions(ctx, ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	_ = rc.Close()

	reqs := srv.snapshot()
	if len(reqs) == 0 {
		t.Fatal("no requests recorded")
	}
	for _, r := range reqs {
		if got := r.header.Get("cf-worker"); got != "" {
			t.Errorf("%s %s carries cf-worker=%q, want absent", r.method, r.path, got)
		}
		if got := r.header.Get("cf-ray"); got != "" {
			t.Errorf("%s %s carries cf-ray=%q, want absent", r.method, r.path, got)
		}
	}
}

// TestSignalGuardChatPostNeverCarriesModel pins that the chat POST carries
// NO x-freebuff-model / x-freebuff-instance-id header (#106): the official
// CLI sends exactly Authorization + the ai-sdk UA (+ optional acting-user-id)
// on chat; the model and instance id ride only in the body metadata.
func TestSignalGuardChatPostNeverCarriesModel(t *testing.T) {
	srv := newRecordingUpstream()
	defer srv.Close()
	client, err := New("tok-a", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := client.ChatCompletions(context.Background(),
		ChatOptions{Model: "deepseek/deepseek-v4-flash", RunID: "r"},
		[]byte(`{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()

	chat := findReq(t, srv, "/api/v1/chat/completions", http.MethodPost)
	if got := chat.header.Get("x-freebuff-model"); got != "" {
		t.Errorf("chat x-freebuff-model = %q, want absent (#106)", got)
	}
	if got := chat.header.Get("x-freebuff-instance-id"); got != "" {
		t.Errorf("chat x-freebuff-instance-id = %q, want absent (#106)", got)
	}
	if got := chat.header.Get("Authorization"); got != "Bearer tok-a" {
		t.Errorf("chat Authorization = %q, want Bearer tok-a", got)
	}
}

// TestSignalGuardSessionPostsModelHeader pins that the session POST carries
// x-freebuff-model exactly when a model is set and is absent when none is.
func TestSignalGuardSessionPostsModelHeader(t *testing.T) {
	srv := newRecordingUpstream()
	defer srv.Close()
	client, err := New("tok-a", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	const model = "deepseek/deepseek-v4-flash"
	if _, err := client.CreateSessionForModel(ctx, model); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateSession(ctx); err != nil {
		t.Fatal(err)
	}

	reqs := srv.snapshot()
	var withModel, withoutModel *recordedReq
	for i := range reqs {
		if reqs[i].path == "/api/v1/freebuff/session/admission" && reqs[i].method == http.MethodPost {
			if reqs[i].header.Get("x-freebuff-model") != "" {
				withModel = &reqs[i]
			} else {
				withoutModel = &reqs[i]
			}
		}
	}
	if withModel == nil {
		t.Fatal("no session POST carrying x-freebuff-model recorded")
		return
	}
	if got := withModel.header.Get("x-freebuff-model"); got != model {
		t.Errorf("session x-freebuff-model = %q, want %q", got, model)
	}
	if withoutModel == nil {
		t.Fatal("no session POST without x-freebuff-model recorded")
		return
	}
	if got := withoutModel.header.Get("x-freebuff-model"); got != "" {
		t.Errorf("CreateSession x-freebuff-model = %q, want absent", got)
	}
}

// TestSignalGuardClientIDShape pins every generated client id to the
// SDK-faithful 13-char base36 shape (^[a-z0-9]{13}$) and that no sampled id
// echoes a proxy-looking prefix (sess:/run:/wf-). The 13-char base36 alphabet
// excludes ':' and '-', so those prefixes cannot appear, but the guard
// documents the no-proxy-client-id policy the vendor gates on.
func TestSignalGuardClientIDShape(t *testing.T) {
	re := regexp.MustCompile(`^[a-z0-9]{13}$`)
	for range 200 {
		id := NewClientID()
		if !re.MatchString(id) {
			t.Errorf("NewClientID() = %q, want ^[a-z0-9]{13}$", id)
		}
		for _, prefix := range []string{"sess:", "run:", "wf-"} {
			if strings.HasPrefix(id, prefix) {
				t.Errorf("NewClientID() = %q, want no %q prefix", id, prefix)
			}
		}
	}
}

// TestAgentRunsBearerOnly pins that agent-runs START/FINISH carry Bearer
// plus the optional acting-user header and NEVER x-codebuff-api-key (CLI
// parity: sdk/src/impl/database.ts startAgentRun/finishAgentRun send
// Authorization + optional FREEBUFF_ACTING_USER_HEADER only; the dual-auth
// pair lives solely on the agent-runtime web-search/docs/gravity/token-count
// POSTs). The acting-user header rides only when the client's own account
// id is configured.
func TestAgentRunsBearerOnly(t *testing.T) {
	t.Run("no acting user configured", func(t *testing.T) {
		srv := newRecordingUpstream()
		defer srv.Close()
		client, err := New("tok-secret-abc", testConfig(srv.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.StartRun(context.Background(), "agent-1"); err != nil {
			t.Fatal(err)
		}
		if err := client.FinishRun(context.Background(), "run-0001", "completed", 1, nil, ""); err != nil {
			t.Fatal(err)
		}
		saw := false
		for _, r := range srv.snapshot() {
			if r.path != "/api/v1/agent-runs" || r.method != http.MethodPost {
				continue
			}
			saw = true
			if got := r.header.Get("Authorization"); got != "Bearer tok-secret-abc" {
				t.Errorf("%s Authorization = %q, want Bearer tok-secret-abc", r.path, got)
			}
			if got := r.header.Get("x-codebuff-api-key"); got != "" {
				t.Errorf("%s x-codebuff-api-key = %q, want absent (Bearer-only, like the CLI)", r.path, got)
			}
			if got := r.header.Get("x-freebuff-acting-user-id"); got != "" {
				t.Errorf("%s x-freebuff-acting-user-id = %q, want absent when unconfigured", r.path, got)
			}
		}
		if !saw {
			t.Fatal("no agent-runs POST recorded")
		}
	})

	t.Run("acting user configured", func(t *testing.T) {
		srv := newRecordingUpstream()
		defer srv.Close()
		client, err := New("tok-a", testConfig(srv.URL(), func(c *config.Config) { c.ActingUserID = "user-123" }))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.StartRun(context.Background(), "agent-1"); err != nil {
			t.Fatal(err)
		}
		if err := client.FinishRun(context.Background(), "run-0001", "completed", 1, nil, ""); err != nil {
			t.Fatal(err)
		}
		saw := false
		for _, r := range srv.snapshot() {
			if r.path != "/api/v1/agent-runs" || r.method != http.MethodPost {
				continue
			}
			saw = true
			if got := r.header.Get("x-freebuff-acting-user-id"); got != "user-123" {
				t.Errorf("%s x-freebuff-acting-user-id = %q, want user-123 (the token's own id)", r.path, got)
			}
			if got := r.header.Get("x-codebuff-api-key"); got != "" {
				t.Errorf("%s x-codebuff-api-key = %q, want absent even with acting-user set", r.path, got)
			}
		}
		if !saw {
			t.Fatal("no agent-runs POST recorded")
		}
	})
}

// TestAgentRunsNeverForwardsForeignKey verifies no agent-run POST can carry
// a relayed/downstream credential: the client builds its own headers
// (Bearer + optional acting-user) and the cross-host redirect sanitizer
// still scrubs x-codebuff-api-key entirely, so a FOREIGN value is never
// forwarded to a redirect target.
func TestAgentRunsNeverForwardsForeignKey(t *testing.T) {
	const token = "tok-proxy-abc"
	const foreign = "tok-attacker-xyz"

	t.Run("agent_run_posts_carry_no_api_key_header", func(t *testing.T) {
		srv := newRecordingUpstream()
		defer srv.Close()
		client, err := New(token, testConfig(srv.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.StartRun(context.Background(), "agent-1"); err != nil {
			t.Fatal(err)
		}
		if err := client.FinishRun(context.Background(), "run-0001", "completed", 1, nil, ""); err != nil {
			t.Fatal(err)
		}
		for _, r := range srv.snapshot() {
			if r.path != "/api/v1/agent-runs" || r.method != http.MethodPost {
				continue
			}
			if got := r.header.Get("x-codebuff-api-key"); got != "" {
				t.Errorf("%s %s x-codebuff-api-key = %q, want absent (nothing sets it; foreign %q never forwarded)",
					r.method, r.path, got, foreign)
			}
			if got := r.header.Get("Authorization"); got != "Bearer "+token {
				t.Errorf("%s %s Authorization = %q, want Bearer %s", r.method, r.path, got, token)
			}
		}
	})

	t.Run("cross_host_redirect_scrubs_foreign_key", func(t *testing.T) {
		client, err := New(token, testConfig("http://127.0.0.1:1", nil))
		if err != nil {
			t.Fatal(err)
		}
		// Simulate a relayed downstream header carrying a DIFFERENT token
		// value: the sanitizer must drop it on a cross-host redirect.
		via := []*http.Request{{
			URL: mustParseURL(t, "https://www.codebuff.com"),
			Header: http.Header{
				"Authorization":      {"Bearer " + foreign},
				"x-codebuff-api-key": {foreign},
			},
		}}
		redirected := &http.Request{
			URL: mustParseURL(t, "https://attacker.example/leak"),
			Header: http.Header{
				"Authorization":      {"Bearer " + foreign},
				"x-codebuff-api-key": {foreign},
			},
		}
		if err := client.http.CheckRedirect(redirected, via); err != nil {
			t.Fatalf("CheckRedirect: %v", err)
		}
		if got := redirected.Header.Get("x-codebuff-api-key"); got != "" {
			t.Errorf("cross-host redirect carried foreign x-codebuff-api-key %q, want scrubbed", got)
		}
		if got := redirected.Header.Get("Authorization"); got != "" {
			t.Errorf("cross-host redirect carried foreign Authorization %q, want scrubbed", got)
		}
	})
}

// TestSignalGuardChatUserAgent pins the chat POST User-Agent to the exact
// ai-sdk/openai-compatible/1.0.0/codebuff string the official CLI pins on
// model calls (chat is the ONLY path carrying it).
func TestSignalGuardChatUserAgent(t *testing.T) {
	srv := newRecordingUpstream()
	defer srv.Close()
	client, err := New("tok-a", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := client.ChatCompletions(context.Background(),
		ChatOptions{Model: "m", RunID: "r"},
		[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()

	chat := findReq(t, srv, "/api/v1/chat/completions", http.MethodPost)
	if got := chat.header.Get("User-Agent"); got != cliUserAgent {
		t.Errorf("chat User-Agent = %q, want %q", got, cliUserAgent)
	}
}

// findReq returns the first recorded request matching path+method, failing the
// test if none exists.
func findReq(t *testing.T, srv *recordingUpstream, path, method string) *recordedReq {
	t.Helper()
	reqs := srv.snapshot()
	for i := range reqs {
		if reqs[i].path == path && reqs[i].method == method {
			return &reqs[i]
		}
	}
	t.Fatalf("no recorded %s %s request", method, path)
	return nil
}

// TestSessionCallsStampFirstTabDiscount pins that EVERY session call carries
// x-freebuff-first-tab-discount "0": admission POST, poll GET, refund DELETE
// and probe GET (callFreebuffSession stamps it on all three methods; the
// proxy holds no user-confirmed offer state, so it always sends the boring
// "0" exactly like a CLI call with firstTabDiscount unset).
func TestSessionCallsStampFirstTabDiscount(t *testing.T) {
	srv := newRecordingUpstream()
	defer srv.Close()
	client, err := New("tok-a", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := client.CreateSession(ctx); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := client.GetSession(ctx, "inst-1"); err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if _, err := client.EndSession(ctx, "inst-1"); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if _, err := client.ProbeAccount(ctx); err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}
	seen := map[string]bool{}
	for _, r := range srv.snapshot() {
		key := r.method + " " + r.path
		switch key {
		case "POST /api/v1/freebuff/session/admission", "GET /api/v1/freebuff/session", "DELETE /api/v1/freebuff/session":
			seen[key] = true
			if got := r.header.Get("x-freebuff-first-tab-discount"); got != "0" {
				t.Errorf("%s first-tab discount = %q, want \"0\"", key, got)
			}
		}
	}
	for key := range map[string]bool{"POST /api/v1/freebuff/session/admission": true, "GET /api/v1/freebuff/session": true, "DELETE /api/v1/freebuff/session": true} {
		if !seen[key] {
			t.Errorf("no recorded %s call", key)
		}
	}
}

// TestWaitingRoomChainFiresAdLegs pins the free-mode ad loop legs: the
// auction's server-issued impUrl is acked on /api/v1/ads/impression and
// /api/v1/ads/click with the Freebuff-CLI product UA, a uuid
// X-Freebuff-Event-Id echoed in the body as clientEventId, and the
// browser-like body UA + os. Best-effort is covered by
// TestWaitingRoomChainAdFailureNeverFailsAdmission.
func TestWaitingRoomChainFiresAdLegs(t *testing.T) {
	srv := newRecordingUpstream()
	defer srv.Close()
	client, err := New("tok-a", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	client.FireWaitingRoomChain(context.Background())
	fired := map[string]recordedReq{}
	for _, r := range srv.snapshot() {
		if r.method == http.MethodPost && (r.path == "/api/v1/ads/impression" || r.path == "/api/v1/ads/click") {
			fired[r.path] = r
		}
	}
	for _, path := range []string{"/api/v1/ads/impression", "/api/v1/ads/click"} {
		r, ok := fired[path]
		if !ok {
			t.Fatalf("no recorded POST %s", path)
			continue
		}
		if got := r.header.Get("User-Agent"); got != freebuffCliUA {
			t.Errorf("%s User-Agent = %q, want the CLI product UA %q", path, got, freebuffCliUA)
		}
		if got := r.header.Get("Authorization"); got != "Bearer tok-a" {
			t.Errorf("%s Authorization = %q, want Bearer tok-a", path, got)
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(r.body), &body); err != nil {
			t.Fatalf("%s body not JSON: %v", path, err)
		}
		if body["impUrl"] != "https://gravity.example/imp/1" {
			t.Errorf("%s impUrl = %v, want the auction-issued impUrl (never invented)", path, body["impUrl"])
		}
		eventID, _ := body["clientEventId"].(string)
		if eventID == "" {
			t.Errorf("%s missing clientEventId", path)
		} else if got := r.header.Get("X-Freebuff-Event-Id"); got != eventID {
			t.Errorf("%s X-Freebuff-Event-Id = %q, want echoed body clientEventId %q", path, got, eventID)
		}
	}
	imp := fired["/api/v1/ads/impression"]
	var impBody map[string]any
	if err := json.Unmarshal([]byte(imp.body), &impBody); err != nil {
		t.Fatal(err)
	}
	ua, _ := impBody["userAgent"].(string)
	if !strings.Contains(ua, "Chrome/151.0.0.0") {
		t.Errorf("impression userAgent = %q, want the Chrome-151 body UA", ua)
	}
	if os, _ := impBody["os"].(string); os == "" {
		t.Error("impression missing os")
	}
	click := fired["/api/v1/ads/click"]
	var clickBody map[string]any
	if err := json.Unmarshal([]byte(click.body), &clickBody); err != nil {
		t.Fatal(err)
	}
	if clickBody["surface"] != "waiting_room" {
		t.Errorf("click surface = %v, want waiting_room (the auction surface)", clickBody["surface"])
	}
}

// TestWaitingRoomChainAdFailureNeverFailsAdmission pins best-effort: a 500
// on the impression leg must not block the click leg, and ad failures
// never surface to the caller (admission must not depend on ads).
func TestWaitingRoomChainAdFailureNeverFailsAdmission(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path]++
		mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/ads":
			writeBodyJSON(w, 200, `{"ads":[{"impUrl":"https://gravity.example/imp/9"}]}`)
		case "/api/v1/ads/impression":
			writeBodyJSON(w, 500, `{"error":"boom"}`)
		case "/api/v1/ads/click", "/api/v1/freebuff/streak":
			writeBodyJSON(w, 200, `{"ok":true}`)
		default:
			writeBodyJSON(w, 404, `{"error":"not found"}`)
		}
	}))
	defer srv.Close()
	client, err := New("tok-a", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	client.FireWaitingRoomChain(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if seen["/api/v1/ads/click"] == 0 {
		t.Error("click leg never fired after the impression 500 (chain must continue best-effort)")
	}
	if seen["/api/v1/freebuff/streak"] == 0 {
		t.Error("streak call never fired after ad failures (chain must finish)")
	}
}

// TestSessionParseSurfacesPrivacyDecision pins gap-5 passthrough: the two
// tip-a9ef9942d FreebuffPrivacyDecision members (spur_suspicious_limited,
// client_hints_limited) parse onto SessionState.PrivacyDecision verbatim,
// with no window fabricated and no branch taken.
func TestSessionParseSurfacesPrivacyDecision(t *testing.T) {
	for _, decision := range []string{"spur_suspicious_limited", "client_hints_limited", "allowed_clean"} {
		req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/freebuff/session", nil)
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
		client, err := New("tok-a", testConfig("http://127.0.0.1:1", nil))
		if err != nil {
			t.Fatal(err)
		}
		body := `{"status":"limited","privacyDecision":"` + decision + `"}`
		st, err := client.parseSessionResponse(req, resp, body)
		if err != nil {
			t.Fatalf("parse %q: %v", decision, err)
		}
		if st.PrivacyDecision != decision {
			t.Errorf("PrivacyDecision = %q, want verbatim %q", st.PrivacyDecision, decision)
		}
		if st.UnavailableWindow != nil {
			t.Errorf("PrivacyDecision %q fabricated an UnavailableWindow", decision)
		}
	}
}

// TestRetryDelayExpBackoffShape pins the CLI control-path backoff shape
// (codebuff-api.ts calculateBackoffDelay): 1s*2^(attempt-1) plus 0-30%
// jitter, capped at 10s. Bounds are deterministic; the jitter draw inside
// is not pinned.
func TestRetryDelayExpBackoffShape(t *testing.T) {
	srv := newRecordingUpstream()
	defer srv.Close()
	client, err := New("tok-a", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		attempt  int
		min, max time.Duration
	}{
		{1, time.Second, 1300 * time.Millisecond},
		{2, 2 * time.Second, 2600 * time.Millisecond},
		{3, 4 * time.Second, 5200 * time.Millisecond},
		{4, 8 * time.Second, 10 * time.Second},
		{5, 10 * time.Second, 10 * time.Second},
		{9, 10 * time.Second, 10 * time.Second},
	}
	for _, tc := range cases {
		for i := 0; i < 25; i++ {
			if got := client.retryDelay(tc.attempt); got < tc.min || got > tc.max {
				t.Errorf("retryDelay(%d) = %s, want [%s, %s]", tc.attempt, got, tc.min, tc.max)
			}
		}
	}
}

// TestBodylessPostsOmitContentType pins the fetch-without-header rule
// (database.ts sends no explicit Content-Type): the bodyless admission
// POST carries no Content-Type, while calls with a body (chat, START)
// keep application/json via newRequest's iff-body rule.
func TestBodylessPostsOmitContentType(t *testing.T) {
	srv := newRecordingUpstream()
	defer srv.Close()
	client, err := New("tok-a", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := client.CreateSession(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.StartRun(ctx, "agent-1"); err != nil {
		t.Fatal(err)
	}
	admission := findReq(t, srv, "/api/v1/freebuff/session/admission", http.MethodPost)
	if got := admission.header.Get("Content-Type"); got != "" {
		t.Errorf("bodyless admission POST Content-Type = %q, want absent", got)
	}
	start := findReq(t, srv, "/api/v1/agent-runs", http.MethodPost)
	if got := start.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("START with body Content-Type = %q, want application/json", got)
	}
}
