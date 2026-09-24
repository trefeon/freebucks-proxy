package config

import (
	"testing"
)

// TestPooledDefault pins the pool-only routing: with AUTH_TOKENS set the
// effective mode is pooled, and without tokens it stays pooled as well
// (no bridge/hybrid modes exist).
func TestPooledDefault(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1,tok-2")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.EffectiveMode(); got != "pooled" {
		t.Errorf("EffectiveMode() = %q, want pooled", got)
	}
}
