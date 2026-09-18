package main

import (
	"os"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// TestMain strips ambient freebuff-proxy config env vars (AUTH_TOKENS,
// ADMIN_TOKEN, ...) so a developer's exported proxy environment
// cannot leak into the subprocess children, which pin their environment
// via e2eEnv.
func TestMain(m *testing.M) {
	testutil.UnsetConfigEnvForTestMain()
	os.Exit(m.Run())
}
