package session

// Park-gate pins: Manager.ShouldPark shares the exact rule with pool
// shouldPark — enabled plus a positive at-or-below-threshold cooldown —
// so a non-positive cooldown never parks in either layer.

import (
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
	"testing"
	"time"
)

func newParkTestManager(t *testing.T) *Manager {
	t.Helper()
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	client, err := upstream.New("tok", &config.Config{
		UpstreamBaseURL:    mock.URL(),
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewManager(client)
}

// TestShouldParkGate pins the park rule on a default manager (park enabled,
// 15m threshold): short positive cooldowns park, the boundary parks,
// longer ones drop, and non-positive cooldowns never park.
func TestShouldParkGate(t *testing.T) {
	m := newParkTestManager(t)
	for _, d := range []time.Duration{time.Second, 5 * time.Minute, 15 * time.Minute} {
		if !m.ShouldPark(d) {
			t.Errorf("ShouldPark(%v) = false, want true (enabled, within threshold)", d)
		}
	}
	if m.ShouldPark(16 * time.Minute) {
		t.Error("ShouldPark(16m) = true, want false (past the 15m threshold)")
	}
	for _, d := range []time.Duration{0, -time.Second} {
		if m.ShouldPark(d) {
			t.Errorf("ShouldPark(%v) = true, want false (non-positive never parks)", d)
		}
	}
}

// TestShouldParkDisabled pins the OFF switch: a parked-disabled manager
// drops every cooldown.
func TestShouldParkDisabled(t *testing.T) {
	m := newParkTestManager(t)
	m.SetParkConfig(false, 15*time.Minute)
	if m.ShouldPark(5 * time.Minute) {
		t.Error("ShouldPark(5m) disabled = true, want false")
	}
}
