package clicreds_test

import (
	"freebucks-proxy/backend/internal/clicreds"
	"os"
	"path/filepath"
	"testing"
)

// setFakeHome points os.UserHomeDir at a temp dir for the test (Windows uses
// USERPROFILE; the CLI login paths use $HOME/.config/... on the wire, so both
// are set).
func setFakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	return home
}

// clearDiscoveryEnv pins the discovery env to the plain default: no
// $FREEBUFF_CONFIG_DIR override, no $NEXT_PUBLIC_CB_ENVIRONMENT suffix.
func clearDiscoveryEnv(t *testing.T) {
	t.Helper()
	t.Setenv("FREEBUFF_CONFIG_DIR", "")
	t.Setenv("NEXT_PUBLIC_CB_ENVIRONMENT", "")
}

func writeCreds(t *testing.T, home, rel, body string) {
	t.Helper()
	dir := filepath.Join(home, filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverTokenManicode(t *testing.T) {
	clearDiscoveryEnv(t)
	home := setFakeHome(t)
	writeCreds(t, home, ".config/manicode", `{"default": {"authToken": "cb_manicode", "email": "dev@example.com"}}`)

	token, email, path, ok := clicreds.DiscoverToken()
	if !ok {
		t.Fatal("DiscoverToken = not found, want cb_manicode")
	}
	if token != "cb_manicode" {
		t.Errorf("token = %q, want cb_manicode", token)
	}
	if email != "dev@example.com" {
		t.Errorf("email = %q, want dev@example.com", email)
	}
	if want := filepath.Join(home, ".config", "manicode", "credentials.json"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

func TestDiscoverTokenEmpty(t *testing.T) {
	clearDiscoveryEnv(t)
	home := setFakeHome(t)
	_ = home
	if _, _, _, ok := clicreds.DiscoverToken(); ok {
		t.Fatal("DiscoverToken = found with no credentials file, want not found")
	}
}

func TestDiscoverTokenStripsBOM(t *testing.T) {
	clearDiscoveryEnv(t)
	home := setFakeHome(t)
	writeCreds(t, home, ".config/manicode", "\xef\xbb\xbf"+`{"default": {"authToken": "cb_bom"}}`)

	token, _, _, ok := clicreds.DiscoverToken()
	if !ok || token != "cb_bom" {
		t.Fatalf("DiscoverToken = (%q, %v), want cb_bom (BOM must not break parsing)", token, ok)
	}
}

// TestDiscoverTokenDefaultOnly pins the upstream narrowing (auth.ts
// userFromJson defaults to profile "default"): any other profile is ignored,
// whether "default" is present (default wins) or absent (no discovery).
func TestDiscoverTokenDefaultOnly(t *testing.T) {
	clearDiscoveryEnv(t)
	home := setFakeHome(t)
	writeCreds(t, home, ".config/manicode", `{"freebuff": {"authToken": "cb_other"}, "codebuff": {"authToken": "cb_other2"}, "default": {"authToken": "cb_default"}}`)

	token, _, _, ok := clicreds.DiscoverToken()
	if !ok || token != "cb_default" {
		t.Fatalf("DiscoverToken = (%q, %v), want cb_default (other profiles ignored)", token, ok)
	}

	home2 := setFakeHome(t)
	writeCreds(t, home2, ".config/manicode", `{"freebuff": {"authToken": "cb_other"}, "codebuff": {"authToken": "cb_other2"}}`)
	if _, _, _, ok := clicreds.DiscoverToken(); ok {
		t.Fatal("DiscoverToken = found with only non-default profiles, want not found")
	}
}

// TestDiscoverTokenIgnoresSessionTokenField pins the upstream narrowing
// (getAuthTokenDetails reads field "authToken" only): a default profile
// carrying only sessionToken/token yields no discovery.
func TestDiscoverTokenIgnoresSessionTokenField(t *testing.T) {
	clearDiscoveryEnv(t)
	home := setFakeHome(t)
	writeCreds(t, home, ".config/manicode", `{"default": {"id": "uuid-123", "sessionToken": "cb_session_token", "token": "cb_token", "email": "dev@example.com"}}`)

	if _, _, _, ok := clicreds.DiscoverToken(); ok {
		t.Fatal("DiscoverToken = found with only sessionToken/token fields, want not found")
	}
}

// TestDiscoverTokenRespectsFreebuffConfigDir pins the absolute-override arm
// of upstream config-dir.ts: discovery reads only the override dir.
func TestDiscoverTokenRespectsFreebuffConfigDir(t *testing.T) {
	clearDiscoveryEnv(t)
	setFakeHome(t)
	dir := t.TempDir()
	t.Setenv("FREEBUFF_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{"default": {"authToken": "cb_override"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	token, _, path, ok := clicreds.DiscoverToken()
	if !ok || token != "cb_override" {
		t.Fatalf("DiscoverToken = (%q, %v), want cb_override", token, ok)
	}
	if want := filepath.Join(dir, "credentials.json"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

// TestDiscoverTokenRejectsRelativeOverride pins the upstream guard (config-dir.ts
// throws on a relative FREEBUFF_CONFIG_DIR): a relative override disables
// discovery entirely instead of falling back to the home dirs.
func TestDiscoverTokenRejectsRelativeOverride(t *testing.T) {
	clearDiscoveryEnv(t)
	home := setFakeHome(t)
	writeCreds(t, home, ".config/manicode", `{"default": {"authToken": "cb_manicode"}}`)
	t.Setenv("FREEBUFF_CONFIG_DIR", "relative/path")

	if _, _, _, ok := clicreds.DiscoverToken(); ok {
		t.Fatal("DiscoverToken = found under a relative FREEBUFF_CONFIG_DIR, want not found")
	}
}

// TestDiscoverTokenEnvSuffixDir pins the dev-stack arm of upstream
// config-dir.ts: with $NEXT_PUBLIC_CB_ENVIRONMENT set and != "prod" only
// the suffixed dir is read.
func TestDiscoverTokenEnvSuffixDir(t *testing.T) {
	clearDiscoveryEnv(t)
	home := setFakeHome(t)
	t.Setenv("NEXT_PUBLIC_CB_ENVIRONMENT", "staging")
	writeCreds(t, home, ".config/manicode-staging", `{"default": {"authToken": "cb_staging"}}`)
	writeCreds(t, home, ".config/manicode", `{"default": {"authToken": "cb_plain"}}`)

	token, _, path, ok := clicreds.DiscoverToken()
	if !ok || token != "cb_staging" {
		t.Fatalf("DiscoverToken = (%q, %v), want cb_staging", token, ok)
	}
	if want := filepath.Join(home, ".config", "manicode-staging", "credentials.json"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}
