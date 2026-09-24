package server_test

import (
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
	"net/http"
	"sync"
	"testing"
)

// TestUSConsistencyPinsSessionZoneEndToEnd pins the preset contract on the
// wire: with US_CONSISTENCY and no explicit SESSION_TIMEZONE, the admission
// POST declares America/New_York (beating a boring host zone and a foreign
// detected region alike) and /healthz reports source "us-consistency".
func TestUSConsistencyPinsSessionZoneEndToEnd(t *testing.T) {
	t.Cleanup(func() { upstream.SetConsistencyAdsOverride("", "") })
	mock := testutil.NewMock()
	defer mock.Close()
	var seen sync.Map
	captureAdmissionTimezone(mock, &seen)

	srv, ts := newLocalityServer(t, mock, func(c *config.Config) {
		c.USConsistency = true
	}, nil)
	srv.SetHostZoneSource(func() string { return "UTC" })
	srv.SetEgressTracker(localityTracker("NZ"))

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		v, ok := seen.Load(method)
		if !ok {
			continue
		}
		if v != config.USConsistencyZone {
			t.Errorf("%s x-fb-timezone = %q, want preset %q", method, v, config.USConsistencyZone)
		}
	}
	if v, ok := seen.Load(http.MethodPost); !ok || v != config.USConsistencyZone {
		t.Errorf("admission x-fb-timezone = %v, want preset %q", v, config.USConsistencyZone)
	}

	hz, _ := fetchLocality(t, ts)
	if hz.SessionTimezone != config.USConsistencyZone || hz.SessionTimezoneSource != "us-consistency" {
		t.Errorf("healthz locality = %q/%q, want %q/us-consistency", hz.SessionTimezone, hz.SessionTimezoneSource, config.USConsistencyZone)
	}
	if hz.EgressRegion != "NZ" {
		t.Errorf("egress_region = %q, want NZ (still reported, just not declared)", hz.EgressRegion)
	}
}

// TestUSConsistencyExplicitTimezoneWins pins the escape hatch: an explicit
// SESSION_TIMEZONE beats the preset, reported as source "override".
func TestUSConsistencyExplicitTimezoneWins(t *testing.T) {
	t.Cleanup(func() { upstream.SetConsistencyAdsOverride("", "") })
	mock := testutil.NewMock()
	defer mock.Close()

	srv, ts := newLocalityServer(t, mock, func(c *config.Config) {
		c.USConsistency = true
		c.SessionTimezone = "Pacific/Auckland"
	}, nil)
	srv.SetHostZoneSource(func() string { return "UTC" })
	srv.SetEgressTracker(localityTracker("US"))

	hz, _ := fetchLocality(t, ts)
	if hz.SessionTimezone != "Pacific/Auckland" || hz.SessionTimezoneSource != "override" {
		t.Errorf("healthz locality = %q/%q, want Pacific/Auckland/override", hz.SessionTimezone, hz.SessionTimezoneSource)
	}
}

// TestUSConsistencyOffLeavesRuleAlone guards the default: without the knob
// a boring host still falls through to the detected region (existing rule,
// source "region" - not the preset).
func TestUSConsistencyOffLeavesRuleAlone(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	srv, ts := newLocalityServer(t, mock, nil, nil)
	srv.SetHostZoneSource(func() string { return "UTC" })
	srv.SetEgressTracker(localityTracker("NZ"))

	hz, _ := fetchLocality(t, ts)
	if hz.SessionTimezoneSource == "us-consistency" {
		t.Errorf("source = us-consistency without the preset, want the plain rule (%q)", hz.SessionTimezone)
	}
}
