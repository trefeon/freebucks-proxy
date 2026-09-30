// Package egress probes the gateway's outbound network path — the public IP
// and country code seen by a remote service — over the direct egress route.
// Results back the doctor's region row and give operators a fast
// ban-avoidance signal (requests unexpectedly appearing to originate from
// another country).
package egress

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ProbeURL is the Cloudflare trace endpoint that reports the caller's
// public IP (ip=) and ISO country code (loc=). Exported so tests can point
// the probe at a local server; production never changes it.
var ProbeURL = "https://www.cloudflare.com/cdn-cgi/trace"

// DefaultTTL is how long a cached probe result stays fresh and the default
// interval between background probes.
const DefaultTTL = 10 * time.Minute

// ProbeTimeout bounds a single probe request end to end (dial, TLS, body).
const ProbeTimeout = 10 * time.Second

// Result is one probe: the public IP and 2-letter ISO country code seen at
// the far end of the egress path. Err carries the failure when the probe
// could not complete; callers treat that as "unknown egress" (fail-open).
type Result struct {
	IP      string
	Country string
	Err     error
}

// Probe GETs ProbeURL over the direct path — a Path carrying only a dialer —
// bounded by timeout, and parses the ip= and loc= lines of the Cloudflare
// trace body. Any failure — dial, TLS, non-200 status, unreadable body —
// returns Result{Err: err}; the probe never retries and never touches the
// configured upstream auth.
func Probe(ctx context.Context, dialer func(ctx context.Context, network, addr string) (net.Conn, error), timeout time.Duration) Result {
	return ProbePath(ctx, Path{Dialer: dialer}, timeout)
}

// ProbePath GETs ProbeURL through one egress path and parses the trace body.
// p.Dialer routes the connection; p.Proxy, when set, is the sanctioned exit
// the gateway's upstream calls egress through (UPSTREAM_EGRESS_PROXY), so the
// reported IP/country is what the upstream country resolver sees rather than
// this host's own address.
func ProbePath(ctx context.Context, p Path, timeout time.Duration) Result {
	dialer := p.Dialer
	if dialer == nil {
		dialer = DirectDialer(timeout)
	}
	if timeout <= 0 {
		timeout = ProbeTimeout
	}
	// Dedicated transport without ProxyFromEnvironment: the probe goes
	// through exactly the dialer and exit given, never whatever env proxies
	// happen to exist.
	proxy := func(*http.Request) (*url.URL, error) { return nil, nil }
	if p.Proxy != nil {
		proxy = http.ProxyURL(p.Proxy)
	}
	tr := &http.Transport{
		// Explicit proxy selection: a nil Proxy field would resolve to
		// http.ProxyFromEnvironment and silently route region reads through
		// env proxies, contradicting the dialer-bound guarantee above.
		Proxy:               proxy,
		DialContext:         dialer,
		MaxIdleConns:        1,
		IdleConnTimeout:     DefaultTTL,
		TLSHandshakeTimeout: timeout,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: timeout}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, ProbeURL, nil)
	if err != nil {
		return Result{Err: err}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Result{Err: fmt.Errorf("egress probe: %s returned %s", ProbeURL, resp.Status)}
	}

	var ip, loc string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "ip="):
			ip = strings.TrimPrefix(line, "ip=")
		case strings.HasPrefix(line, "loc="):
			loc = strings.TrimPrefix(line, "loc=")
		}
	}
	if err := sc.Err(); err != nil {
		return Result{Err: fmt.Errorf("egress probe: reading trace body: %w", err)}
	}
	return Result{IP: ip, Country: loc}
}

// DirectDialer returns the dial function for the direct egress path: a
// plain net dialer with the given connection timeout.
func DirectDialer(timeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return (&net.Dialer{Timeout: timeout}).DialContext
}

// Path identifies one egress path to probe: the cache key ("direct") and
// the dialer that routes the probe connection, plus the explicit exit the
// gateway's upstream calls use when one is configured (UPSTREAM_EGRESS_PROXY).
type Path struct {
	Key    string
	Dialer func(ctx context.Context, network, addr string) (net.Conn, error)
	// Proxy is the sanctioned egress exit, nil = direct. It is the same exit
	// the upstream transport is built with, so probing the path reports the
	// address and country the upstream resolver actually sees.
	Proxy *url.URL
}

// probeAll probes every path concurrently and returns one Result per key.
// A failing path yields a Result with Err set (fail-open) and never aborts
// the other probes.
func probeAll(ctx context.Context, paths []Path, timeout time.Duration) map[string]Result {
	results := make(map[string]Result, len(paths))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range paths {
		wg.Add(1)
		go func(p Path) {
			defer wg.Done()
			r := ProbePath(ctx, p, timeout)
			mu.Lock()
			results[p.Key] = r
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	return results
}

type cachedResult struct {
	Result
	At time.Time
}

// Cache stores the latest probe result per egress path so the health
// surface and doctor can report the egress region without re-probing.
type Cache struct {
	mu      sync.RWMutex
	entries map[string]cachedResult
	ttl     time.Duration
}

// NewCache returns a cache with the default 10-minute TTL.
func NewCache() *Cache { return NewCacheWithTTL(DefaultTTL) }

// NewCacheWithTTL returns a cache whose entries expire after ttl.
func NewCacheWithTTL(ttl time.Duration) *Cache {
	return &Cache{entries: make(map[string]cachedResult), ttl: ttl}
}

// Get returns the cached result for key when present and unexpired.
func (c *Cache) Get(key string) (Result, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[key]
	if !ok {
		return Result{}, false
	}
	if time.Since(e.At) > c.ttl {
		return Result{}, false
	}
	return e.Result, true
}

// Set stores the latest probe result for key, refreshing its timestamp.
func (c *Cache) Set(key string, r Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cachedResult{Result: r, At: time.Now()}
}
