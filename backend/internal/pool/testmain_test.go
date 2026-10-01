package pool

import (
	"os"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// TestMain strips ambient freebuff-proxy config env vars (AUTH_TOKENS,
// ADMIN_TOKEN, ...) so a developer's exported proxy environment
// cannot leak into config.Load with higher precedence than .env and break
// pool assertions.
func TestMain(m *testing.M) {
	testutil.UnsetConfigEnvForTestMain()
	os.Exit(m.Run())
}
