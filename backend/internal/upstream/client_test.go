package upstream

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/testutil"
)

// testConfig builds a config; baseURL "" keeps the default (only for tests
// that do not perform requests). All request-making tests pass mock.URL().
func testConfig(baseURL string, mut func(*config.Config)) *config.Config {
	cfg := &config.Config{
		ListenAddr:         ":3457",
		UpstreamBaseURL:    "https://www.codebuff.com",
		AuthTokens:         []string{"tok-a"},
		RotationInterval:   6 * time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 30 * time.Second,
		RegistryRefresh:    6 * time.Hour,
	}
	if baseURL != "" {
		cfg.UpstreamBaseURL = baseURL
	}
	if mut != nil {
		mut(cfg)
	}
	return cfg
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

// TestCrossHostRedirectStripsToken verifies a redirect outside the bare<->www
// trust pair does not carry x-codebuff-api-key (or Authorization): Go strips
// the latter itself but not the former, so the raw token used to leak to any
// redirect target. Identical-host and bare<->www hops (the CLI's 301/307s,
// live-captured 2026-09-27) keep their credentials, restoring Authorization
// from the predecessor when Go already removed it.
func TestCrossHostRedirectStripsToken(t *testing.T) {
	const token = "tok-secret-redirect"

	t.Run("scheme downgrade strips token", func(t *testing.T) {
		// https -> http to the SAME host (plaintext) must drop both
		// credential headers; the token must never cross onto a cleartext
		// hop, even when the hostname is unchanged.
		check := func(t *testing.T, from, to string, wantStripped bool) {
			t.Helper()
			client, err := New(token, testConfig("http://127.0.0.1:1", nil))
			if err != nil {
				t.Fatal(err)
			}
			via := []*http.Request{{
				URL:    mustParseURL(t, from),
				Header: http.Header{},
			}}
			via[0].Header.Set("x-codebuff-api-key", token)
			via[0].Header.Set("Authorization", "Bearer "+token)
			req := &http.Request{URL: mustParseURL(t, to), Header: via[0].Header.Clone()}
			if err := client.http.CheckRedirect(req, via); err != nil {
				t.Fatalf("CheckRedirect: %v", err)
			}
			gotKey := req.Header.Get("x-codebuff-api-key")
			gotAuth := req.Header.Get("Authorization")
			if wantStripped {
				if gotKey != "" || gotAuth != "" {
					t.Errorf("redirect %s -> %s kept credentials (key %q auth %q), want stripped", from, to, gotKey, gotAuth)
				}
			} else {
				if gotKey != token || gotAuth != "Bearer "+token {
					t.Errorf("redirect %s -> %s stripped credentials (key %q auth %q), want kept", from, to, gotKey, gotAuth)
				}
			}
		}
		check(t, "https://www.codebuff.com", "http://www.codebuff.com", true)
		check(t, "https://www.codebuff.com", "http://www.codebuff.com:8080", true)
		check(t, "https://www.codebuff.com", "https://www.codebuff.com", false)
		check(t, "http://www.codebuff.com", "https://www.codebuff.com", false)
		check(t, "https://codebuff.com", "https://www.codebuff.com", false)
		check(t, "https://www.codebuff.com", "https://codebuff.com", false)
		check(t, "https://codebuff.com", "https://api.codebuff.com", true)
		check(t, "https://codebuff.com", "https://evil.com", true)
		check(t, "https://www.codebuff.com", "https://www.evil.com", true)
		check(t, "https://127.0.0.1:1", "https://localhost:2", true)
	})

	keySeen := make(chan string, 1)
	authSeen := make(chan string, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keySeen <- r.Header.Get("x-codebuff-api-key")
		authSeen <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Rewrite 127.0.0.1 -> localhost: different hostname, outside the
		// bare<->www trust pair, so credentials must be stripped.
		crossURL := strings.Replace(target.URL+"/final", "127.0.0.1", "localhost", 1)
		http.Redirect(w, r, crossURL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client, err := New(token, testConfig(origin.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	req, err := client.newRequest(context.Background(), http.MethodGet, "/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := <-keySeen; got != "" {
		t.Errorf("cross-host redirect carried x-codebuff-api-key %q, want stripped", got)
	}
	if got := <-authSeen; got != "" {
		t.Errorf("cross-host redirect carried Authorization %q, want stripped", got)
	}

	sameKey := make(chan string, 1)
	same := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
			return
		}
		sameKey <- r.Header.Get("x-codebuff-api-key")
		w.WriteHeader(http.StatusOK)
	}))
	defer same.Close()

	sameClient, err := New(token, testConfig(same.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	sameReq, err := sameClient.newRequest(context.Background(), http.MethodGet, "/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	sameResp, err := sameClient.http.Do(sameReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = sameResp.Body.Close()
	if got := <-sameKey; got != "" {
		t.Errorf("same-host request carried x-codebuff-api-key %q, want absent (newRequest-only paths never set it; agent-runs set it separately)", got)
	}
}

// TestRedirectRestoresStrippedAuth pins the belt-and-braces restore: when Go
// removes Authorization before CheckRedirect on a trusted hop, the handler
// copies it back from the immediate predecessor; on an untrusted hop the
// stripped header stays empty (never fabricated).
func TestRedirectRestoresStrippedAuth(t *testing.T) {
	const token = "tok-restore-me"
	client, err := New(token, testConfig("http://127.0.0.1:1", nil))
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, from, to string, wantAuth bool) {
		t.Helper()
		via := []*http.Request{{
			URL:    mustParseURL(t, from),
			Header: http.Header{"Authorization": {"Bearer " + token}},
		}}
		req := &http.Request{URL: mustParseURL(t, to), Header: http.Header{}}
		if err := client.http.CheckRedirect(req, via); err != nil {
			t.Fatalf("CheckRedirect: %v", err)
		}
		got := req.Header.Get("Authorization")
		if wantAuth && got != "Bearer "+token {
			t.Errorf("redirect %s -> %s auth = %q, want restored Bearer", from, to, got)
		}
		if !wantAuth && got != "" {
			t.Errorf("redirect %s -> %s auth = %q, want empty (never fabricated)", from, to, got)
		}
	}
	check(t, "https://codebuff.com/start", "https://www.codebuff.com/final", true)
	check(t, "https://codebuff.com/start", "https://evil.com/final", false)
}

func TestWrapDecompress(t *testing.T) {
	const want = `{"status":"active","instanceId":"inst-abc-123"}`
	cases := []struct {
		name       string
		encoding   string
		compress   func([]byte) []byte
		wantErrSub string
	}{
		{"identity passthrough", "", nil, ""},
		{"gzip", "gzip", func(b []byte) []byte {
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			_, _ = zw.Write(b)
			_ = zw.Close()
			return buf.Bytes()
		}, ""},
		{"deflate", "deflate", func(b []byte) []byte {
			var buf bytes.Buffer
			zw, _ := flate.NewWriter(&buf, flate.DefaultCompression)
			_, _ = zw.Write(b)
			_ = zw.Close()
			return buf.Bytes()
		}, ""},
		// RFC 9110 §8.4.1.3: deflate = zlib-wrapped (RFC 1950). A conforming
		// server's body must decode; the raw-flate fallback must not break
		// the existing raw case above.
		{"deflate zlib-wrapped", "deflate", func(b []byte) []byte {
			var buf bytes.Buffer
			zw := zlib.NewWriter(&buf)
			_, _ = zw.Write(b)
			_ = zw.Close()
			return buf.Bytes()
		}, ""},
		{"corrupt gzip", "gzip", func(b []byte) []byte {
			return []byte("this is not gzip data")
		}, "gzip:"},
		// Multi-value Content-Encoding is rejected with a clear error, not
		// silently mis-decoded.
		{"multi-value encoding rejected", "gzip, br", nil, "unsupported Content-Encoding"},
		{"brotli rejected", "br", nil, "unsupported Content-Encoding"},
		{"zstd rejected", "zstd", nil, "unsupported Content-Encoding"},
		{"unsupported encoding", "lz4", nil, "unsupported Content-Encoding"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(want)
			if tc.compress != nil {
				body = tc.compress([]byte(want))
			}
			resp := &http.Response{
				Header: http.Header{},
				Body:   io.NopCloser(bytes.NewReader(body)),
			}
			if tc.encoding != "" {
				resp.Header.Set("Content-Encoding", tc.encoding)
			}
			err := wrapDecompress(resp)
			if tc.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("wrapDecompress err = %v, want %q", err, tc.wantErrSub)
					return
				}
				return
			}
			if err != nil {
				t.Fatalf("wrapDecompress: %v", err)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			_ = resp.Body.Close()
			if string(got) != want {
				t.Errorf("body = %q, want %q", got, want)
			}
			if resp.Header.Get("Content-Encoding") != "" {
				t.Error("Content-Encoding header not stripped")
			}
		})
	}
}

// TestDumpRedactsTokenHeaders verifies the debug dump redacts the
// Authorization header, and that a chat dump never contains an
// x-codebuff-api-key line: no wire path sets it (agent-runs are
// Bearer-only like the CLI; the redaction list still covers it
// defensively). Regression: dump() only redacted Authorization, so
// DEBUG_DUMP=true leaked the plaintext token into dump/ files via
// x-codebuff-api-key.
func TestDumpRedactsTokenHeaders(t *testing.T) {
	t.Chdir(t.TempDir())
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = http.StatusUnauthorized
	mock.ChatErrorBody = `{"error":"unauthorized"}`

	client, err := New("tok-secret-1234", testConfig(mock.URL(), func(c *config.Config) {
		c.DebugDump = true
	}))
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if _, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m"}, body); err == nil {
		t.Fatal("expected error from 401 response")
	}

	entries, err := filepath.Glob(filepath.Join("dump", "*.dump"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no dump file written")
	}
	data, err := os.ReadFile(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	dump := string(data)
	if strings.Contains(dump, "tok-secret-1234") {
		t.Fatalf("dump file leaks token:\n%s", dump)
	}
	if !strings.Contains(dump, "Authorization: [redacted]") {
		t.Errorf("dump file missing redacted Authorization header:\n%s", dump)
	}
	// The chat request never carries x-codebuff-api-key (no wire path sets
	// it), so it must not appear in the chat dump (the defensive redaction
	// list stays for any future setter).
	if strings.Contains(strings.ToLower(dump), "x-codebuff-api-key") {
		t.Errorf("dump file contains an x-codebuff-api-key header line (absent on the chat path):\n%s", dump)
	}
}

// TestRedirectMultihop guards multi-hop redirect token semantics: an
// A→B→A loop keeps the token at the origin (B never sees it, A receives its
// own token on the loop-back hop), the 3-hop limit errors out, and a
// port-differing same-host hop is treated as cross-host (token stripped).
func TestRedirectMultihop(t *testing.T) {
	const token = "tok-multihop"

	t.Run("A-B-A loop", func(t *testing.T) {
		bKeySeen := make(chan string, 1)
		aKeySeen := make(chan string, 2)

		var targetBURL string
		originA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/start":
				http.Redirect(w, r, targetBURL+"/b", http.StatusTemporaryRedirect)
			default:
				aKeySeen <- r.Header.Get("x-codebuff-api-key")
				w.WriteHeader(http.StatusOK)
			}
		}))
		defer originA.Close()

		targetB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			bKeySeen <- r.Header.Get("x-codebuff-api-key")
			// Loop back to the ORIGIN: Go re-copies headers from via[0]
			// (the origin), so the origin must receive its own token again.
			http.Redirect(w, r, originA.URL+"/loop", http.StatusTemporaryRedirect)
		}))
		defer targetB.Close()
		targetBURL = targetB.URL

		client, err := New(token, testConfig(originA.URL, nil))
		if err != nil {
			t.Fatal(err)
		}
		req, err := client.newRequest(context.Background(), http.MethodGet, "/start", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()

		if got := <-bKeySeen; got != "" {
			t.Errorf("intermediate host B received token %q, want stripped", got)
		}
		if got := <-aKeySeen; got != "" {
			t.Errorf("loop-back hop to A carried %q, want absent (newRequest-only paths never set x-codebuff-api-key)", got)
		}
	})

	t.Run("three-hop limit", func(t *testing.T) {
		targetC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
		}))
		defer targetC.Close()
		targetB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, targetC.URL+"/c", http.StatusTemporaryRedirect)
		}))
		defer targetB.Close()
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, targetB.URL+"/b", http.StatusTemporaryRedirect)
		}))
		defer origin.Close()

		client, err := New(token, testConfig(origin.URL, nil))
		if err != nil {
			t.Fatal(err)
		}
		req, err := client.newRequest(context.Background(), http.MethodGet, "/start", nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.http.Do(req)
		if err == nil {
			t.Fatal("3-redirect chain succeeded, want too-many-redirects error")
			return
		}
		if !strings.Contains(err.Error(), "too many redirects") {
			t.Errorf("err = %v, want too many redirects", err)
		}
	})

	t.Run("port-differing same-host strips token", func(t *testing.T) {
		keySeen := make(chan string, 1)
		otherPort := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			keySeen <- r.Header.Get("x-codebuff-api-key")
			w.WriteHeader(http.StatusOK)
		}))
		defer otherPort.Close()
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, otherPort.URL+"/final", http.StatusTemporaryRedirect)
		}))
		defer origin.Close()

		client, err := New(token, testConfig(origin.URL, nil))
		if err != nil {
			t.Fatal(err)
		}
		req, err := client.newRequest(context.Background(), http.MethodGet, "/start", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		// Pin current behavior: Go's Host includes the port, so a different
		// port is treated as cross-host and the token is dropped.
		if got := <-keySeen; got != "" {
			t.Errorf("port-differing hop carried token %q, want stripped", got)
		}
	})
}

// TestReqIDContextHelpers pins the D1 ctx plumbing: withReqID stores the id
// and ReqID reads it through descendant contexts (the timeout wraps in
// ChatCompletions/do derive from the wrapped ctx).
func TestReqIDContextHelpers(t *testing.T) {
	if got := ReqID(context.Background()); got != "" {
		t.Errorf("ReqID(background) = %q, want empty", got)
	}
	ctx := withReqID(context.Background(), "req-123")
	if got := ReqID(ctx); got != "req-123" {
		t.Errorf("ReqID = %q, want req-123", got)
	}
	child, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if got := ReqID(child); got != "req-123" {
		t.Errorf("ReqID(child) = %q, want req-123 (value must survive descendant wraps)", got)
	}
}

// TestDumpWriteFailureLogsWarn verifies the dump-write failure log: when DEBUG_DUMP is enabled but
// the dump write fails (a regular file occupies the dump/ path), the failure
// is logged as a WARN with path and err instead of being swallowed.
func TestDumpWriteFailureLogsWarn(t *testing.T) {
	orig := slog.Default()
	var sink bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&sink, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	t.Chdir(t.TempDir())
	// A regular FILE named "dump": MkdirAll fails and WriteFile hits
	// ENOTDIR/EEXIST — deterministic failure injection.
	if err := os.WriteFile("dump", []byte("occupied"), 0o644); err != nil {
		t.Fatal(err)
	}
	client, err := New("tok", testConfig("", func(c *config.Config) { c.DebugDump = true }))
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "https://www.codebuff.com/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	client.dump("chat", req, []byte("request body"), http.StatusOK, "response body")

	logs := sink.String()
	if !strings.Contains(logs, "debug dump write failed") {
		t.Fatalf("dump WARN missing: %s", logs)
	}
	if !strings.Contains(logs, "path=") || !strings.Contains(logs, "err=") {
		t.Errorf("dump WARN missing path/err attrs: %s", logs)
	}
}

func TestDumpWritesRequestBodyAndResponse(t *testing.T) {
	t.Chdir(t.TempDir())
	client, err := New("tok", testConfig("", func(c *config.Config) { c.DebugDump = true }))
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "https://www.codebuff.com/api/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sensitive-secret-token")
	req.Header.Set("User-Agent", "test-ua")

	reqBody := []byte(`{"model":"z-ai/glm-5.3-flash","prompt":"secret-pass"}`)
	client.dump("chat", req, reqBody, http.StatusOK, "response content")

	entries, err := os.ReadDir("dump")
	if err != nil || len(entries) != 1 {
		t.Fatalf("dump dir entries = %v, err = %v; want 1 file", entries, err)
	}
	dumpPath := filepath.Join("dump", entries[0].Name())
	content, err := os.ReadFile(dumpPath)
	if err != nil {
		t.Fatal(err)
	}
	str := string(content)
	if !strings.Contains(str, "POST https://www.codebuff.com/api/v1/chat/completions") {
		t.Errorf("dump missing method URL line: %s", str)
	}
	if strings.Contains(str, "sensitive-secret-token") {
		t.Errorf("dump leaked Authorization header: %s", str)
	}
	if !strings.Contains(str, "[request body]\n"+string(reqBody)) {
		t.Errorf("dump missing request body: %s", str)
	}
	if !strings.Contains(str, "[status 200]\nresponse content") {
		t.Errorf("dump missing response status/body: %s", str)
	}
}
