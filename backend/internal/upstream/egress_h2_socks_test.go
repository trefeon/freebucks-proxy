package upstream

// Tests for HTTP/2 through a socks5:// UPSTREAM_EGRESS_PROXY exit (#50/#51).
//
// Background: x/net/http2.Transport has no Proxy support — it dials the
// origin directly via DialTLSContext, ignoring Transport.Proxy. Registering
// it while a socks5 exit was configured would route the H2 handshake past
// the exit (live: the server's H2 bytes landing in an H1 reader,
// "net/http: HTTP/1.x transport connection broken: malformed HTTP
// response"), so proxied egress used to force HTTP/1.1. SOCKS is a layer-4
// stream to the origin, so the TLS handshake (and the ALPN negotiation
// inside it) still runs end-to-end against the origin — the fix dials the
// h2 transport through the exit (socksBaseDial under the same utls dialer).
// http/https exits keep the H1-forced path (CONNECT-tunnel mismatch).
//
// All tests are hermetic: the mock SOCKS5 exit speaks the handshake on
// 127.0.0.1 and echoes afterwards, and origins use .invalid names that must
// never resolve — any direct dial fails the test instead of touching the
// network. The handshake always fails at TLS (the mock is not the origin);
// what the tests pin is ROUTING: the exit must observe the CONNECT.

import (
	"context"
	"errors"
	"freebuff-proxy/backend/internal/config"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockSOCKS5 is a minimal no-auth-or-username/password SOCKS5 exit: it
// completes the greeting and one CONNECT, records the requested target,
// replies success, then echoes bytes until close (the echo breaks the
// caller's TLS handshake deterministically — routing, not completion, is
// under test).
type mockSOCKS5 struct {
	t            *testing.T
	ln           net.Listener
	requireAuth  bool
	wantUser     string
	wantPass     string
	mu           sync.Mutex
	targets      []string
	authAttempts []string
}

func newMockSOCKS5(t *testing.T, requireAuth bool, user, pass string) *mockSOCKS5 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen mock socks5: %v", err)
	}
	m := &mockSOCKS5{t: t, ln: ln, requireAuth: requireAuth, wantUser: user, wantPass: pass}
	go m.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return m
}

func (m *mockSOCKS5) addr() string { return m.ln.Addr().String() }

func (m *mockSOCKS5) serve() {
	for {
		c, err := m.ln.Accept()
		if err != nil {
			return
		}
		go m.handle(c)
	}
}

func (m *mockSOCKS5) readN(c net.Conn, n int) []byte {
	m.t.Helper()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		_ = c.Close()
		m.t.Errorf("mock socks5 read: %v", err)
	}
	return buf
}

func (m *mockSOCKS5) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	greet := m.readN(c, 2)
	if greet[0] != 0x05 {
		return
	}
	methods := m.readN(c, int(greet[1]))
	hasNoAuth, hasUserPass := false, false
	for _, mt := range methods {
		switch mt {
		case 0x00:
			hasNoAuth = true
		case 0x02:
			hasUserPass = true
		}
	}
	if m.requireAuth {
		if !hasUserPass {
			_, _ = c.Write([]byte{0x05, 0xff})
			return
		}
		_, _ = c.Write([]byte{0x05, 0x02})
		hdr := m.readN(c, 2)
		ulen := int(hdr[1])
		user := string(m.readN(c, ulen))
		plen := int(m.readN(c, 1)[0])
		pass := string(m.readN(c, plen))
		m.mu.Lock()
		m.authAttempts = append(m.authAttempts, user+":"+pass)
		m.mu.Unlock()
		if user != m.wantUser || pass != m.wantPass {
			_, _ = c.Write([]byte{0x01, 0x01})
			return
		}
		_, _ = c.Write([]byte{0x01, 0x00})
	} else {
		if !hasNoAuth {
			_, _ = c.Write([]byte{0x05, 0xff})
			return
		}
		_, _ = c.Write([]byte{0x05, 0x00})
	}
	hdr := m.readN(c, 4)
	if hdr[1] != 0x01 { // CONNECT only
		return
	}
	var host string
	switch hdr[3] {
	case 0x01: // IPv4
		ip := m.readN(c, 4)
		host = net.IP(ip).String()
	case 0x03: // domain
		n := int(m.readN(c, 1)[0])
		host = string(m.readN(c, n))
	case 0x04: // IPv6
		ip := m.readN(c, 16)
		host = net.IP(ip).String()
	default:
		return
	}
	port := m.readN(c, 2)
	target := host + ":" + itoaPort(port)
	m.mu.Lock()
	m.targets = append(m.targets, target)
	m.mu.Unlock()
	_, _ = c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	// Echo: the caller's TLS handshake fails on its own echoed bytes,
	// deterministically. Plain-HTTP callers read their echoed request back
	// as the response — the live "malformed HTTP response" shape.
	_, _ = io.Copy(c, c)
}

func itoaPort(b []byte) string {
	return strconv.Itoa(int(b[0])<<8 | int(b[1]))
}

func (m *mockSOCKS5) targetsSnapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.targets...)
}

// roundTripErr performs one request with a bounded context and returns the
// error string; a success is fatal (the mock can never complete TLS).
func roundTripErr(t *testing.T, c *Client, method, url string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.http.Transport.RoundTrip(req)
	if err == nil {
		t.Fatal("RoundTrip through the mock exit succeeded, want the TLS handshake to fail")
	}
	return err.Error()
}

func socksCfg(t *testing.T, exit string, mut func(*config.Config)) *config.Config {
	t.Helper()
	return testConfig("", func(c *config.Config) {
		c.UpstreamEgressProxy = exit
		if mut != nil {
			mut(c)
		}
	})
}

// TestSocksH2TraversesProxy is the fix regression: stealth + HTTP2_UPSTREAM
// through a socks5 exit must open the origin stream THROUGH the exit (the
// mock observes CONNECT origin.invalid:443) with the utls dialer driving
// the handshake (the error carries the stealth wrapper — proof the https
// request reached the registered h2 transport, not the h1 fallback). The
// .invalid origin never resolves, so any direct dial fails the test via DNS
// instead of reaching the mock — exactly the naive-fix failure (h2t with a
// direct baseDial ignoring Transport.Proxy).
func TestSocksH2TraversesProxy(t *testing.T) {
	m := newMockSOCKS5(t, false, "", "")
	c, err := New("tok-socks-h2", socksCfg(t, "socks5://"+m.addr(), func(c *config.Config) {
		c.HTTP2Upstream = true
		c.TLSFingerprint = "chrome126"
	}))
	if err != nil {
		t.Fatal(err)
	}
	msg := roundTripErr(t, c, http.MethodGet, "https://origin.invalid/")
	if !strings.Contains(msg, "stealth:") {
		t.Errorf("socks h2 dial error = %q, want the stealth wrapper (https dispatched to the utls h2 dialer)", msg)
	}
	got := m.targetsSnapshot()
	if len(got) != 1 || got[0] != "origin.invalid:443" {
		t.Errorf("mock exit observed CONNECT %v, want [origin.invalid:443] (H2 handshake bypassed the exit)", got)
	}
}

// TestSocksPlainH2TraversesProxy pins the plain path: no fingerprint, H2 on,
// socks5 exit — the stdlib negotiates H2 over the socks Proxy func, so the
// exit must still observe the CONNECT. Guards the TLSNextProto kill switch
// from re-widening to socks exits.
func TestSocksPlainH2TraversesProxy(t *testing.T) {
	m := newMockSOCKS5(t, false, "", "")
	c, err := New("tok-socks-plain-h2", socksCfg(t, "socks5://"+m.addr(), func(c *config.Config) {
		c.HTTP2Upstream = true
	}))
	if err != nil {
		t.Fatal(err)
	}
	msg := roundTripErr(t, c, http.MethodGet, "https://origin.invalid/")
	if strings.Contains(msg, "stealth:") {
		t.Errorf("plain socks h2 dial error = %q, want a plain stdlib error", msg)
	}
	got := m.targetsSnapshot()
	if len(got) != 1 || got[0] != "origin.invalid:443" {
		t.Errorf("mock exit observed CONNECT %v, want [origin.invalid:443]", got)
	}
}

// TestSocksH1TraversesProxy pins the H1 path through the same exit: plain
// HTTP/1.1 over socks5. The mock echoes the request back, so the H1 reader
// chokes on its own echoed bytes — the live "malformed HTTP response"
// failure shape, here proving the bytes demonstrably crossed the exit.
func TestSocksH1TraversesProxy(t *testing.T) {
	m := newMockSOCKS5(t, false, "", "")
	c, err := New("tok-socks-h1", socksCfg(t, "socks5://"+m.addr(), func(c *config.Config) {
		c.HTTP2Upstream = false
	}))
	if err != nil {
		t.Fatal(err)
	}
	msg := roundTripErr(t, c, http.MethodGet, "http://origin.invalid/")
	// Family prefix, not the full variant: Go reports "malformed HTTP
	// status code" for echoed request bytes and "malformed HTTP response"
	// for a bare H2 preface, depending on what the reader chokes on.
	if !strings.Contains(msg, "malformed HTTP") {
		t.Errorf("socks h1 echo error = %q, want the malformed-response family (echoed request read as response)", msg)
	}
	got := m.targetsSnapshot()
	if len(got) != 1 || got[0] != "origin.invalid:80" {
		t.Errorf("mock exit observed CONNECT %v, want [origin.invalid:80]", got)
	}
}

// TestHttpProxyStaysH1WithH2Enabled is the negative guard: an http(s) exit
// keeps the H1-forced path even with HTTP2_UPSTREAM (CONNECT-tunnel ALPN
// mismatch), so a refused exit must surface a plain proxy-dial error —
// never the stealth wrapper that would prove an h2 transport took the
// request.
func TestHttpProxyStaysH1WithH2Enabled(t *testing.T) {
	c, err := New("tok-http-proxy-h1", socksCfg(t, "http://127.0.0.1:9", func(c *config.Config) {
		c.HTTP2Upstream = true
		c.TLSFingerprint = "chrome126"
	}))
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport = %T, want *http.Transport", c.http.Transport)
	}
	if tr.TLSNextProto == nil {
		t.Error("http-proxied egress must pin HTTP/1.1 (non-nil TLSNextProto), got nil")
	}
	msg := roundTripErr(t, c, http.MethodGet, "https://127.0.0.1:1/")
	if strings.Contains(msg, "stealth:") {
		t.Errorf("http-proxy dial error = %q, want a plain proxy-dial error (h2 must not take proxied requests)", msg)
	}
}

// TestSocksBaseDialRelaysThroughExit exercises the dial-layer seam
// directly: userinfo becomes SOCKS auth, the CONNECT target reaches the
// exit, and bytes relay. A nil exit yields nil (default net dialer).
func TestSocksBaseDialRelaysThroughExit(t *testing.T) {
	if socksBaseDial(nil) != nil {
		t.Error("socksBaseDial(nil) = non-nil, want nil (default dialer fallback)")
	}
	m := newMockSOCKS5(t, true, "user", "pass")
	dial := socksBaseDial(mustParseURL(t, "socks5://user:pass@"+m.addr()))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp", "example.com:80")
	if err != nil {
		t.Fatalf("socksBaseDial dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write through exit: %v", err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(conn, echo); err != nil {
		t.Fatalf("read echo through exit: %v", err)
	}
	if string(echo) != "ping" {
		t.Errorf("echo = %q, want %q", echo, "ping")
	}
	got := m.targetsSnapshot()
	if len(got) != 1 || got[0] != "example.com:80" {
		t.Errorf("mock exit observed CONNECT %v, want [example.com:80]", got)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.authAttempts) != 1 || m.authAttempts[0] != "user:pass" {
		t.Errorf("auth attempts = %v, want [user:pass]", m.authAttempts)
	}
}

// TestSocksBaseDialCanceledContext pins cancellation: a dead context fails
// before any network touch.
func TestSocksBaseDialCanceledContext(t *testing.T) {
	m := newMockSOCKS5(t, false, "", "")
	dial := socksBaseDial(mustParseURL(t, "socks5://"+m.addr()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := dial(ctx, "tcp", "example.com:80"); err == nil {
		t.Error("canceled-context dial succeeded, want ctx.Err()")
	}
	if got := m.targetsSnapshot(); len(got) != 0 {
		t.Errorf("mock exit observed CONNECT %v on a canceled dial, want none", got)
	}
}

// TestH2BytesInH1Reader is the live-symptom pin against a fixed mock: a
// server that speaks the HTTP/2 connection preface to a plain HTTP/1.1
// client. The H1 reader must reject it as a malformed response — the exact
// "net/http: HTTP/1.x transport connection broken" family the socks-H2
// bypass produced in production.
func TestH2BytesInH1Reader(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		// HTTP/2 connection preface + empty SETTINGS frame, then hold the
		// conn so the client fails on the bytes, not on EOF.
		_, _ = c.Write([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n\x00\x00\x00\x04\x00\x00\x00\x00\x00"))
		time.Sleep(5 * time.Second)
	}()
	tr := &http.Transport{Proxy: nil}
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ln.Addr().String()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tr.RoundTrip(req)
	if err == nil {
		t.Fatal("H1 RoundTrip against an H2-speaking server succeeded, want malformed-response failure")
	}
	// The rejection must come from the response bytes, not from our own
	// context: the stdlib surfaces H2-preface rejection either as the classic
	// malformed-response family or — under -race on a loaded machine, when
	// the read loop peeks before the preface arrives — as a peek failure
	// whose nil cause formats as %!w(<nil>). A context lapse here would mean
	// the client hung instead of rejecting, which stays a failure.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("H1-vs-H2 error = %q, want a response rejection, not a context lapse", err.Error())
	}
}
