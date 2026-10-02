// WALLET_SPEND_LIMIT operator surface: env/.env/JSON resolve into the
// standing wallet-spend consent stamped on each admission POST, the headless
// default stays "0", and anything inexpressible fails Load (never silently
// widens spend).
package config

import (
	"os"
	"testing"
)

func TestWalletSpendLimitSurface(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("WALLET_SPEND_LIMIT", "5")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WalletSpendLimit != "5" {
		t.Errorf("WalletSpendLimit = %q, want 5 (env)", cfg.WalletSpendLimit)
	}

	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("WALLET_SPEND_LIMIT", "session")
	cfg, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WalletSpendLimit != "session" {
		t.Errorf("WalletSpendLimit = %q, want session", cfg.WalletSpendLimit)
	}

	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	cfg, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WalletSpendLimit != "0" {
		t.Errorf("WalletSpendLimit = %q, want headless default 0", cfg.WalletSpendLimit)
	}
}

func TestWalletSpendLimitDotenv(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	if err := os.WriteFile(".env", []byte("WALLET_SPEND_LIMIT=session\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(".env") })
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WalletSpendLimit != "session" {
		t.Errorf("WalletSpendLimit = %q, want session (from .env)", cfg.WalletSpendLimit)
	}
}

func TestWalletSpendLimitRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"bogus", "-3", "5.5", "0x10", "99999999999"} {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		t.Setenv("WALLET_SPEND_LIMIT", bad)
		if _, err := Load(""); err == nil {
			t.Errorf("Load(WALLET_SPEND_LIMIT=%q) succeeded, want rejection", bad)
		}
	}
	// "session " with a trailing space trims clean — load must accept it.
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("WALLET_SPEND_LIMIT", "session ")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(WALLET_SPEND_LIMIT=%q): %v", "session ", err)
	}
	if cfg.WalletSpendLimit != "session" {
		t.Errorf("WalletSpendLimit = %q, want trimmed session", cfg.WalletSpendLimit)
	}
}

func TestValidWalletSpendLimitTable(t *testing.T) {
	for raw, want := range map[string]bool{
		"":        true,
		"0":       true,
		"5":       true,
		"007":     true,
		"session": true,
		"bogus":   false,
		"-3":      false,
		"5.5":     false,
		"SESSION": false,
	} {
		if got := validWalletSpendLimit(raw); got != want {
			t.Errorf("validWalletSpendLimit(%q) = %v, want %v", raw, got, want)
		}
	}
}
