package upstream

import (
	"freebuff-proxy/backend/internal/config"
	"net/http"
	"testing"
)

// egressTransport builds a client and unwraps its *http.Transport: the knob
// is applied once at construction, so the transport is the whole proof.
func egressTransport(t *testing.T, cfg *config.Config) *http.Transport {
	t.Helper()
	client, err := New("tok-egress", cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tr, ok := client.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport = %T, want *http.Transport", client.http.Transport)
	}
	return tr
}

// TestEgressDirectByDefault pins the default path: no knob, no proxy — even
// with ambient HTTP_PROXY/HTTPS_PROXY set. The DefaultTransport clone
// inherits ProxyFromEnvironment; the constructor must keep it disabled so
// operator env vars never route upstream traffic through a proxy.
func TestEgressDirectByDefault(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	t.Setenv("http_proxy", "http://127.0.0.1:9")
	t.Setenv("https_proxy", "http://127.0.0.1:9")
	if tr := egressTransport(t, testConfig("", nil)); tr.Proxy != nil {
		t.Error("default transport Proxy is set, want nil (direct egress)")
	}
}

// TestEgressProxyOverrideHonored pins the override: the transport's Proxy
// func must resolve upstream requests to exactly the configured exit, for
// every accepted scheme.
func TestEgressProxyOverrideHonored(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"http", "http://127.0.0.1:40000"},
		{"https", "https://exit.example:8080"},
		{"socks5", "socks5://127.0.0.1:40000"},
		{"socks5 with userinfo", "socks5://user:pass@127.0.0.1:40000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := egressTransport(t, testConfig("", func(c *config.Config) {
				c.UpstreamEgressProxy = tt.raw
			}))
			if tr.Proxy == nil {
				t.Fatal("transport Proxy is nil, want the configured exit")
			}
			req, err := http.NewRequest(http.MethodGet, "https://www.codebuff.com/api/v1/freebuff/session", nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := tr.Proxy(req)
			if err != nil {
				t.Fatalf("Proxy(req): %v", err)
			}
			if got == nil || got.String() != tt.raw {
				t.Errorf("Proxy(req) = %v, want %q", got, tt.raw)
			}
			// Proxied egress always speaks HTTP/1.1 through the exit (the
			// custom h2 transport cannot dial through a proxy): the
			// constructor pins it by setting TLSNextProto.
			if tr.TLSNextProto == nil {
				t.Error("proxied egress must pin HTTP/1.1 (non-nil TLSNextProto), got nil")
			}
		})
	}
}

// TestEgressProxyInvalidFallsBackToDirect pins the EgressProxyURL contract
// for hand-built Configs that bypass Validate: a host-less value must not
// fail requests — the client silently keeps the direct path.
func TestEgressProxyInvalidFallsBackToDirect(t *testing.T) {
	tr := egressTransport(t, testConfig("", func(c *config.Config) {
		c.UpstreamEgressProxy = "http://"
	}))
	if tr.Proxy != nil {
		t.Error("host-less knob must fall back to direct (nil Proxy), got a proxy func")
	}
}
