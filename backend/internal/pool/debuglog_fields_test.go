package pool

import (
	"bytes"
	"context"
	"freebucks-proxy/backend/internal/testutil"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestCooldownSkipLogCarriesTokenAndReason pins the debug-log contract for
// the cooldown gate: the skip line must carry the 1-based token, the window
// expiry, and the machine-readable reason (cooldown vs banned vs
// rate_limited) so the next production incident is analyzable from logs.
func TestCooldownSkipLogCarriesTokenAndReason(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	var buf bytes.Buffer
	p.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	p.CooldownToken(0, time.Hour)
	if _, err := p.Acquire(context.Background(), modelA); err == nil {
		t.Fatal("want error while the only token is cooling down")
	}

	var skip string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "pool: token skipped (cooldown)") {
			skip = line
			break
		}
	}
	if skip == "" {
		t.Fatalf("no `pool: token skipped (cooldown)` line logged:\n%s", buf.String())
	}
	for _, want := range []string{"token=1", "until=", "reason=cooldown"} {
		if !strings.Contains(skip, want) {
			t.Errorf("cooldown skip line missing %q: %s", want, skip)
		}
	}
}
