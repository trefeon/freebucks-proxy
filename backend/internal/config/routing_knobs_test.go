package config

// MASQ knob tests: SLOTS_PER_ACCOUNT, QUEUE_WAIT, QUEUE_DEPTH,
// MAX_SPILL_ACCOUNTS — defaults, env/file/JSON tiers, floors, and
// validation. Restart behavior: all four are live-apply (read per Acquire
// through the atomic config pointer; no pool rebuild).

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withRoutingEnvUnset removes the MASQ knobs from the environment so a
// test observes loader defaults, restoring any prior values afterwards.
func withRoutingEnvUnset(t *testing.T) {
	t.Helper()
	keys := []string{"SLOTS_PER_ACCOUNT", "QUEUE_WAIT", "QUEUE_DEPTH", "MAX_SPILL_ACCOUNTS"}
	saved := make(map[string]string, len(keys))
	present := make(map[string]bool, len(keys))
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			saved[k], present[k] = v, true
			t.Setenv(k, "")
			if err := os.Unsetenv(k); err != nil {
				t.Fatal(err)
			}
		} else {
			present[k] = false
		}
	}
	t.Cleanup(func() {
		for _, k := range keys {
			if present[k] {
				t.Setenv(k, saved[k])
			} else {
				_ = os.Unsetenv(k)
			}
		}
	})
}

func TestRoutingKnobDefaults(t *testing.T) {
	unsetConfigEnv(t)
	withRoutingEnvUnset(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SlotsPerAccount != 2 {
		t.Errorf("SlotsPerAccount = %d, want 2", cfg.SlotsPerAccount)
	}
	if cfg.QueueWait != 30*time.Second {
		t.Errorf("QueueWait = %v, want 30s", cfg.QueueWait)
	}
	if cfg.QueueDepth != 16 {
		t.Errorf("QueueDepth = %d, want 16", cfg.QueueDepth)
	}
	if cfg.MaxSpillAccounts != 0 {
		t.Errorf("MaxSpillAccounts = %d, want 0 (unbounded)", cfg.MaxSpillAccounts)
	}
}

func TestRoutingKnobEnvOverrides(t *testing.T) {
	unsetConfigEnv(t)
	withRoutingEnvUnset(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("SLOTS_PER_ACCOUNT", "1")
	t.Setenv("QUEUE_WAIT", "5s")
	t.Setenv("QUEUE_DEPTH", "4")
	t.Setenv("MAX_SPILL_ACCOUNTS", "2")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SlotsPerAccount != 1 {
		t.Errorf("SlotsPerAccount = %d, want 1 (bunker)", cfg.SlotsPerAccount)
	}
	if cfg.QueueWait != 5*time.Second {
		t.Errorf("QueueWait = %v, want 5s", cfg.QueueWait)
	}
	if cfg.QueueDepth != 4 {
		t.Errorf("QueueDepth = %d, want 4", cfg.QueueDepth)
	}
	if cfg.MaxSpillAccounts != 2 {
		t.Errorf("MaxSpillAccounts = %d, want 2", cfg.MaxSpillAccounts)
	}
}

func TestRoutingKnobFloors(t *testing.T) {
	unsetConfigEnv(t)
	withRoutingEnvUnset(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	// SLOTS_PER_ACCOUNT=0 means unlimited (no slot gating at all);
	// negative values floor to 0 instead of failing the load.
	t.Setenv("SLOTS_PER_ACCOUNT", "0")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(SLOTS_PER_ACCOUNT=0): %v", err)
	}
	if cfg.SlotsPerAccount != 0 {
		t.Errorf("SlotsPerAccount(0) = %d, want 0 (unlimited)", cfg.SlotsPerAccount)
	}
	t.Setenv("SLOTS_PER_ACCOUNT", "-3")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load(SLOTS_PER_ACCOUNT=-3): %v", err)
	}
	if cfg.SlotsPerAccount != 0 {
		t.Errorf("SlotsPerAccount(-3) = %d, want floor 0", cfg.SlotsPerAccount)
	}
	// QUEUE_DEPTH=0 disables queueing (fail over at once when full).
	t.Setenv("SLOTS_PER_ACCOUNT", "2")
	t.Setenv("QUEUE_DEPTH", "0")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.QueueDepth != 0 {
		t.Errorf("QueueDepth = %d, want 0 (no queueing)", cfg.QueueDepth)
	}
	// QUEUE_WAIT is zero-tolerant like BURST_WINDOW: non-positive falls
	// back to the 30s default.
	t.Setenv("QUEUE_DEPTH", "16")
	t.Setenv("QUEUE_WAIT", "0s")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.QueueWait != 30*time.Second {
		t.Errorf("QueueWait(0s) = %v, want 30s fallback", cfg.QueueWait)
	}
}

func TestRoutingKnobValidation(t *testing.T) {
	unsetConfigEnv(t)
	withRoutingEnvUnset(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("QUEUE_DEPTH", "-1")
	if _, err := Load(""); err == nil {
		t.Error("negative QUEUE_DEPTH accepted")
	}
	t.Setenv("QUEUE_DEPTH", "16")
	t.Setenv("QUEUE_WAIT", "bogus")
	if _, err := Load(""); err == nil {
		t.Error("bogus QUEUE_WAIT accepted")
	}
	t.Setenv("QUEUE_WAIT", "30s")
	t.Setenv("MAX_SPILL_ACCOUNTS", "-1")
	if _, err := Load(""); err == nil {
		t.Error("negative MAX_SPILL_ACCOUNTS accepted")
	}
	t.Setenv("MAX_SPILL_ACCOUNTS", "0")
	// Hand-built configs bypass the Load floor: Validate rejects negatives
	// directly (Load itself floors them to 0 = unlimited, proven above).
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.SlotsPerAccount = -1
	if err := cfg.Validate(); err == nil {
		t.Error("negative SlotsPerAccount accepted")
	}
	cfg.SlotsPerAccount = 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("SlotsPerAccount=0 rejected: %v (0 = unlimited)", err)
	}
}

func TestRoutingKnobDotenvTier(t *testing.T) {
	unsetConfigEnv(t)
	withRoutingEnvUnset(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	dir := t.TempDir()
	env := "SLOTS_PER_ACCOUNT=1\nQUEUE_WAIT=7s\nQUEUE_DEPTH=3\nMAX_SPILL_ACCOUNTS=1\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SlotsPerAccount != 1 || cfg.QueueWait != 7*time.Second || cfg.QueueDepth != 3 || cfg.MaxSpillAccounts != 1 {
		t.Errorf("dotenv tier not applied: cap=%d wait=%v depth=%d spill=%d",
			cfg.SlotsPerAccount, cfg.QueueWait, cfg.QueueDepth, cfg.MaxSpillAccounts)
	}
}

func TestRoutingKnobJSONTier(t *testing.T) {
	unsetConfigEnv(t)
	withRoutingEnvUnset(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	dir := t.TempDir()
	doc := `{"SLOTS_PER_ACCOUNT": 1, "QUEUE_WAIT": "9s", "QUEUE_DEPTH": 5, "MAX_SPILL_ACCOUNTS": 2}`
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SlotsPerAccount != 1 || cfg.QueueWait != 9*time.Second || cfg.QueueDepth != 5 || cfg.MaxSpillAccounts != 2 {
		t.Errorf("JSON tier not applied: cap=%d wait=%v depth=%d spill=%d",
			cfg.SlotsPerAccount, cfg.QueueWait, cfg.QueueDepth, cfg.MaxSpillAccounts)
	}
}
