package config

// Cooldown and session-park knob defaults: the Contract values every
// enforcement point assumed before the knobs existed. Load must reproduce
// them exactly when the environment sets nothing.

import (
	"testing"
	"time"
)

// TestCooldownKnobDefaults pins the Contract defaults for all 15
// cooldown/park knobs: an empty environment loads the previous hardcoded
// behavior verbatim, so a blank row can never zero-out a backoff. Park-ON
// is the production default here; a hand-built &Config{} is park-OFF by
// contrast (pinned by TestHandBuiltConfigDisablesPark in pool), so pool
// tests must set SessionParkEnabledFlag explicitly.
func TestCooldownKnobDefaults(t *testing.T) {
	unsetConfigEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	checks := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"COOLDOWN_DEFAULT_MS", cfg.DefaultMs(), 30 * time.Minute},
		{"COOLDOWN_COUNTRY_BLOCK_MS", cfg.CountryBlockMs(), 15 * time.Minute},
		{"COOLDOWN_CEILING_MS", cfg.CeilingMs(), 7 * 24 * time.Hour},
		{"COOLDOWN_FANOUT_MS", cfg.FanoutMs(), time.Minute},
		{"COOLDOWN_INVALID_MODEL_MS", cfg.InvalidModelMs(), time.Minute},
		{"COOLDOWN_OPAQUE_MS", cfg.OpaqueMs(), time.Minute},
		{"COOLDOWN_LOADSHED_MS", cfg.LoadShedMs(), 90 * time.Second},
		{"COOLDOWN_PEAK_HOURS_MS", cfg.PeakHoursMs(), 30 * time.Minute},
		{"SESSION_PARK_THRESHOLD_MS", cfg.SessionParkThresholdMs(), 15 * time.Minute},
		{"SESSION_POLL_MAX_MS", cfg.SessionPollMaxMs(), 5 * time.Minute},
		{"SMART_PROBE_BACKOFF_MAX_MS", cfg.SmartProbeBackoffMaxMs(), 30 * time.Minute},
		{"MATURITY_BACKOFF_MS", cfg.MaturityBackoffMs(), 3 * time.Minute},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v (Contract default)", c.name, c.got, c.want)
		}
	}
	if cfg.IpMaxReadmits() != 3 {
		t.Errorf("COOLDOWN_IP_MAX_READMITS = %d, want 3", cfg.IpMaxReadmits())
	}
	if cfg.IpJitterRatio() != 0.2 {
		t.Errorf("COOLDOWN_IP_JITTER_RATIO = %v, want 0.2", cfg.IpJitterRatio())
	}
	if !cfg.SessionParkEnabled() {
		t.Error("SESSION_PARK_ENABLED = false, want true (production default is park-ON)")
	}
}

// TestSessionParkExplicitOptOut pins the OFF switch: SESSION_PARK_ENABLED
// =false loads park-OFF, so tests and operators opt out explicitly rather
// than inheriting a silent default.
func TestSessionParkExplicitOptOut(t *testing.T) {
	unsetConfigEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("SESSION_PARK_ENABLED", "false")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SessionParkEnabled() {
		t.Error("SESSION_PARK_ENABLED=false loaded park-ON, want park-OFF")
	}
}

// TestCooldownIPMaxReadmitsZeroFallsBack pins the zero-tolerance contract
// for the one non-ms knob: an explicit 0 loads the default 3 (display and
// enforcement agree — runs would still enforce the old global, so 0 can
// never mean "no re-admits"), a positive value applies, and a negative
// still fails validation.
func TestCooldownIPMaxReadmitsZeroFallsBack(t *testing.T) {
	unsetConfigEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")

	t.Setenv("COOLDOWN_IP_MAX_READMITS", "0")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(0): %v", err)
	}
	if cfg.IpMaxReadmits() != 3 {
		t.Errorf("COOLDOWN_IP_MAX_READMITS=0 loads %d, want 3 (zero falls back)", cfg.IpMaxReadmits())
	}
	if (&Config{}).IpMaxReadmits() != 3 {
		t.Errorf("zero Config.IpMaxReadmits() = %d, want 3", (&Config{}).IpMaxReadmits())
	}

	t.Setenv("COOLDOWN_IP_MAX_READMITS", "7")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load(7): %v", err)
	}
	if cfg.IpMaxReadmits() != 7 {
		t.Errorf("COOLDOWN_IP_MAX_READMITS=7 loads %d, want 7", cfg.IpMaxReadmits())
	}

	t.Setenv("COOLDOWN_IP_MAX_READMITS", "-1")
	if _, err := Load(""); err == nil {
		t.Error("COOLDOWN_IP_MAX_READMITS=-1 accepted, want validation error")
	}
}

// TestDefaultRawConfigKeepsParkOn pins the production default at the raw
// layer: every load source layers over defaultRawConfig, so flipping this
// bool would silently turn the park off for all operators.
func TestDefaultRawConfigKeepsParkOn(t *testing.T) {
	if raw := defaultRawConfig(); !raw.SessionParkEnabled {
		t.Error("defaultRawConfig().SessionParkEnabled = false, want true (production park-ON)")
	}
}
