package config

// Unified-store derivation tests: ApplyOverlay rebuilds the effective config
// from the live mem snapshot without touching disk (SkipFiles), so
// dashboard mutations never re-read the boot seed.

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// applyBase loads a config from a temp seed (.env + JSON) with ambient env
// stripped, mirroring boot precedence.
func applyBase(t *testing.T, envContent string, jsonContent string) Config {
	t.Helper()
	unsetConfigEnv(t)
	t.Chdir(t.TempDir())
	if envContent != "" {
		if err := os.WriteFile(".env", []byte(envContent), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	configPath := ""
	if jsonContent != "" {
		configPath = filepath.Join(t.TempDir(), "cfg.json")
		if err := os.WriteFile(configPath, []byte(jsonContent), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load seed: %v", err)
	}
	return cfg
}

// TestApplyOverlayIdentity pins the sync-applier fidelity contract: with an
// empty delta the derivation reproduces the boot snapshot exactly (same
// effective knobs, same discovery receipt, same EnvFile), except the
// display-default LOG_LEVEL, which canonicalizes unset to "debug" —
// behaviorally identical (every resolveLogLevel path lands on debug).
func TestApplyOverlayIdentity(t *testing.T) {
	base := applyBase(t,
		"AUTH_TOKENS=tok-a,tok-b\nSAFE_MODE=true\nQUEUE_WAIT=45s\nLOG_FILE=/tmp/fb.log\nSESSION_PERSIST=true\n",
		`{"log_level":"info","rate_limit_per_ip":1.5}`)
	derived, err := ApplyOverlay(base, nil, nil)
	if err != nil {
		t.Fatalf("ApplyOverlay identity: %v", err)
	}
	if derived.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want seeded info", derived.LogLevel)
	}
	// The one sanctioned canonicalization is the display-default LOG_LEVEL
	// (unset renders "debug"); normalize it, then demand exactness.
	want := base
	want.LogLevel = derived.LogLevel
	if base.LogLevel == "" && derived.LogLevel != "debug" {
		t.Errorf("unset LOG_LEVEL derived = %q, want debug canonicalization", derived.LogLevel)
	}
	if !reflect.DeepEqual(derived, want) {
		t.Errorf("ApplyOverlay identity diverged:\nbase    %+v\nderived %+v", base, derived)
	}
}

// TestApplyOverlaySetDel pins delta semantics: set overrides one knob while
// every other effective knob (including file-seed values) survives, and del
// drops an overlay key back to its seed/default.
func TestApplyOverlaySetDel(t *testing.T) {
	base := applyBase(t, "AUTH_TOKENS=tok-a\nSAFE_MODE=true\nQUEUE_WAIT=45s\n", "")
	set, err := ApplyOverlay(base, map[string]string{"SAFE_MODE": "false", "LOG_LEVEL": "debug"}, nil)
	if err != nil {
		t.Fatalf("ApplyOverlay set: %v", err)
	}
	if set.SafeMode {
		t.Error("SAFE_MODE not applied")
	}
	if set.LogLevel != "debug" {
		t.Errorf("LOG_LEVEL = %q, want debug", set.LogLevel)
	}
	if set.QueueWait.Seconds() != 45 {
		t.Errorf("QUEUE_WAIT = %v, want file-seed 45s preserved", set.QueueWait)
	}
	if len(set.AuthTokens) != 1 || set.AuthTokens[0] != "tok-a" {
		t.Errorf("AuthTokens = %v, want file-seed pool preserved", set.AuthTokens)
	}

	// Del: overlay-provided knob falls back to the seed tier.
	withOverlay, err := ApplyOverlay(base, map[string]string{"QUEUE_WAIT": "10s"}, nil)
	if err != nil {
		t.Fatalf("ApplyOverlay set QUEUE_WAIT: %v", err)
	}
	if withOverlay.QueueWait.Seconds() != 10 {
		t.Fatalf("QUEUE_WAIT = %v, want 10s", withOverlay.QueueWait)
	}
	back, err := ApplyOverlay(withOverlay, nil, []string{"QUEUE_WAIT"})
	if err != nil {
		t.Fatalf("ApplyOverlay del: %v", err)
	}
	// The full effective map (which the del derives from) carries 10s, so
	// del of a key absent from every lower tier falls to the built-in
	// default — exactly like deleting an overlay row for a defaulted knob.
	if back.QueueWait.Seconds() != 30 {
		t.Errorf("QUEUE_WAIT after del = %v, want 30s default", back.QueueWait)
	}
}

// TestApplyOverlayPreservesBlockedFileSeed pins the file-seed carry for
// env-only keys: the skipped file tier cannot supply them, so an
// unrelated mutation must not reset LOG_FILE/SESSION_STATE_FILE/
// HTTP_READ_TIMEOUT/SESSION_PERSIST to defaults. Process env still wins
// when pinned.
func TestApplyOverlayPreservesBlockedFileSeed(t *testing.T) {
	base := applyBase(t,
		"AUTH_TOKENS=tok-a\nLOG_FILE=/tmp/seed.log\nSESSION_STATE_FILE=/tmp/seed-state.json\nHTTP_READ_TIMEOUT=90s\nSESSION_PERSIST=true\n",
		"")
	if base.LogFile != "/tmp/seed.log" || base.SessionStateFile != "/tmp/seed-state.json" {
		t.Fatalf("seed did not land: %+v", base)
	}
	derived, err := ApplyOverlay(base, map[string]string{"SAFE_MODE": "true"}, nil)
	if err != nil {
		t.Fatalf("ApplyOverlay: %v", err)
	}
	if derived.LogFile != "/tmp/seed.log" {
		t.Errorf("LOG_FILE = %q, want file-seed preserved", derived.LogFile)
	}
	if derived.SessionStateFile != "/tmp/seed-state.json" {
		t.Errorf("SESSION_STATE_FILE = %q, want file-seed preserved", derived.SessionStateFile)
	}
	if derived.HTTPReadTimeout.Seconds() != 90 {
		t.Errorf("HTTPReadTimeout = %v, want file-seed 90s preserved", derived.HTTPReadTimeout)
	}
	if !derived.SessionPersist {
		t.Error("SESSION_PERSIST lost the file-seed true")
	}

	t.Run("env-pin-wins", func(t *testing.T) {
		unsetConfigEnv(t)
		t.Setenv("LOG_FILE", "/tmp/env.log")
		derived, err := ApplyOverlay(base, map[string]string{"SAFE_MODE": "true"}, nil)
		if err != nil {
			t.Fatalf("ApplyOverlay: %v", err)
		}
		if derived.LogFile != "/tmp/env.log" {
			t.Errorf("LOG_FILE = %q, want env pin", derived.LogFile)
		}
	})
}

// TestApplyOverlayNeverReadsFiles pins SkipFiles: a .env created AFTER the
// mem snapshot (post-boot edit without restart) is invisible to the
// derivation — the file tier is boot seed only.
func TestApplyOverlayNeverReadsFiles(t *testing.T) {
	unsetConfigEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	base, err := Load("")
	if err != nil {
		t.Fatalf("Load without seed: %v", err)
	}
	// Post-snapshot .env: every value here contradicts the mem snapshot
	// (defaults subbed for seed). The derivation must see none of it.
	if err := os.WriteFile(".env", []byte("SAFE_MODE=false\nQUEUE_WAIT=99s\nDASHBOARD_REQUIRE_LOGIN=false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	derived, err := ApplyOverlay(base, map[string]string{"TRANSIENT_RETRIES": "3"}, nil)
	if err != nil {
		t.Fatalf("ApplyOverlay: %v", err)
	}
	if !derived.SafeMode {
		t.Error("derivation re-read a post-snapshot .env (SAFE_MODE leaked in)")
	}
	if derived.QueueWait.Seconds() != 30 {
		t.Errorf("QUEUE_WAIT = %v, want default 30s (post-snapshot .env leaked in)", derived.QueueWait)
	}
	if !derived.DashboardRequireLogin {
		t.Error("derivation re-read a post-snapshot .env (DASHBOARD_REQUIRE_LOGIN leaked in)")
	}
	if derived.TransientRetries != 3 {
		t.Errorf("TransientRetries = %d, want 3", derived.TransientRetries)
	}
	if derived.EnvFile != base.EnvFile {
		t.Errorf("EnvFile = %q, want carried %q", derived.EnvFile, base.EnvFile)
	}
}

// TestApplyOverlayRejectsBadValues pins validation on the derivation path:
// a bad duration/enum fails here (400 upstream) instead of persisting a row
// that can never apply.
func TestApplyOverlayRejectsBadValues(t *testing.T) {
	base := applyBase(t, "AUTH_TOKENS=tok-a\n", "")
	for key, val := range map[string]string{
		"QUEUE_WAIT":        "forever",
		"LOG_LEVEL":         "bogus",
		"RATE_LIMIT_PER_IP": "-1",
	} {
		if _, err := ApplyOverlay(base, map[string]string{key: val}, nil); err == nil {
			t.Errorf("ApplyOverlay(%s=%q) = nil error, want rejection", key, val)
		}
	}
}

// TestApplyOverlayEmptyPoolPresence pins the AUTH_TOKENS presence round
// trip: an explicit empty pool stays an explicit empty pool (discovery
// stays suppressed), never a missing tier.
func TestApplyOverlayEmptyPoolPresence(t *testing.T) {
	base := applyBase(t, "AUTH_TOKENS=\n", "")
	if len(base.AuthTokens) != 0 {
		t.Fatalf("seed pool = %v, want empty", base.AuthTokens)
	}
	derived, err := ApplyOverlay(base, map[string]string{"SAFE_MODE": "true"}, nil)
	if err != nil {
		t.Fatalf("ApplyOverlay: %v", err)
	}
	if len(derived.AuthTokens) != 0 {
		t.Errorf("derived pool = %v, want empty preserved", derived.AuthTokens)
	}
}
