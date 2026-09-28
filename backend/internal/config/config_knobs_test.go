package config

// Wave-3 config knob tests: the bounded finish queue (#90), the
// draining-list bounds (#55), and the re-admit lead (#99).
// (Retired: SESSION_PROBE_CACHE_TTL — the admission probe-cache skip is
// gone; polls are unconditional like the CLI.)

import (
	"testing"
	"time"
)

func TestWave3KnobDefaults(t *testing.T) {
	unsetConfigEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RunFinishQueueSize != 64 {
		t.Errorf("RunFinishQueueSize = %d, want 64", cfg.RunFinishQueueSize)
	}
	if cfg.RunFinishInlineTimeout != 250*time.Millisecond {
		t.Errorf("RunFinishInlineTimeout = %v, want 250ms", cfg.RunFinishInlineTimeout)
	}
	if cfg.RunsDrainQueueCap != 64 || cfg.RunsDrainTTL != 10*time.Minute {
		t.Errorf("drain bounds = %d/%v, want 64/10m", cfg.RunsDrainQueueCap, cfg.RunsDrainTTL)
	}
	if cfg.SessionReAdmitLead != 60*time.Second {
		t.Errorf("SessionReAdmitLead = %v, want 60s", cfg.SessionReAdmitLead)
	}
}

func TestWave3KnobEnvOverrides(t *testing.T) {
	unsetConfigEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("RUN_FINISH_QUEUE_SIZE", "8")
	t.Setenv("RUN_FINISH_INLINE_TIMEOUT", "100ms")
	t.Setenv("RUNS_DRAIN_QUEUE_CAP", "3")
	t.Setenv("RUNS_DRAIN_TTL", "5m")
	t.Setenv("SESSION_RE_ADMIT_LEAD", "30s")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RunFinishQueueSize != 8 || cfg.RunFinishInlineTimeout != 100*time.Millisecond {
		t.Errorf("finish queue = %d/%v, want 8/100ms", cfg.RunFinishQueueSize, cfg.RunFinishInlineTimeout)
	}
	if cfg.RunsDrainQueueCap != 3 || cfg.RunsDrainTTL != 5*time.Minute {
		t.Errorf("drain bounds = %d/%v, want 3/5m", cfg.RunsDrainQueueCap, cfg.RunsDrainTTL)
	}
	if cfg.SessionReAdmitLead != 30*time.Second {
		t.Errorf("session knob = %v, want 30s", cfg.SessionReAdmitLead)
	}
}

func TestWave3KnobValidation(t *testing.T) {
	unsetConfigEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("RUN_FINISH_QUEUE_SIZE", "-5")
	if _, err := Load(""); err == nil {
		t.Error("negative RUN_FINISH_QUEUE_SIZE accepted")
	}
	t.Setenv("RUN_FINISH_QUEUE_SIZE", "64")
	t.Setenv("SESSION_RE_ADMIT_LEAD", "bogus")
	if _, err := Load(""); err == nil {
		t.Error("bogus SESSION_RE_ADMIT_LEAD accepted")
	}
}
