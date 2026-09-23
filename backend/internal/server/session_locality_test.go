package server_test

import (
	"encoding/json"
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/egress"
	"freebucks-proxy/backend/internal/server"
	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	_ "time/tzdata"
)

// localityTracker builds a tracker whose region is already known (the shared
// cache is pre-populated) and which is never started, so no probe traffic
// leaves the test. Country "" means "region unknown".
func localityTracker(country string) *egress.Tracker {
	cache := egress.NewCache()
	if country != "" {
		cache.Set("direct", egress.Result{Country: country})
	}
	return egress.NewTracker(cache, egress.Path{Key: "direct"}, time.Hour)
}

// healthzLocality is the /healthz locality surface (additive fields).
type healthzLocality struct {
	Status                string `json:"status"`
	Mode                  string `json:"mode"`
	SessionTimezone       string `json:"session_timezone"`
	SessionTimezoneSource string `json:"session_timezone_source"`
	EgressRegion          string `json:"egress_region"`
}

// captureAdmissionTimezone installs a session handler on the mock that records
// the x-fb-timezone header of the admission POST (and forwards the active
// session body the pool needs to serve a chat).
func captureAdmissionTimezone(mock *testutil.MockUpstream, out *sync.Map) {
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		out.Store(r.Method, r.Header.Get(upstream.FreebucksTimezoneHeader))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-loc","expiresAt":"2030-01-01T00:00:00Z"}`)
	}
}

// newLocalityServer builds the real stack (mock upstream, pool, server) behind
// an httptest server with the given config mutation, returning the server for
// tracker wiring and the logger sink for warn assertions.
func newLocalityServer(t *testing.T, mock *testutil.MockUpstream, mut func(*config.Config), logSink *strings.Builder) (*server.Server, *httptest.Server) {
	t.Helper()
	var logger *slog.Logger
	if logSink != nil {
		logger = slog.New(slog.NewTextHandler(logSink, nil))
	} else {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	srv, _ := server.NewTestServerStack(t, nil, []*testutil.MockUpstream{mock}, mut, logger, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

func fetchLocality(t *testing.T, ts *httptest.Server) (healthzLocality, map[string]any) {
	t.Helper()
	resp, data := doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	var hz healthzLocality
	if err := json.Unmarshal(data, &hz); err != nil {
		t.Fatalf("healthz unmarshal: %v: %s", err, data)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("healthz raw unmarshal: %v", err)
	}
	return hz, raw
}

// TestHealthzSessionLocalityFieldsIsAdditive pins the /healthz locality fields
// as additive: the pre-existing surface is untouched, the three new keys are
// present, and the probe's public IP is NOT: /healthz is unauthenticated.
func TestHealthzSessionLocalityFieldsIsAdditive(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	_, ts := newLocalityServer(t, mock, nil, nil)

	hz, raw := fetchLocality(t, ts)
	if hz.Status != "ok" || hz.Mode == "" {
		t.Errorf("existing healthz surface = %+v, want ok + a mode", hz)
	}
	if hz.SessionTimezone == "" {
		t.Error("session_timezone empty, want the declared zone")
	}
	if _, err := time.LoadLocation(hz.SessionTimezone); err != nil {
		t.Errorf("session_timezone = %q, not a loadable zone: %v", hz.SessionTimezone, err)
	}
	if hz.SessionTimezoneSource != "host" && hz.SessionTimezoneSource != "utc" {
		t.Errorf("session_timezone_source = %q, want host or utc (no override, no region)", hz.SessionTimezoneSource)
	}
	if hz.EgressRegion != "" {
		t.Errorf("egress_region = %q, want empty (no tracker wired)", hz.EgressRegion)
	}
	for _, key := range []string{"status", "mode", "uptime_seconds", "models", "tokens", "bridge_tokens", "bridge_entries"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("healthz lost the pre-existing key %q", key)
		}
	}
	if _, ok := raw["egress_ip"]; ok {
		t.Error("healthz must never expose the egress IP (unauthenticated surface)")
	}
}

// TestSessionLocalityOverrideWinsEndToEnd pins the operator knob end to end:
// SESSION_TIMEZONE is what the admission POST declares upstream, and /healthz
// reports it with source "override".
func TestSessionLocalityOverrideWinsEndToEnd(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var seen sync.Map
	captureAdmissionTimezone(mock, &seen)

	srv, ts := newLocalityServer(t, mock, func(c *config.Config) {
		c.SessionTimezone = "Pacific/Auckland"
	}, nil)
	// The resolver is installed by the tracker wiring; the region stays
	// unknown (empty cache) so only the override can produce the result.
	srv.SetEgressTracker(localityTracker(""))

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}
	got, _ := seen.Load(http.MethodPost)
	if got != "Pacific/Auckland" {
		t.Errorf("admission POST %s = %v, want Pacific/Auckland (SESSION_TIMEZONE)", upstream.FreebucksTimezoneHeader, got)
	}

	hz, _ := fetchLocality(t, ts)
	if hz.SessionTimezone != "Pacific/Auckland" || hz.SessionTimezoneSource != "override" {
		t.Errorf("healthz locality = %q/%q, want Pacific/Auckland/override", hz.SessionTimezone, hz.SessionTimezoneSource)
	}
	if hz.EgressRegion != "" {
		t.Errorf("egress_region = %q, want empty (no probe result)", hz.EgressRegion)
	}
}

// TestSessionLocalityRegionFillsBoringHost pins the owner's rule: a UTC host
// carries no locality, so the detected egress region's zone is declared on the
// wire and surfaced as source "region".
func TestSessionLocalityRegionFillsBoringHost(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var seen sync.Map
	captureAdmissionTimezone(mock, &seen)

	srv, ts := newLocalityServer(t, mock, nil, nil)
	// The host zone is injected rather than written to time.Local: the seam
	// keeps the test deterministic without racing the httptest logger.
	srv.SetHostZoneSource(func() string { return "UTC" })
	srv.SetEgressTracker(localityTracker("NZ"))

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}
	if got, _ := seen.Load(http.MethodPost); got != "Pacific/Auckland" {
		t.Errorf("admission POST %s = %v, want Pacific/Auckland (NZ region, boring host)", upstream.FreebucksTimezoneHeader, got)
	}

	hz, _ := fetchLocality(t, ts)
	if hz.SessionTimezone != "Pacific/Auckland" || hz.SessionTimezoneSource != "region" || hz.EgressRegion != "NZ" {
		t.Errorf("healthz locality = %+v, want Pacific/Auckland/region/NZ", hz)
	}
}

// TestSessionLocalityNonBoringHostWinsOverRegion pins the other half of the
// rule: a deliberately configured host zone beats the detected region, which
// is still reported (honest) but not declared.
func TestSessionLocalityNonBoringHostWinsOverRegion(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var seen sync.Map
	captureAdmissionTimezone(mock, &seen)

	srv, ts := newLocalityServer(t, mock, nil, nil)
	srv.SetHostZoneSource(func() string { return "Asia/Tokyo" })
	srv.SetEgressTracker(localityTracker("NZ"))

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}
	if got, _ := seen.Load(http.MethodPost); got != "Asia/Tokyo" {
		t.Errorf("admission POST %s = %v, want Asia/Tokyo (configured host zone wins)", upstream.FreebucksTimezoneHeader, got)
	}

	hz, _ := fetchLocality(t, ts)
	if hz.SessionTimezone != "Asia/Tokyo" || hz.SessionTimezoneSource != "host" {
		t.Errorf("healthz locality = %q/%q, want Asia/Tokyo/host", hz.SessionTimezone, hz.SessionTimezoneSource)
	}
	if hz.EgressRegion != "NZ" {
		t.Errorf("egress_region = %q, want NZ (region still reported, just not declared)", hz.EgressRegion)
	}
}

// TestSessionLocalityUnknownRegionFallsBackToUTC pins the last branch: a host
// with no usable zone and a region the table does not know lands on UTC.
func TestSessionLocalityUnknownRegionFallsBackToUTC(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	srv, ts := newLocalityServer(t, mock, nil, nil)
	// A host zone that is neither boring nor a real IANA zone: it must lose to
	// nothing and the unknown region must not invent one either.
	srv.SetHostZoneSource(func() string { return "Nowhere" })
	srv.SetEgressTracker(localityTracker("ZZ"))

	hz, _ := fetchLocality(t, ts)
	if hz.SessionTimezone != "UTC" || hz.SessionTimezoneSource != "utc" {
		t.Errorf("healthz locality = %q/%q, want UTC/utc", hz.SessionTimezone, hz.SessionTimezoneSource)
	}
	if hz.EgressRegion != "ZZ" {
		t.Errorf("egress_region = %q, want ZZ (reported even when unmapped)", hz.EgressRegion)
	}
}

// TestSessionLocalityInvalidKnobFallsBackToAuto pins the fail-open knob: an
// invalid SESSION_TIMEZONE never fails a load or a request, it falls to auto
// (the region branch here) and the boot path warns about the bad value.
func TestSessionLocalityInvalidKnobFallsBackToAuto(t *testing.T) {
	var logSink strings.Builder
	mock := testutil.NewMock()
	defer mock.Close()
	srv, ts := newLocalityServer(t, mock, func(c *config.Config) {
		c.SessionTimezone = "Not/AZone"
	}, &logSink)
	srv.SetHostZoneSource(func() string { return "UTC" })
	srv.SetEgressTracker(localityTracker("NZ"))

	hz, _ := fetchLocality(t, ts)
	if hz.SessionTimezone != "Pacific/Auckland" || hz.SessionTimezoneSource != "region" {
		t.Errorf("healthz locality = %q/%q, want Pacific/Auckland/region (invalid knob -> auto)", hz.SessionTimezone, hz.SessionTimezoneSource)
	}
	if !strings.Contains(logSink.String(), "SESSION_TIMEZONE is not a valid IANA zone") {
		t.Errorf("no warning for the invalid value; log: %s", logSink.String())
	}
}
