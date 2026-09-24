package config

import (
	"testing"
)

// TestEffectiveSessionTimezone pins the US-consistency preset precedence:
// an explicit SESSION_TIMEZONE always wins; otherwise US_CONSISTENCY
// supplies the US zone; otherwise auto ("").
func TestEffectiveSessionTimezone(t *testing.T) {
	cases := []struct {
		name   string
		cfg    Config
		want   string
		preset bool
	}{
		{"unset resolves auto", Config{}, "", false},
		{"explicit wins without preset", Config{SessionTimezone: "Asia/Tokyo"}, "Asia/Tokyo", false},
		{"preset supplies US zone", Config{USConsistency: true}, USConsistencyZone, true},
		{"explicit wins over preset", Config{USConsistency: true, SessionTimezone: "Pacific/Auckland"}, "Pacific/Auckland", false},
		{"explicit blank-with-spaces still counts as preset", Config{USConsistency: true, SessionTimezone: "  "}, USConsistencyZone, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.EffectiveSessionTimezone(); got != tc.want {
				t.Errorf("EffectiveSessionTimezone() = %q, want %q", got, tc.want)
			}
			if got := tc.cfg.USPresetActive(); got != tc.preset {
				t.Errorf("USPresetActive() = %v, want %v", got, tc.preset)
			}
		})
	}
	var nilCfg *Config
	if got := nilCfg.EffectiveSessionTimezone(); got != "" {
		t.Errorf("nil EffectiveSessionTimezone() = %q, want auto", got)
	}
	if nilCfg.USPresetActive() {
		t.Error("nil USPresetActive() = true, want false")
	}
	if USConsistencyZone != "America/New_York" {
		t.Errorf("USConsistencyZone = %q, want the documented US default", USConsistencyZone)
	}
	if USConsistencyLocale != "en-US" {
		t.Errorf("USConsistencyLocale = %q, want en-US", USConsistencyLocale)
	}
}

// TestUSConsistencyLoadsFromEnv pins the knob end to end: a US_CONSISTENCY
// env line enables the preset, and the default stays off (a fresh install
// copying .env.example must not change its declared signals).
func TestUSConsistencyLoadsFromEnv(t *testing.T) {
	unsetConfigEnv(t)
	t.Setenv("US_CONSISTENCY", "true")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.USConsistency {
		t.Error("USConsistency = false with US_CONSISTENCY=true, want true")
	}
	if got := cfg.EffectiveSessionTimezone(); got != USConsistencyZone {
		t.Errorf("EffectiveSessionTimezone() = %q, want %q", got, USConsistencyZone)
	}
}

// TestUSConsistencyDefaultsOff pins the safe default: no env, no preset.
func TestUSConsistencyDefaultsOff(t *testing.T) {
	unsetConfigEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.USConsistency {
		t.Error("USConsistency = true by default, want false (preset is opt-in)")
	}
}

// TestUSConsistencyRendersInData pins the dashboard effective-config row:
// the knob must be visible (not a phantom key) with its bool shape.
func TestUSConsistencyRendersInData(t *testing.T) {
	cfg := Config{USConsistency: true}
	found := false
	for _, e := range cfg.Data() {
		if e.Key == "US_CONSISTENCY" {
			found = true
			if e.Value != "true" {
				t.Errorf("Data US_CONSISTENCY = %q, want true", e.Value)
			}
		}
	}
	if !found {
		t.Error("Data() has no US_CONSISTENCY row, want one (catalog lockstep)")
	}
}
