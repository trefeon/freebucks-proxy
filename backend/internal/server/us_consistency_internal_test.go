package server

import (
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
	"testing"
)

// TestApplyConsistencyAdsDecision pins the wiring the ads lane consumes:
// under the preset the installed pin is (declared session zone, en-US);
// with the knob off (or no config) the pin clears. The consult helpers
// themselves are covered in the upstream package; this covers the decision.
func TestApplyConsistencyAdsDecision(t *testing.T) {
	t.Cleanup(func() { upstream.SetConsistencyAdsOverride("", "") })
	newStack := func(mut func(*config.Config)) *Server {
		t.Helper()
		mock := testutil.NewMock()
		t.Cleanup(mock.Close)
		srv, _ := newTestServerStack(t, nil, []*testutil.MockUpstream{mock}, mut, nil, nil)
		return srv
	}

	// Preset on, boring host, foreign region: pin is (US zone, en-US).
	srv := newStack(func(c *config.Config) { c.USConsistency = true })
	srv.SetHostZoneSource(func() string { return "UTC" })
	zone, locale, active := srv.applyConsistencyAds()
	if !active {
		t.Fatal("applyConsistencyAds active = false under the preset, want true")
	}
	if zone != config.USConsistencyZone {
		t.Errorf("pin zone = %q, want %q", zone, config.USConsistencyZone)
	}
	if locale != config.USConsistencyLocale {
		t.Errorf("pin locale = %q, want %q", locale, config.USConsistencyLocale)
	}

	// Preset on with an explicit zone: the pin follows the explicit zone.
	srv = newStack(func(c *config.Config) {
		c.USConsistency = true
		c.SessionTimezone = "Pacific/Auckland"
	})
	zone, locale, active = srv.applyConsistencyAds()
	if !active || zone != "Pacific/Auckland" || locale != config.USConsistencyLocale {
		t.Errorf("explicit pin = (%q, %q, %v), want (Pacific/Auckland, en-US, true)", zone, locale, active)
	}

	// Knob off: the pin clears (no stale pin outlives the knob).
	srv = newStack(nil)
	if zone, locale, active := srv.applyConsistencyAds(); active || zone != "" || locale != "" {
		t.Errorf("cleared pin = (%q, %q, %v), want (\"\", \"\", false)", zone, locale, active)
	}
}
