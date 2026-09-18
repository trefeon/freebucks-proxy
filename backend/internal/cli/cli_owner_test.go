package cli

import (
	"path/filepath"
	"testing"
)

// TestCliOwnerFilePathFollowsConfigDir pins cliOwnerFilePath to the shared
// clicreds.ConfigDir resolver: the absolute $FREEBUFF_CONFIG_DIR override,
// the $NEXT_PUBLIC_CB_ENVIRONMENT suffix, and the plain manicode default
// each select the owner file next to the credentials file discovery reads.
func TestCliOwnerFilePathFollowsConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	t.Setenv("FREEBUFF_CONFIG_DIR", "")
	t.Setenv("NEXT_PUBLIC_CB_ENVIRONMENT", "")
	got, err := cliOwnerFilePath()
	if err != nil {
		t.Fatalf("cliOwnerFilePath: %v", err)
	}
	if want := filepath.Join(home, ".config", "manicode", "freebuff-instance-owner.json"); got != want {
		t.Errorf("default: got %q, want %q", got, want)
	}

	dir := t.TempDir()
	t.Setenv("FREEBUFF_CONFIG_DIR", dir)
	got, err = cliOwnerFilePath()
	if err != nil {
		t.Fatalf("cliOwnerFilePath with override: %v", err)
	}
	if want := filepath.Join(dir, "freebuff-instance-owner.json"); got != want {
		t.Errorf("override: got %q, want %q", got, want)
	}

	t.Setenv("FREEBUFF_CONFIG_DIR", "")
	t.Setenv("NEXT_PUBLIC_CB_ENVIRONMENT", "staging")
	got, err = cliOwnerFilePath()
	if err != nil {
		t.Fatalf("cliOwnerFilePath with suffix: %v", err)
	}
	if want := filepath.Join(home, ".config", "manicode-staging", "freebuff-instance-owner.json"); got != want {
		t.Errorf("suffix: got %q, want %q", got, want)
	}

	t.Setenv("FREEBUFF_CONFIG_DIR", "relative/path")
	if _, err := cliOwnerFilePath(); err == nil {
		t.Error("relative override: got nil error, want rejection")
	}
}
