package pool

// Regression guards for the shutdown fixes:
//
//   - Pool.Shutdown must not hold bridgeMu across the per-entry upstream
//     drain (FinishAllRuns + session shutdown for every cached bridge
//     entry): with the lock held for the whole drain, every other bridge
//     operation (AcquireBridge, bridgeRecordChat/bridgeRecordSpend,
//     BridgeCount) stalls behind sequential upstream calls (the same rule
//     bridgeEvictLocked / bridgeMaintain already follow).
//   - A bridge entry with an outstanding lease at shutdown must not lose
//     its FINISH: FinishAllRuns defers in-flight runs, and the last lease
//     release re-queues the FINISH before the drain completes.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"freebucks-proxy/backend/internal/testutil"
)

// TestShutdownBridgeDrainOutsideBridgeMu is the regression guard for the
// bridgeMu stall: Pool.Shutdown used to hold bridgeMu across the whole
// per-entry drain. Here a slow FINISH (mock FinishDelay) holds the drain in
// flight while a concurrent bridgeMu acquisition must still complete — with
// the bug it would block for the rest of the drain.

// TestShutdownBridgeInflightRunFinishedOnRelease: bridge entries drain
// through the same RunManager.Shutdown as fixed tokens (issue #233). An
// outstanding lease is FINISHed by the shutdown drain itself (the HTTP
// server already stopped accepting and the lease holder's connection is
// gone by then), and the later lease release is a clean no-op — no orphaned
// upstream run, no double FINISH.
