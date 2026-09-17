package config

import (
	"os"
	"reflect"
	"testing"
)

func TestParsePinModel(t *testing.T) {
	got, err := parsePinModel("")
	if err != nil || got != nil {
		t.Fatalf("empty = %v, %v; want nil, nil", got, err)
	}
	got, err = parsePinModel("0:z-ai/glm-5.2;1:deepseek/deepseek-v4-flash")
	if err != nil {
		t.Fatalf("valid: %v", err)
	}
	want := map[int]string{
		0: "z-ai/glm-5.2",
		1: "deepseek/deepseek-v4-flash",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parse = %v, want %v", got, want)
	}
	// Strict single-pin: a slot takes exactly one model — comma lists,
	// extra colons and duplicate slots are rejected instead of silently
	// keeping the last write.
	for _, bad := range []string{
		"no-colon-here",
		"x:model-a",
		"-1:model-a",
		"0:",
		"0:  ",
		"0:model-a,model-b",
		"0:model-a;0:model-b",
		"1:model-a:extra",
	} {
		if _, err := parsePinModel(bad); err == nil {
			t.Errorf("parse %q succeeded, want error", bad)
		}
	}
}

// TestPinModelDotenv verifies PIN_MODEL flows from .env through Load and
// that malformed values reject the config instead of silently misrouting
// quota.
func TestPinModelDotenv(t *testing.T) {
	clearEnv(t)

	content := "PIN_MODEL=0:z-ai/glm-5.2;1:deepseek/deepseek-v4-flash\n"
	if err := os.WriteFile(".env", []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := map[int]string{
		0: "z-ai/glm-5.2",
		1: "deepseek/deepseek-v4-flash",
	}
	if !reflect.DeepEqual(cfg.PinModel, want) {
		t.Errorf("PinModel = %v, want %v (from .env)", cfg.PinModel, want)
	}

	if err := os.WriteFile(".env", []byte("PIN_MODEL=banana\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(""); err == nil {
		t.Errorf("Load with malformed PIN_MODEL succeeded, want error")
	}
}
