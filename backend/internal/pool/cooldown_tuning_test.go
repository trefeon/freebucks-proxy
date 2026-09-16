package pool

// Cooldown-tuning isolation and park-gate pins: New/SetConfig push the
// operator's COOLDOWN_*/SESSION_* knob values into package globals (runs,
// upstream, and the three pool backoff caps below), so tests that build
// pools must snapshot first and restore on cleanup. Companion to the
// runs/upstream SnapshotTuning helpers.

import (
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/runs"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
	"testing"
	"time"
)

// saveCooldownTuning snapshots every package global mutated by New/SetConfig
// and restores them when the test finishes: a nonzero test config pushed
// through New must never leak into later tests in the same binary. Call it
// before building any pool with nonzero COOLDOWN_*/SESSION_POLL_* knobs
// (all-default configs only rewrite the defaults, but snapshot anyway —
// the cost is nil and the next editor may add a knob).
func saveCooldownTuning(t *testing.T) {
	t.Helper()
	runsSnap := runs.SnapshotTuning()
	upSnap := upstream.SnapshotTuning()
	pollMax, probeMax, maturity := sessionPollBackoffMax, quotaProbeMaxInterval, maturity429Backoff
	t.Cleanup(func() {
		runsSnap.Restore()
		upSnap.Restore()
		sessionPollBackoffMax, quotaProbeMaxInterval, maturity429Backoff = pollMax, probeMax, maturity
	})
}

// TestCooldownTuningPushDoesNotLeak pins the tuning-var isolation: a pool
// built with nonzero knobs enforces them (globals take the custom values)
// and the cleanup restores whatever preceded the test, so no nonzero value
// leaks across tests.
func TestCooldownTuningPushDoesNotLeak(t *testing.T) {
	runsBefore := runs.SnapshotTuning()
	upBefore := upstream.SnapshotTuning()
	pollBefore, probeBefore, matBefore := sessionPollBackoffMax, quotaProbeMaxInterval, maturity429Backoff

	t.Run("push", func(t *testing.T) {
		saveCooldownTuning(t)
		mock := testutil.NewMock()
		defer mock.Close()
		cfg := &config.Config{
			RotationInterval:   time.Hour,
			RequestTimeout:     15 * time.Minute,
			SessionCallTimeout: 5 * time.Second,
			RegistryRefresh:    6 * time.Hour,
			UpstreamBaseURL:    mock.URL(),
			// Distinctive nonzero values per enforcement point.
			CooldownDefault:        61 * time.Second,
			CooldownCountryBlock:   62 * time.Second,
			CooldownCeiling:        49 * time.Hour,
			CooldownFanout:         71 * time.Second,
			CooldownInvalidModel:   72 * time.Second,
			CooldownOpaque:         73 * time.Second,
			CooldownLoadShed:       74 * time.Second,
			CooldownPeakHours:      75 * time.Second,
			CooldownIPMaxReadmits:  7,
			CooldownIPJitterRatio:  0.33,
			SessionPollMax:         76 * time.Second,
			SmartProbeBackoffMax:   77 * time.Second,
			MaturityBackoff:        78 * time.Second,
			SessionParkEnabledFlag: true,
		}
		reg := registry.New(cfg, nil)
		reg.LoadFallback()
		if _, err := New(cfg, nil, nil, reg); err != nil {
			t.Fatal(err)
		}
		if got := runs.SnapshotTuning(); got.Default != 61*time.Second || got.CountryBlock != 62*time.Second || got.Ceiling != 49*time.Hour || got.IPMaxReadmits != 7 || got.IPJitterRatio != 0.33 {
			t.Errorf("runs tuning = %+v, want 61s/62s/49h/7/0.33 (custom knobs not enforced)", got)
		}
		if got := upstream.SnapshotTuning(); got.Fanout != 71*time.Second || got.InvalidModel != 72*time.Second || got.Opaque != 73*time.Second || got.LoadShed != 74*time.Second || got.PeakHours != 75*time.Second || got.Ceiling != 49*time.Hour {
			t.Errorf("upstream tuning = %+v, want 71s/72s/73s/74s/75s/49h (custom knobs not enforced)", got)
		}
		if sessionPollBackoffMax != 76*time.Second || quotaProbeMaxInterval != 77*time.Second || maturity429Backoff != 78*time.Second {
			t.Errorf("pool caps = %v/%v/%v, want 76s/77s/78s", sessionPollBackoffMax, quotaProbeMaxInterval, maturity429Backoff)
		}
	})

	// The subtest's cleanup ran at subtest end: every global is back.
	if got := runs.SnapshotTuning(); got != runsBefore {
		t.Errorf("runs tuning leaked: before %+v, after %+v", runsBefore, got)
	}
	if got := upstream.SnapshotTuning(); got != upBefore {
		t.Errorf("upstream tuning leaked: before %+v, after %+v", upBefore, got)
	}
	if sessionPollBackoffMax != pollBefore || quotaProbeMaxInterval != probeBefore || maturity429Backoff != matBefore {
		t.Errorf("pool caps leaked: before %v/%v/%v, after %v/%v/%v",
			pollBefore, probeBefore, matBefore, sessionPollBackoffMax, quotaProbeMaxInterval, maturity429Backoff)
	}
}

// TestHandBuiltConfigDisablesPark pins the hand-rolled-Config asymmetry: a
// zero-value Config parks NOTHING (SessionParkEnabledFlag defaults false)
// while production Load defaults park-ON (pinned by TestCooldownKnobDefaults
// in config). Every pool test must therefore set SessionParkEnabledFlag
// explicitly: a future park test without the flag fails its park assertion
// loudly (shouldPark false below) instead of silently covering the off path.
func TestHandBuiltConfigDisablesPark(t *testing.T) {
	cfg := &config.Config{}
	if cfg.SessionParkEnabled() {
		t.Error("zero-value Config.SessionParkEnabled() = true, want false (hand-built pools are park-OFF; set the flag explicitly)")
	}
	for _, d := range []time.Duration{time.Second, 5 * time.Minute, 15 * time.Minute} {
		if shouldPark(cfg, d) {
			t.Errorf("shouldPark(zero Config, %v) = true, want false (flagless pools never park)", d)
		}
	}
	if shouldPark(nil, 5*time.Minute) {
		t.Error("shouldPark(nil, 5m) = true, want false (nil cfg never parks)")
	}
}
