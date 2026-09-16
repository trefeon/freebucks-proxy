package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInContainer(t *testing.T) {
	write := func(t *testing.T, content string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "marker")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("write marker: %v", err)
		}
		return p
	}
	missing := func(t *testing.T) string {
		t.Helper()
		return filepath.Join(t.TempDir(), "absent")
	}

	t.Run("dockerenv present", func(t *testing.T) {
		if !inContainer(write(t, ""), missing(t)) {
			t.Errorf("inContainer = false, want true (dockerenv present)")
		}
	})
	t.Run("both missing", func(t *testing.T) {
		if inContainer(missing(t), missing(t)) {
			t.Errorf("inContainer = true, want false (nothing present)")
		}
	})

	cgroups := map[string]struct {
		content string
		want    bool
	}{
		"docker":            {"12:devices:/docker/abc123\n", true},
		"kubepods":          {"0::/kubepods/burstable/pod123/abc\n", true},
		"containerd":        {"0::/system.slice/containerd.service\n", true},
		"uppercase DOCKER":  {"1:name=systemd:/DOCKER/abc\n", true},
		"plain host cgroup": {"0::/init.scope\n", false},
		"empty cgroup":      {"", false},
	}
	for name, tc := range cgroups {
		t.Run("cgroup "+name, func(t *testing.T) {
			if got := inContainer(missing(t), write(t, tc.content)); got != tc.want {
				t.Errorf("inContainer = %v, want %v", got, tc.want)
			}
		})
	}
}
