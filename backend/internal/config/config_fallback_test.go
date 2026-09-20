package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// --- #48: WEBHOOK_URL -------------------------------------------------------

func TestWebhookURLParsed(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("WEBHOOK_URL", "https://example.com/hook")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebhookURL != "https://example.com/hook" {
		t.Errorf("WebhookURL = %q, want https://example.com/hook", cfg.WebhookURL)
	}
}

func TestWebhookURLInvalidFails(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("WEBHOOK_URL", "not-a-url")

	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "WEBHOOK_URL") {
		t.Fatalf("Load err = %v, want WEBHOOK_URL validation error", err)
		return
	}
}

// --- #97: ADOPT_CLI_SESSION -------------------------------------------------

func TestAdoptCLISessionParsed(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("ADOPT_CLI_SESSION", "true")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AdoptCLISession {
		t.Error("AdoptCLISession = false, want true")
	}
	if cfg.WaitingRoomChain {
		t.Error("WaitingRoomChain = true, want false default")
	}
}

// --- #39: XDG / AppData config search ---------------------------------------

func TestEnvFileCandidatesOrderAndShape(t *testing.T) {
	clearEnv(t)
	cands := EnvFileCandidates()
	if len(cands) != 2 {
		t.Fatalf("EnvFileCandidates = %v, want exactly 2 candidates", cands)
	}
	// Working directory always wins (first candidate).
	if filepath.Clean(cands[0]) != filepath.Join(".", ".env") {
		t.Errorf("candidate[0] = %q, want ./.env first", cands[0])
	}
	// The platform dir must end with freebucks-proxy/.env.
	if filepath.Base(filepath.Dir(cands[1])) != "freebucks-proxy" || filepath.Base(cands[1]) != ".env" {
		t.Errorf("candidate[1] = %q, want <config-dir>/freebucks-proxy/.env", cands[1])
	}
	// Platform-specific base dir.
	switch runtime.GOOS {
	case "windows":
		if !strings.Contains(strings.ToLower(cands[1]), "appdata") && !strings.Contains(strings.ToLower(cands[1]), "roaming") {
			t.Errorf("windows candidate[1] = %q, want %%APPDATA%%\\freebucks-proxy\\.env", cands[1])
		}
	case "darwin":
		if !strings.Contains(cands[1], "Application Support") {
			t.Errorf("darwin candidate[1] = %q, want ~/Library/Application Support/freebucks-proxy/.env", cands[1])
		}
	default:
		if !strings.Contains(cands[1], ".config") {
			t.Errorf("linux candidate[1] = %q, want ~/.config/freebucks-proxy/.env", cands[1])
		}
	}
}

func TestResolveEnvFileCwdWins(t *testing.T) {
	clearEnv(t)
	// cwd (temp dir) has .env; the platform dir also gets one — cwd must win.
	if err := os.WriteFile(".env", []byte("LISTEN_ADDR=:1001\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	platform := EnvFileCandidates()[1]
	if err := os.MkdirAll(filepath.Dir(platform), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(platform, []byte("LISTEN_ADDR=:1002\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(platform)) })

	if got := ResolveEnvFile(); filepath.Clean(got) != filepath.Join(".", ".env") {
		t.Errorf("ResolveEnvFile = %q, want ./.env (cwd wins)", got)
	}
}

func TestResolveEnvFilePlatformFallback(t *testing.T) {
	clearEnv(t)
	// No ./.env in cwd; only the platform config dir has one.
	platform := EnvFileCandidates()[1]
	if err := os.MkdirAll(filepath.Dir(platform), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(platform, []byte("LISTEN_ADDR=:1003\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(platform)) })

	if got := ResolveEnvFile(); filepath.Clean(got) != filepath.Clean(platform) {
		t.Errorf("ResolveEnvFile = %q, want platform candidate %q", got, platform)
	}
}

func TestResolveEnvFileNone(t *testing.T) {
	clearEnv(t)
	// Neither candidate exists (temp cwd; remove any platform file).
	platform := EnvFileCandidates()[1]
	_ = os.RemoveAll(filepath.Dir(platform))
	if got := ResolveEnvFile(); got != "" {
		t.Errorf("ResolveEnvFile = %q, want empty when no candidate exists", got)
	}
}

// Load must actually READ the platform-dir .env when cwd has none and record
// the path on Config.EnvFile (issue #39).
func TestLoadReadsPlatformEnvFile(t *testing.T) {
	clearEnv(t)
	platform := EnvFileCandidates()[1]
	if err := os.MkdirAll(filepath.Dir(platform), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(platform, []byte("AUTH_TOKENS=tok-platform\nLISTEN_ADDR=:1004\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(platform)) })

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":1004" {
		t.Errorf("ListenAddr = %q, want :1004 from platform .env", cfg.ListenAddr)
	}
	if filepath.Clean(cfg.EnvFile) != filepath.Clean(platform) {
		t.Errorf("EnvFile = %q, want platform candidate %q", cfg.EnvFile, platform)
	}
}

// With a cwd .env present, Load must use it (not the platform file) and
// record the cwd path.
func TestLoadCwdEnvWinsOverPlatform(t *testing.T) {
	clearEnv(t)
	if err := os.WriteFile(".env", []byte("AUTH_TOKENS=tok-cwd\nLISTEN_ADDR=:1005\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	platform := EnvFileCandidates()[1]
	if err := os.MkdirAll(filepath.Dir(platform), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(platform, []byte("AUTH_TOKENS=tok-platform\nLISTEN_ADDR=:1006\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(platform)) })

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":1005" {
		t.Errorf("ListenAddr = %q, want :1005 from cwd .env", cfg.ListenAddr)
	}
	if filepath.Clean(cfg.EnvFile) != filepath.Join(".", ".env") {
		t.Errorf("EnvFile = %q, want ./.env", cfg.EnvFile)
	}
}

func TestEnvFileEmptyWithoutFiles(t *testing.T) {
	clearEnv(t)
	_ = os.RemoveAll(filepath.Dir(EnvFileCandidates()[1]))
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EnvFile != "" {
		t.Errorf("EnvFile = %q, want empty when no .env exists", cfg.EnvFile)
	}
}
