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
