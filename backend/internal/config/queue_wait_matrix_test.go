package config

// QUEUE_WAIT matrix. The knob bounds how long one pooled live-turn acquire
// parks on a full token's FIFO queue before failing over, it is owned by the
// pool strategy presets (Drain 300s / Balance 15s), and it is the row the
// dashboard writes on every pool-tuning edit — so its write gate has to be
// exact in both directions: a rejectable value must never reach the settings
// table (the 500 persist_failed the operator saw), and an accepted value must
// always load, because the POST validates through a real Load before it stores.
//
// Two contracts are pinned here, at their real boundaries:
//
//   - ValidateSettingValue is the pre-transaction gate: empty and whitespace
//     are DELETE-only (the key is reset, never blanked), a non-duration is a
//     400, and every valid Go duration passes — non-positive included, since
//     the loader deliberately floors 0s/-5s to the 30s default instead of
//     failing the load.
//   - Load is the floor and the live-apply path: what the gate accepts lands on
//     the running Config with the documented floor, and what it rejects makes
//     Load fail, so no accepted value can become a stored no-op.

import (
	"os"
	"strings"
	"testing"
	"time"
)

// queueWaitDataValue reads the dashboard rendering of one key from the same
// Config.Data() mapping the settings console shows, so the echo assertions
// exercise the real display path rather than the struct field twice.
func queueWaitDataValue(t *testing.T, cfg *Config) string {
	t.Helper()
	for _, e := range cfg.Data() {
		if e.Key == "QUEUE_WAIT" {
			return e.Value
		}
	}
	t.Fatal("QUEUE_WAIT missing from Config.Data()")
	return ""
}

// TestValidateSettingValueQueueWaitMatrix pins the pre-transaction gate for the
// knob: the outcomes the settings POST turns into a response code (empty -> 400
// reset, unparseable -> 400 naming the key, the requirement and the offending
// value, non-positive -> accepted and floored later, positive -> accepted and
// applied live).
//
// The 400 text is asserted as the three facts the operator needs — which key,
// that a Go duration is required, and what was rejected — not as the driver's
// raw parse text: the message follows this package's existing duration-knob
// shape (cf. the QUOTA_PROBE_* checks), and a test that pins the stdlib string
// would fight the convention instead of the behavior.
func TestValidateSettingValueQueueWaitMatrix(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr []string // substrings the 400 must carry; empty means the gate accepts the value
	}{
		{name: "empty is reset-only", value: "", wantErr: []string{"QUEUE_WAIT", "use DELETE to reset the key"}},
		{name: "whitespace is empty", value: "   ", wantErr: []string{"QUEUE_WAIT", "must not be empty", "use DELETE to reset the key"}},
		{name: "garbage", value: "bogus", wantErr: []string{"QUEUE_WAIT", "must be a Go duration", `got "bogus"`}},
		{name: "bare number is not a duration", value: "5", wantErr: []string{"QUEUE_WAIT", "must be a Go duration", `got "5"`}},
		{name: "zero floors at load", value: "0s"},
		{name: "negative floors at load", value: "-5s"},
		{name: "seconds", value: "90s"},
		{name: "compound", value: "1m30s"},
		{name: "hours", value: "2h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSettingValue("QUEUE_WAIT", tc.value)
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("ValidateSettingValue(QUEUE_WAIT, %q) = %v, want nil", tc.value, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateSettingValue(QUEUE_WAIT, %q) accepted, want a 400 message carrying %q", tc.value, tc.wantErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("ValidateSettingValue(QUEUE_WAIT, %q) = %v, want a message containing %q", tc.value, err, want)
				}
			}
		})
	}
}

// TestQueueWaitGateParityWithLoad is the property the pre-transaction gate
// exists for. The gate runs before the save lock and before any write, while the
// POST still re-validates through a real Load before it stores, so for every
// candidate value
//
//	gate accepts <=> Load accepts and lands a positive wait
//
// must hold. A gap in either direction is a defect: a value the gate waves
// through but Load rejects costs the operator a 400 (or a stored row the running
// config cannot use), and a value the gate rejects after Load accepted it is a
// 400 on something that would have worked.
func TestQueueWaitGateParityWithLoad(t *testing.T) {
	clearEnv(t)
	for _, value := range []string{"bogus", "5", "1d", "0s", "-5s", "-1m", "90s", "1m30s", "2h", "500ms"} {
		t.Run(value, func(t *testing.T) {
			gateErr := ValidateSettingValue("QUEUE_WAIT", value)
			cfg, loadErr := LoadOpts("", LoadOptions{Overlay: map[string]string{"QUEUE_WAIT": value}})
			if (gateErr == nil) != (loadErr == nil) {
				t.Fatalf("gate/Load disagree for %q: gate=%v load=%v — the pre-transaction gate must reject exactly what Load rejects",
					value, gateErr, loadErr)
			}
			if gateErr != nil {
				return
			}
			if cfg.QueueWait <= 0 {
				t.Fatalf("gate accepted %q but Load landed QueueWait = %v, want a positive wait", value, cfg.QueueWait)
			}
		})
	}
}

// TestQueueWaitLiveApplyAndEcho pins what a saved row does to the running config
// and to the value the console renders back: a positive duration applies live
// (and echoes in canonical Go form), while a non-positive one is floored to the
// 30s default instead of erroring — the behavior the catalog description
// promises ("Zero-tolerant: empty or non-positive values fall back to 30s").
func TestQueueWaitLiveApplyAndEcho(t *testing.T) {
	clearEnv(t)
	for _, tc := range []struct {
		overlay  string
		want     time.Duration
		wantEcho string
	}{
		{overlay: "90s", want: 90 * time.Second, wantEcho: "1m30s"},
		{overlay: "5s", want: 5 * time.Second, wantEcho: "5s"},
		{overlay: "0s", want: 30 * time.Second, wantEcho: "30s"},
		{overlay: "-5s", want: 30 * time.Second, wantEcho: "30s"},
		{overlay: "300s", want: 5 * time.Minute, wantEcho: "5m0s"},
	} {
		cfg, err := LoadOpts("", LoadOptions{Overlay: map[string]string{"QUEUE_WAIT": tc.overlay}})
		if err != nil {
			t.Fatalf("LoadOpts(QUEUE_WAIT=%s): %v", tc.overlay, err)
		}
		if cfg.QueueWait != tc.want {
			t.Errorf("QueueWait(overlay %s) = %v, want %v", tc.overlay, cfg.QueueWait, tc.want)
		}
		if got := queueWaitDataValue(t, &cfg); got != tc.wantEcho {
			t.Errorf("QUEUE_WAIT rendered as %q for overlay %s, want %q", got, tc.overlay, tc.wantEcho)
		}
	}
}

// TestQueueWaitTierPrecedence walks the knob through all four tiers of
// ADR-0019 (default < file < db overlay < process env) and checks the source tag
// the console renders for each, because the pool presets take ownership of this
// key and an operator must see which tier wins.
func TestQueueWaitTierPrecedence(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())

	// Default.
	cfg, err := LoadOpts("", LoadOptions{})
	if err != nil {
		t.Fatalf("LoadOpts(default): %v", err)
	}
	if cfg.QueueWait != 30*time.Second {
		t.Errorf("QueueWait(default) = %v, want 30s", cfg.QueueWait)
	}
	if src := SettingSources("", nil)["QUEUE_WAIT"]; src != "default" {
		t.Errorf("QUEUE_WAIT source = %q, want default", src)
	}

	// File (.env).
	if err := os.WriteFile(".env", []byte("QUEUE_WAIT=45s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadOpts("", LoadOptions{})
	if err != nil {
		t.Fatalf("LoadOpts(file): %v", err)
	}
	if cfg.QueueWait != 45*time.Second {
		t.Errorf("QueueWait(file) = %v, want 45s", cfg.QueueWait)
	}
	if src := SettingSources("", nil)["QUEUE_WAIT"]; src != "file" {
		t.Errorf("QUEUE_WAIT source = %q, want file", src)
	}

	// DB overlay beats the file.
	overlay := map[string]string{"QUEUE_WAIT": "60s"}
	cfg, err = LoadOpts("", LoadOptions{Overlay: overlay})
	if err != nil {
		t.Fatalf("LoadOpts(overlay): %v", err)
	}
	if cfg.QueueWait != 60*time.Second {
		t.Errorf("QueueWait(overlay) = %v, want 60s (overlay beats .env)", cfg.QueueWait)
	}
	if src := SettingSources("", overlay)["QUEUE_WAIT"]; src != "db" {
		t.Errorf("QUEUE_WAIT source = %q, want db", src)
	}

	// Explicit process env beats the overlay.
	t.Setenv("QUEUE_WAIT", "90s")
	cfg, err = LoadOpts("", LoadOptions{Overlay: overlay})
	if err != nil {
		t.Fatalf("LoadOpts(env): %v", err)
	}
	if cfg.QueueWait != 90*time.Second {
		t.Errorf("QueueWait(env) = %v, want 90s (env beats overlay)", cfg.QueueWait)
	}
	if src := SettingSources("", overlay)["QUEUE_WAIT"]; src != "env" {
		t.Errorf("QUEUE_WAIT source = %q, want env", src)
	}
}
