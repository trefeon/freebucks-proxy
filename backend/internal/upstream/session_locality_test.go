package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// sessionWireCapture records the x-fb-timezone header of every session call a
// mock upstream receives, keyed by HTTP method.
type sessionWireCapture struct {
	mu   sync.Mutex
	seen map[string]string
	ts   *httptest.Server
}

func newSessionWireCapture(t *testing.T) *sessionWireCapture {
	t.Helper()
	c := &sessionWireCapture{seen: make(map[string]string)}
	c.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.seen[r.Method] = r.Header.Get(FreebucksTimezoneHeader)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			_, _ = io.WriteString(w, `{"status":"ended"}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-loc","expiresAt":"2030-01-01T00:00:00Z"}`)
	}))
	t.Cleanup(c.ts.Close)
	return c
}

func (c *sessionWireCapture) header(method string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen[method]
}

// callAll drives one admission POST, one poll GET (with the instance header)
// and one DELETE so every session-call shape is observed.
func (c *sessionWireCapture) callAll(t *testing.T, client *Client) {
	t.Helper()
	ctx := context.Background()
	if _, err := client.CreateSessionForModel(ctx, ""); err != nil {
		t.Fatalf("admission POST: %v", err)
	}
	if _, err := client.GetSessionWithOpts(ctx, "inst-loc", true); err != nil {
		t.Fatalf("poll GET: %v", err)
	}
	if _, err := client.EndSession(ctx, "inst-loc"); err != nil {
		t.Fatalf("refund DELETE: %v", err)
	}
}

// TestSessionCallsDeclareResolverZone pins the locality contract on the wire:
// an installed resolver's zone is what every session call declares — the
// admission POST (where the server derives the account's daily reset zone),
// the poll GET, and the DELETE/refund. All three funnel through the same
// chokepoint (or its EndSession mirror), so one resolver installation covers
// the whole session surface.
func TestSessionCallsDeclareResolverZone(t *testing.T) {
	capture := newSessionWireCapture(t)
	client, err := New("tok", testConfig(capture.ts.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	client.SetLocalityResolver(func() string { return "Pacific/Auckland" })

	capture.callAll(t, client)

	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		if got := capture.header(method); got != "Pacific/Auckland" {
			t.Errorf("%s %s = %q, want Pacific/Auckland (installed resolver)", method, FreebucksTimezoneHeader, got)
		}
	}
	// A second, independent resolver value (the live-config closure can change
	// after a reload) must take effect on the next call, never a cached one.
	client.SetLocalityResolver(func() string { return "Asia/Tokyo" })
	if _, err := client.GetSessionWithOpts(context.Background(), "inst-loc", true); err != nil {
		t.Fatalf("poll after re-resolve: %v", err)
	}
	if got := capture.header(http.MethodGet); got != "Asia/Tokyo" {
		t.Errorf("%s after re-resolve = %q, want Asia/Tokyo", FreebucksTimezoneHeader, got)
	}
}

// TestSessionCallsFallBackToHostZoneWithoutResolver pins today's behaviour
// byte for byte: with no resolver installed (or a resolver that has nothing to
// say yet) the host zone is declared — the same value the probe already pins —
// on the admission POST, the poll GET, and the DELETE.
func TestSessionCallsFallBackToHostZoneWithoutResolver(t *testing.T) {
	capture := newSessionWireCapture(t)
	client, err := New("tok", testConfig(capture.ts.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	want := localIANATimezone()

	capture.callAll(t, client)
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		if got := capture.header(method); got != want {
			t.Errorf("%s %s = %q, want %q (host zone without a resolver)", method, FreebucksTimezoneHeader, got, want)
		}
	}

	// A resolver mid-boot (a region probe that has not landed yet) must not
	// blank the header: "" falls back to the host zone.
	client.SetLocalityResolver(func() string { return "" })
	if _, err := client.GetSessionWithOpts(context.Background(), "inst-loc", true); err != nil {
		t.Fatalf("poll with empty resolver: %v", err)
	}
	if got := capture.header(http.MethodGet); got != want {
		t.Errorf("%s with an empty resolver = %q, want %q", FreebucksTimezoneHeader, got, want)
	}
}

// TestHostZoneTreatsLocalAsUnset pins the placeholder rule: Go's bare "Local"
// (and an empty name) means "whatever the host is set to" and must reach the
// locality rule as unset, while a real name passes through untouched.
func TestHostZoneTreatsLocalAsUnset(t *testing.T) {
	orig := time.Local
	defer func() { time.Local = orig }()

	time.Local = time.FixedZone("Local", 0)
	if got := HostZone(); got != "" {
		t.Errorf("HostZone() with the bare Local placeholder = %q, want \"\"", got)
	}
	time.Local = time.FixedZone("Asia/Tokyo", 9*3600)
	if got := HostZone(); got != "Asia/Tokyo" {
		t.Errorf("HostZone() = %q, want Asia/Tokyo", got)
	}
	time.Local = time.FixedZone("", 0)
	if got := HostZone(); got != "" {
		t.Errorf("HostZone() with an empty name = %q, want \"\"", got)
	}
}
