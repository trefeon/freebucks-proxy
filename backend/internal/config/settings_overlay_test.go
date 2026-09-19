package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSettingsOverlayPrecedence pins ADR-0019 precedence on one live knob
// (LOG_LEVEL, default "info"): file < db overlay < process env.
func TestSettingsOverlayPrecedence(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte("LOG_LEVEL=warn\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// File only.
	cfg, err := LoadOpts("", LoadOptions{})
	if err != nil {
		t.Fatalf("LoadOpts: %v", err)
	}
	if cfg.LogLevel != "warn" {
		t.Fatalf("file-only LogLevel = %q, want warn", cfg.LogLevel)
	}

	// DB overlay beats the file.
	cfg, err = LoadOpts("", LoadOptions{Overlay: map[string]string{"LOG_LEVEL": "debug"}})
	if err != nil {
		t.Fatalf("LoadOpts overlay: %v", err)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("overlay LogLevel = %q, want debug", cfg.LogLevel)
	}

	// Explicit process env beats the overlay.
	t.Setenv("LOG_LEVEL", "error")
	cfg, err = LoadOpts("", LoadOptions{Overlay: map[string]string{"LOG_LEVEL": "debug"}})
	if err != nil {
		t.Fatalf("LoadOpts env-wins: %v", err)
	}
	if cfg.LogLevel != "error" {
		t.Fatalf("env LogLevel = %q, want error", cfg.LogLevel)
	}
}

// TestSettingsOverlayIntBool pins typed overlay parsing (int + bool knobs).
func TestSettingsOverlayIntBool(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	cfg, err := LoadOpts("", LoadOptions{Overlay: map[string]string{
		"RATE_LIMIT_BURST": "7",
		"SAFE_MODE":        "false",
	}})
	if err != nil {
		t.Fatalf("LoadOpts: %v", err)
	}
	if cfg.RateLimitBurst != 7 {
		t.Errorf("RateLimitBurst = %d, want 7", cfg.RateLimitBurst)
	}
	if cfg.SafeMode {
		t.Error("SafeMode = true, want false (overlay)")
	}
}

// TestSettingsOverlaySecretsApply: since the env-to-DB migration every
// catalog key is overlay-addressable, secrets included (the DB file holds
// them at mode 0600) — a migrated row takes effect when the environment
// leaves the key unset. Fake values only, never real credentials.
func TestSettingsOverlaySecretsApply(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	cfg, err := LoadOpts("", LoadOptions{Overlay: map[string]string{
		"AUTH_TOKENS":       "fb-test-fake-token-1",
		"ADMIN_TOKEN":       "fb-test-fake-admin-1",
		"API_KEYS":          "fb-test-fake-client-1",
		"WEBHOOK_URL":       "https://example.invalid/hook",
		"UPSTREAM_BASE_URL": "https://example.invalid",
		"SAFE_MODE":         "false",
	}})
	if err != nil {
		t.Fatalf("LoadOpts: %v", err)
	}
	if len(cfg.AuthTokens) != 1 || cfg.AuthTokens[0] != "fb-test-fake-token-1" {
		t.Errorf("overlay AUTH_TOKENS did not apply: %v", cfg.AuthTokens)
	}
	if cfg.AdminToken != "fb-test-fake-admin-1" {
		t.Error("overlay ADMIN_TOKEN did not apply")
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0] != "fb-test-fake-client-1" {
		t.Errorf("overlay API_KEYS did not apply: %v", cfg.APIKeys)
	}
	if cfg.WebhookURL != "https://example.invalid/hook" {
		t.Errorf("overlay WEBHOOK_URL = %q, want the overlay value", cfg.WebhookURL)
	}
	if cfg.UpstreamBaseURL != "https://example.invalid" {
		t.Errorf("overlay UPSTREAM_BASE_URL = %q, want the overlay value", cfg.UpstreamBaseURL)
	}
	if cfg.SafeMode {
		t.Error("SafeMode = true, want false (allowed overlay must still apply)")
	}
}

// TestSettingsOverlaySecretsEnvWins: explicit process env still beats a
// migrated DB row for every formerly-blocked key — precedence never silently
// changes for existing users.
func TestSettingsOverlaySecretsEnvWins(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	t.Setenv("AUTH_TOKENS", "fb-test-fake-env-token-1")
	t.Setenv("ADMIN_TOKEN", "fb-test-fake-env-admin-1")
	t.Setenv("API_KEYS", "fb-test-fake-env-client-1")
	t.Setenv("WEBHOOK_URL", "https://env.invalid/hook")
	t.Setenv("UPSTREAM_BASE_URL", "https://env.invalid")
	cfg, err := LoadOpts("", LoadOptions{Overlay: map[string]string{
		"AUTH_TOKENS":       "fb-test-fake-db-token-1",
		"ADMIN_TOKEN":       "fb-test-fake-db-admin-1",
		"API_KEYS":          "fb-test-fake-db-client-1",
		"WEBHOOK_URL":       "https://db.invalid/hook",
		"UPSTREAM_BASE_URL": "https://db.invalid",
	}})
	if err != nil {
		t.Fatalf("LoadOpts env-wins: %v", err)
	}
	if len(cfg.AuthTokens) != 1 || cfg.AuthTokens[0] != "fb-test-fake-env-token-1" {
		t.Errorf("AUTH_TOKENS = %v, want the env value", cfg.AuthTokens)
	}
	if cfg.AdminToken != "fb-test-fake-env-admin-1" {
		t.Error("ADMIN_TOKEN lost to the overlay, want env to win")
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0] != "fb-test-fake-env-client-1" {
		t.Errorf("API_KEYS = %v, want the env value", cfg.APIKeys)
	}
	if cfg.WebhookURL != "https://env.invalid/hook" {
		t.Errorf("WEBHOOK_URL = %q, want the env value", cfg.WebhookURL)
	}
	if cfg.UpstreamBaseURL != "https://env.invalid" {
		t.Errorf("UPSTREAM_BASE_URL = %q, want the env value", cfg.UpstreamBaseURL)
	}
}

// TestOverlayCoversCatalog: every non-blocked catalog key must be
// overlay-addressable (applyMappedValues is the single shared key list, so
// this guards the filter in applySettingsOverlay, not the list itself).
// The env-only keys (SettingsBlockedKeys) are skipped by that filter;
// AUTH_TOKENS rides the overlay with presence semantics next to
// applyMappedValues.
func TestOverlayCoversCatalog(t *testing.T) {
	for _, def := range Catalog() {
		if IsSettingsBlocked(def.Key) {
			continue
		}
		raw := defaultRawConfig()
		get := func(name string) string {
			if name == def.Key {
				return "true"
			}
			return ""
		}
		// Must not panic on any catalog key; filter membership is
		// asserted via OverlayFromRows below.
		applyMappedValues(&raw, get)
	}
	rows := map[string]string{"config:SAFE_MODE": "false", "theme": "dark"}
	ov := OverlayFromRows(rows)
	if ov["SAFE_MODE"] != "false" {
		t.Errorf("OverlayFromRows dropped config:SAFE_MODE: %v", ov)
	}
	if _, ok := ov["THEME"]; ok {
		t.Errorf("OverlayFromRows kept non-config row: %v", ov)
	}
	kept := OverlayFromRows(map[string]string{"config:AUTH_TOKENS": "x"})
	if kept["AUTH_TOKENS"] != "x" {
		t.Errorf("OverlayFromRows dropped unblocked AUTH_TOKENS: %v", kept)
	}
	unknown := OverlayFromRows(map[string]string{"config:NOPE_NOT_A_KEY": "x"})
	if len(unknown) != 0 {
		t.Errorf("OverlayFromRows kept unknown key: %v", unknown)
	}
}

// TestValidateSettingValue pins the POST gate: unknown keys, unparseable
// typed values, and env-only keys (SettingsBlockedKeys, even well-formed)
// reject; writable knobs accept. Secrets are storable since
// the env-to-DB migration (the DB file holds them at mode 0600); AUTH_TOKENS
// and ADMIN_TOKEN take the dedicated-endpoint path in the settings POST
// handler, but Validate itself accepts them so migrated rows read back.
func TestValidateSettingValue(t *testing.T) {
	for key, value := range map[string]string{
		"LOG_LEVEL":         "debug",
		"SAFE_MODE":         "false",
		"RATE_LIMIT_BURST":  "30",
		"RATE_LIMIT_PER_IP": "2.5",
		"MODELS_ALLOW":      "deepseek/deepseek-v4-flash",
		"AUTH_TOKENS":       "fb-test-fake-token-1",
		"ADMIN_TOKEN":       "fb-test-fake-admin-1",
		"API_KEYS":          "fb-test-fake-client-1",
		"WEBHOOK_URL":       "https://example.invalid/hook",
		"UPSTREAM_BASE_URL": "https://example.invalid",
	} {
		if err := ValidateSettingValue(key, value); err != nil {
			t.Errorf("ValidateSettingValue(%s,%s) = %v, want nil", key, value, err)
		}
	}
	for key, value := range map[string]string{
		"NOPE_NOT_A_KEY":    "x",
		"DB_PATH":           "/tmp/x.db",
		"SAFE_MODE":         "banana",
		"RATE_LIMIT_BURST":  "lots",
		"RATE_LIMIT_PER_IP": "fast",
		"LOG_LEVEL":         "",
		"":                  "x",
		// Env-only keys (data-architecture decision) never store: the
		// settings-block gate rejects even well-formed values with an
		// env/.env pointer so POST never persists an inert row.
		"SESSION_STATE_FILE":  "custom-state.json",
		"SESSION_PERSIST":     "false",
		"LOG_FILE":            "proxy.log",
		"HTTP_READ_TIMEOUT":   "90s",
		"AUTO_DISCOVER_TOKEN": "false",
	} {
		if err := ValidateSettingValue(key, value); err == nil {
			t.Errorf("ValidateSettingValue(%q,%q) accepted, want an error", key, value)
		}
	}
	// The block message points at the environment/.env, mirroring the
	// ADMIN_FORCE_SECURE_COOKIES POST pointer.
	for _, key := range []string{"SESSION_STATE_FILE", "SESSION_PERSIST", "LOG_FILE", "HTTP_READ_TIMEOUT", "AUTO_DISCOVER_TOKEN"} {
		err := ValidateSettingValue(key, "false")
		if err == nil {
			t.Errorf("ValidateSettingValue(%s) accepted, want the env-only block", key)
			continue
		}
		if !strings.Contains(err.Error(), ".env") {
			t.Errorf("ValidateSettingValue(%s) = %v, want an environment/.env pointer", key, err)
		}
	}
}

// TestValidateSettingValueDurationGate pins the duration knobs to the same
// pre-DB gate the settings handler calls: an unparseable value is rejected
// here, and the zero-tolerant values the loader documents (a floor for
// QUEUE_WAIT, "disabled" for the idle timeouts) stay accepted — the gate
// judges parseability only, never the semantics.
func TestValidateSettingValueDurationGate(t *testing.T) {
	for _, value := range []string{"30s", "1m30s", "0", "0s", "-5s", "1500ms"} {
		if err := ValidateSettingValue("QUEUE_WAIT", value); err != nil {
			t.Errorf("ValidateSettingValue(QUEUE_WAIT,%q) = %v, want nil", value, err)
		}
	}
	for _, value := range []string{"bogus", "5", "30 s", "1m30", "always"} {
		if err := ValidateSettingValue("QUEUE_WAIT", value); err == nil {
			t.Errorf("ValidateSettingValue(QUEUE_WAIT,%q) accepted, want a parse error", value)
		} else if !strings.Contains(err.Error(), "Go duration") {
			t.Errorf("ValidateSettingValue(QUEUE_WAIT,%q) = %v, want a Go duration message", value, err)
		}
	}
	// The gate is catalog-driven, so every duration knob behaves the same.
	for key := range durationSettingKeys {
		if err := ValidateSettingValue(key, "not-a-duration"); err == nil {
			t.Errorf("ValidateSettingValue(%s,not-a-duration) accepted, want a parse error", key)
		}
	}
}

// TestValidateSettingValueLoadAgreement pins the gate/Load contract behind
// POST: for every value Load rejects, the pre-transaction gate must reject
// too — a value the gate waves through but Load refuses costs the operator
// a 400 on something the gate just blessed (or a stored row the running
// config cannot use). Where the loader fails loudly (durations, selects,
// range-checked numbers) agreement runs both ways. Unparseable ints/floats
// are silently skipped by the override helpers (the env tier keeps its
// historic ignore), so those pin gate-side rejection only.
func TestValidateSettingValueLoadAgreement(t *testing.T) {
	clearEnv(t)
	agreeReject := map[string][]string{
		"RATE_LIMIT_BURST":     {"-1"},
		"QUEUE_DEPTH":          {"-1"},
		"MAX_SPILL_ACCOUNTS":   {"-1"},
		"TRANSIENT_RETRIES":    {"-1"},
		"MATURITY_TARGET_DAYS": {"0", "-1", "29"},
		"RATE_LIMIT_PER_IP":    {"-1", "NaN", "Inf"},
		"LOG_LEVEL":            {"verbose"},
		"LOG_FORMAT":           {"xml", "JSON"},
		"COST_MODE":            {"paid", "FREE"},
		"TLS_FINGERPRINT":      {"netscape"},
		"ROTATION_INTERVAL":    {"0s"},
		"REQUEST_TIMEOUT":      {"0s"},
		"SESSION_CALL_TIMEOUT": {"0s"},
		"REGISTRY_REFRESH":     {"0s"},
		"REQUEST_JITTER":       {"-1s"},
	}
	for key, values := range agreeReject {
		for _, value := range values {
			if err := ValidateSettingValue(key, value); err == nil {
				t.Errorf("gate accepted %s=%q, want rejection (Load refuses it)", key, value)
			}
			if _, err := LoadOpts("", LoadOptions{Overlay: map[string]string{key: value}}); err == nil {
				t.Errorf("Load accepted %s=%q, want rejection (the gate refuses it)", key, value)
			}
		}
	}
	// Every value here must pass BOTH, including the case-folded selects
	// and the loader-folded durations (QUEUE_WAIT owns its own parity
	// test; IDLE_ROTATION_TIMEOUT pins the store-side agreement here).
	// SLOTS_PER_ACCOUNT=-1 rides the loader floor to 0 (unlimited) — the
	// one int negative Load takes, so the gate takes it too.
	agreeAccept := map[string][]string{
		"RATE_LIMIT_BURST":          {"0", "30"},
		"QUEUE_DEPTH":               {"0", "16"},
		"MAX_SPILL_ACCOUNTS":        {"0"},
		"SLOTS_PER_ACCOUNT":         {"-1", "0", "2"},
		"TRANSIENT_RETRIES":         {"0", "3"},
		"MATURITY_TARGET_DAYS":      {"1", "7", "28"},
		"RATE_LIMIT_PER_IP":         {"0", "2.5"},
		"LOG_LEVEL":                 {"debug", "DEBUG"},
		"LOG_FORMAT":                {"text", "json"},
		"COST_MODE":                 {"free"},
		"TLS_FINGERPRINT":           {"auto", "Chrome126"},
		"ROTATION_INTERVAL":         {"6h"},
		"REQUEST_TIMEOUT":           {"15m"},
		"SESSION_CALL_TIMEOUT":      {"30s"},
		"REGISTRY_REFRESH":          {"6h"},
		"REQUEST_JITTER":            {"0s", "200ms"},
		"IDLE_ROTATION_TIMEOUT":     {"0", "0s", "-5s", "30m"},
		"BRIDGE_IDLE_EVICT":         {"0s", "72h"},
		"RUN_FINISH_INLINE_TIMEOUT": {"0s", "250ms"},
	}
	for key, values := range agreeAccept {
		for _, value := range values {
			if err := ValidateSettingValue(key, value); err != nil {
				t.Errorf("gate rejected %s=%q: %v, want acceptance (Load takes it)", key, value, err)
			}
			if _, err := LoadOpts("", LoadOptions{Overlay: map[string]string{key: value}}); err != nil {
				t.Errorf("Load rejected %s=%q: %v, want acceptance (the gate takes it)", key, value, err)
			}
		}
	}
	// Unparseable ints/floats never reach the loader (the override helpers
	// skip them): the gate still 400s them so POST never stores a no-op.
	for key, value := range map[string]string{
		"RATE_LIMIT_BURST":  "lots",
		"RATE_LIMIT_PER_IP": "fast",
	} {
		if err := ValidateSettingValue(key, value); err == nil {
			t.Errorf("gate accepted %s=%q, want a parse rejection", key, value)
		}
	}
}

// TestDurationSettingKeysMatchCatalog keeps the duration gate honest: every
// name in it must be a real catalog key whose documented default parses, so a
// renamed or dropped knob cannot leave a dead entry (or a live duration knob
// silently outside the gate) behind.
func TestDurationSettingKeysMatchCatalog(t *testing.T) {
	defs := map[string]KeyDef{}
	for _, def := range Catalog() {
		defs[def.Key] = def
	}
	for key := range durationSettingKeys {
		def, ok := defs[key]
		if !ok {
			t.Errorf("durationSettingKeys lists %s, which is not a catalog key", key)
			continue
		}
		if def.Kind != "text" && def.Kind != "select" {
			t.Errorf("%s is kind %q in the catalog, want a value-carrying control", key, def.Kind)
		}
		if _, err := time.ParseDuration(def.Default); err != nil {
			t.Errorf("%s default %q does not parse as a Go duration: %v", key, def.Default, err)
		}
	}
}

// TestSettingSources pins the per-key source tags across all four tiers.
func TestSettingSources(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte("LOG_LEVEL=warn\nSAFE_MODE=false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	overlay := map[string]string{"LOG_LEVEL": "debug", "BRIDGE_ENABLED": "false"}
	t.Setenv("SAFE_MODE", "true")

	sources := SettingSources("", overlay)
	if sources["LOG_LEVEL"] != "db" {
		t.Errorf("LOG_LEVEL source = %q, want db", sources["LOG_LEVEL"])
	}
	if sources["SAFE_MODE"] != "env" {
		t.Errorf("SAFE_MODE source = %q, want env", sources["SAFE_MODE"])
	}
	if sources["BRIDGE_ENABLED"] != "db" {
		t.Errorf("BRIDGE_ENABLED source = %q, want db", sources["BRIDGE_ENABLED"])
	}
	if sources["RATE_LIMIT_BURST"] != "default" {
		t.Errorf("RATE_LIMIT_BURST source = %q, want default", sources["RATE_LIMIT_BURST"])
	}

	// Without overlay or env, the .env key reports file.
	sources = SettingSources("", nil)
	if sources["LOG_LEVEL"] != "file" {
		t.Errorf("LOG_LEVEL source = %q, want file", sources["LOG_LEVEL"])
	}
}

// TestSettingSourcesJSONFile: a JSON -config key counts as the file tier.
func TestSettingSourcesJSONFile(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"LOG_LEVEL":"debug"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sources := SettingSources(path, nil)
	if sources["LOG_LEVEL"] != "file" {
		t.Errorf("LOG_LEVEL source = %q, want file (JSON)", sources["LOG_LEVEL"])
	}
	if sources["SAFE_MODE"] != "default" {
		t.Errorf("SAFE_MODE source = %q, want default", sources["SAFE_MODE"])
	}
}

// TestLogAndListenKeysAreRestartOnly pins the review finding that the logger
// (LOG_LEVEL/LOG_FORMAT/LOG_FILE) and the listener socket
// (LISTEN_ADDR) are never touched by a reload: applyReloadedConfig fans out
// to cfg/registry/pool/rate-limiter only, so the settings POST must report
// setting_restart_only for these keys instead of claiming a live apply.
// The server-side half of the parity (restartOnlyConfigKeys) is pinned by
// TestConfigCatalogRestartOnlyMatchesServer in the server package.
func TestLogAndListenKeysAreRestartOnly(t *testing.T) {
	for _, key := range []string{"LOG_LEVEL", "LOG_FORMAT", "LOG_FILE", "LISTEN_ADDR"} {
		def, ok := LookupSetting(key)
		if !ok {
			t.Fatalf("LookupSetting(%s) missing from catalog", key)
		}
		if !def.RestartOnly {
			t.Errorf("%s RestartOnly = false, want true (reload never reconfigures the logger or re-binds the socket)", key)
		}
	}
}

// TestAutoDiscoverTokenOverlay: AUTO_DISCOVER_TOKEN is env-only per the
// data-architecture decision. The overlay row is inert: IsSettingsBlocked
// holds, Validate rejects with an env/.env pointer, OverlayFromRows drops
// the row, and LoadOpts ignores a hand-built overlay entry (effective
// falls back to env-then-default-true). The overlay-false-suppresses-
// discovery behavior change is intended.
func TestAutoDiscoverTokenOverlay(t *testing.T) {
	if !IsSettingsBlocked("AUTO_DISCOVER_TOKEN") {
		t.Error("IsSettingsBlocked(AUTO_DISCOVER_TOKEN) = false, want true (env-only)")
	}
	if err := ValidateSettingValue("AUTO_DISCOVER_TOKEN", "false"); err == nil {
		t.Error("ValidateSettingValue(AUTO_DISCOVER_TOKEN) accepted, want the env-only block")
	}
	if ov := OverlayFromRows(map[string]string{"config:AUTO_DISCOVER_TOKEN": "false"}); len(ov) != 0 {
		t.Errorf("OverlayFromRows kept blocked AUTO_DISCOVER_TOKEN: %v", ov)
	}
	clearEnv(t)
	t.Chdir(t.TempDir())
	// clearEnv pins AUTO_DISCOVER_TOKEN=false in the environment; unset it
	// so the load below resolves the default tier with an inert overlay.
	if err := os.Unsetenv("AUTO_DISCOVER_TOKEN"); err != nil {
		t.Fatal(err)
	}
	fakeDiscover := func() (string, string, string, bool) { return "fb-test-fake-discovered-1", "", "", true }
	// Inert overlay "false" with no env: default true stands, discovery fires.
	cfg, err := LoadOpts("", LoadOptions{DiscoverCLIToken: fakeDiscover, Overlay: map[string]string{"AUTO_DISCOVER_TOKEN": "false"}})
	if err != nil {
		t.Fatalf("LoadOpts: %v", err)
	}
	if !cfg.AutoDiscoverToken {
		t.Error("AutoDiscoverToken = false, want true (overlay row is inert, default stands)")
	}
	if len(cfg.AuthTokens) != 1 || cfg.AuthTokens[0] != "fb-test-fake-discovered-1" {
		t.Errorf("AuthTokens = %v, want the discovered token (inert overlay enables discovery)", cfg.AuthTokens)
	}
	// Env "false" still suppresses discovery (and records false).
	t.Setenv("AUTO_DISCOVER_TOKEN", "false")
	cfg, err = LoadOpts("", LoadOptions{DiscoverCLIToken: fakeDiscover, Overlay: map[string]string{"AUTO_DISCOVER_TOKEN": "true"}})
	if err != nil {
		t.Fatalf("LoadOpts env: %v", err)
	}
	if cfg.AutoDiscoverToken {
		t.Error("AutoDiscoverToken = true, want false (env decides)")
	}
	if len(cfg.AuthTokens) != 0 {
		t.Errorf("AuthTokens = %v, want empty (env false suppresses discovery)", cfg.AuthTokens)
	}
}

// TestEnvOnlyKeysAreInert pins the data-architecture env-only gate for all
// five keys: blocked from the overlay, dropped from row dumps, ignored by
// the loader (defaults stand), and never reported as the db source tier.
func TestEnvOnlyKeysAreInert(t *testing.T) {
	envOnly := []string{"SESSION_STATE_FILE", "SESSION_PERSIST", "LOG_FILE", "HTTP_READ_TIMEOUT", "AUTO_DISCOVER_TOKEN"}
	for _, key := range envOnly {
		if !IsSettingsBlocked(key) {
			t.Errorf("IsSettingsBlocked(%s) = false, want true (env-only)", key)
		}
	}
	rows := map[string]string{}
	for _, key := range envOnly {
		rows[OverlayRowKey(key)] = "false"
	}
	if ov := OverlayFromRows(rows); len(ov) != 0 {
		t.Errorf("OverlayFromRows kept blocked rows: %v", ov)
	}
	clearEnv(t)
	t.Chdir(t.TempDir())
	// clearEnv pins AUTO_DISCOVER_TOKEN=false; unset it so the default
	// tier resolves below.
	if err := os.Unsetenv("AUTO_DISCOVER_TOKEN"); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadOpts("", LoadOptions{Overlay: map[string]string{
		"SESSION_PERSIST":     "false",
		"SESSION_STATE_FILE":  "custom-state.json",
		"LOG_FILE":            "proxy.log",
		"HTTP_READ_TIMEOUT":   "300s",
		"AUTO_DISCOVER_TOKEN": "false",
	}})
	if err != nil {
		t.Fatalf("LoadOpts with inert overlay: %v", err)
	}
	if !cfg.SessionPersist {
		t.Error("SessionPersist = false, want true (blocked overlay ignored, default stands)")
	}
	if cfg.SessionStateFile != ".freebuff-session-state.json" {
		t.Errorf("SessionStateFile = %q, want the default (blocked overlay ignored)", cfg.SessionStateFile)
	}
	if cfg.HTTPReadTimeout != 60*time.Second {
		t.Errorf("HTTPReadTimeout = %v, want the 60s default (blocked overlay ignored)", cfg.HTTPReadTimeout)
	}
	if !cfg.AutoDiscoverToken {
		t.Error("AutoDiscoverToken = false, want true (blocked overlay ignored, default stands)")
	}
	// Source tiers stay truthful: a hand-built overlay carrying blocked
	// keys never reports db.
	sources := SettingSources("", map[string]string{
		"SESSION_PERSIST":     "false",
		"SESSION_STATE_FILE":  "custom-state.json",
		"LOG_FILE":            "proxy.log",
		"HTTP_READ_TIMEOUT":   "300s",
		"AUTO_DISCOVER_TOKEN": "false",
	})
	for _, key := range envOnly {
		if sources[key] == "db" {
			t.Errorf("%s source = db for a blocked overlay row, want env/file/default", key)
		}
	}
}

// TestSettingSourcesDotenvUserIDAlias pins the .env USER_ID alias to the
// file tier for ACTING_USER_ID: the loader resolves it via
// overrideStringAlias on the dotenv tier, so the source tag must agree
// (the JSON and env tiers already attributed the alias). An empty alias
// value leaves the default in force like any other empty .env line.
func TestSettingSourcesDotenvUserIDAlias(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte("AUTH_TOKENS=tok-1\nUSER_ID=user-dotenv-legacy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if sources := SettingSources("", nil); sources["ACTING_USER_ID"] != "file" {
		t.Errorf("ACTING_USER_ID source = %q, want file (.env USER_ID alias)", sources["ACTING_USER_ID"])
	}
}

// TestOverlayFromRowsDropsMalformed pins the documented contract that
// malformed rows are skipped: a non-empty value that fails
// ValidateSettingValue could never take effect (it would fail the POST
// gate), so a tampered or stale row must not poison the load. Empty values
// are kept as no-op pins instead (every override helper skips blanks, and
// AUTH_TOKENS presence is the bridge-mode pin).
func TestOverlayFromRowsDropsMalformed(t *testing.T) {
	ov := OverlayFromRows(map[string]string{
		"config:SAFE_MODE":         "false",
		"config:LOG_LEVEL":         "debug",
		"config:RATE_LIMIT_BURST":  "30",
		"config:RATE_LIMIT_PER_IP": "2.5",
		"config:NOPE_NOT_A_KEY":    "x",
		"theme":                    "dark",
	})
	for _, key := range []string{"SAFE_MODE", "LOG_LEVEL", "RATE_LIMIT_BURST", "RATE_LIMIT_PER_IP"} {
		if _, ok := ov[key]; !ok {
			t.Errorf("OverlayFromRows dropped valid row %s: %v", key, ov)
		}
	}
	if len(ov) != 4 {
		t.Errorf("OverlayFromRows = %v, want exactly the 4 valid rows", ov)
	}
	malformed := OverlayFromRows(map[string]string{
		"config:SAFE_MODE":         "banana",
		"config:RATE_LIMIT_BURST":  "lots",
		"config:RATE_LIMIT_PER_IP": "fast",
		"config:LOG_LEVEL2":        "debug",
	})
	if len(malformed) != 0 {
		t.Errorf("OverlayFromRows kept malformed rows: %v", malformed)
	}
	pins := OverlayFromRows(map[string]string{
		"config:LOG_LEVEL":   "",
		"config:AUTH_TOKENS": "",
	})
	if v, ok := pins["LOG_LEVEL"]; !ok || v != "" {
		t.Errorf("OverlayFromRows dropped the empty LOG_LEVEL pin: %v", pins)
	}
	if _, ok := pins["AUTH_TOKENS"]; !ok {
		t.Errorf("OverlayFromRows dropped the empty AUTH_TOKENS bridge pin: %v", pins)
	}
}

// TestSettingSourcesJSONEmptyIsDefault pins value-aware JSON attribution: an
// empty JSON value (null, "" or whitespace) leaves the default in force and
// reports "default" like an empty .env line does — never "file". Explicit
// zeros, falses, and empty arrays are real values and still report "file".
func TestSettingSourcesJSONEmptyIsDefault(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "config.json")
	body := `{"LOG_LEVEL":"","SAFE_MODE":"   ","RATE_LIMIT_BURST":null,` +
		`"RATE_LIMIT_PER_IP":"2.5","QUEUE_DEPTH":0,"MODELS_HIDE_UNAVAILABLE":false,` +
		`"ACTING_USER_ID":"","USER_ID":"user-json-legacy"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	sources := SettingSources(path, nil)
	for key := range map[string]bool{"LOG_LEVEL": true, "SAFE_MODE": true, "RATE_LIMIT_BURST": true} {
		if sources[key] != "default" {
			t.Errorf("%s source = %q, want default (empty JSON value)", key, sources[key])
		}
	}
	for key := range map[string]bool{"RATE_LIMIT_PER_IP": true, "QUEUE_DEPTH": true, "MODELS_HIDE_UNAVAILABLE": true} {
		if sources[key] != "file" {
			t.Errorf("%s source = %q, want file (explicit JSON value)", key, sources[key])
		}
	}
	// The legacy USER_ID alias attributes to ACTING_USER_ID even when the
	// primary JSON key is empty (the loader's LegacyActingUserID merge).
	if sources["ACTING_USER_ID"] != "file" {
		t.Errorf("ACTING_USER_ID source = %q, want file (JSON USER_ID alias)", sources["ACTING_USER_ID"])
	}
}
