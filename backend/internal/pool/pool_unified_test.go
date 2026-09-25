package pool

// pool_unified_test.go — Lane B proofs of the four unified-store behavioral
// invariants for the runtime plane (pool_persist.go dirty-flag Flush +
// cooldown_hint.go spill; sessions_persist via the session.Store spill):
//
//   - I1 sync-visible: a mutation is visible to the next read synchronously.
//   - I2 no disk on request path: mutations issue zero backend writes
//     (write-call counting, not timing) until an explicit Flush.
//   - I3 restart recovers: Flush on one pool + restore on a fresh pool over
//     the same backend keeps counters and hints (existing restore tests stay
//     green alongside).
//   - I4 .env untouched/unread post-boot: mutations + flush + restore with a
//     garbage .env in the cwd behave identically and leave every file byte
//     (the pool takes no file paths — any file leg would show here).
//
// Revalidation is unchanged and stays pinned by the existing tests:
// restore drops out-of-window usage (TestPoolPersistExpiresUsageOnRestore),
// installLedger clips windows and rolls spend buckets, hints are
// expiry-checked (TestCooldownHintMissingAndExpiredEligible). Slot counters,
// cooldowns, single-flight and ip_capped are never staged (allowlist in
// pool_persist.go; TestCooldownHintNoRowsForRateLimitAndIpCapped).

import (
	"context"
	"freebucks-proxy/backend/internal/testutil"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPoolUnifiedSyncVisible (I1): ledger and hint mutations are visible to
// the next read synchronously, before any flush.
func TestPoolUnifiedSyncVisible(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mem := newMemPoolPersist()
	p := newTestPool(t, mock)
	p.SetPoolPersist(mem)

	toks := p.roster.Load()
	p.recordChatEntry((*toks)[0])
	if got := p.usageCount(0); got != 1 {
		t.Fatalf("usageCount right after recordChatEntry = %d, want 1 (I1)", got)
	}

	hash := poolTokenHash("tok-0")
	p.storeCooldownHint(hash, poolCooldownBlob{Kind: cooldownHintKindBan})
	if !p.cooldownHintFresh(hash, time.Now()) {
		t.Fatal("hint not fresh right after storeCooldownHint (I1)")
	}
}

// TestPoolUnifiedNoDiskOnRequestPath (I2): ledger, spend, admission and hint
// mutations issue zero backend writes; only the explicit flush persists.
func TestPoolUnifiedNoDiskOnRequestPath(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mem := newMemPoolPersist()
	p := newTestPool(t, mock)
	p.SetPoolPersist(mem)

	toks := p.roster.Load()
	entry := (*toks)[0]
	p.recordChatEntry(entry)
	p.recordSpendEntry(entry, 100)
	p.admissionsMu.Lock()
	p.admissions[modelA] = 3
	p.admissionsMu.Unlock()
	p.markPersistDirty()
	p.storeCooldownHint(poolTokenHash("tok-0"), poolCooldownBlob{Kind: cooldownHintKindBan})

	if n := mem.saveCount(); n != 0 {
		t.Fatalf("backend saves after mutations without flush = %d, want 0 (I2)", n)
	}
	if err := p.FlushPoolPersist(); err != nil {
		t.Fatalf("FlushPoolPersist: %v", err)
	}
	if n := mem.saveCount(); n == 0 {
		t.Fatal("backend saves after Flush = 0, want > 0 (flush persists behind)")
	}
}

// TestPoolUnifiedRestartRecovers (I3): a flush on one pool plus a restore on
// a fresh pool over the same backend recovers ledger counters and hints.
func TestPoolUnifiedRestartRecovers(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mem := newMemPoolPersist()

	p1 := newTestPool(t, mock)
	p1.SetPoolPersist(mem)
	toks := p1.roster.Load()
	p1.recordChatEntry((*toks)[0])
	p1.recordSpendEntry((*toks)[0], 100)
	p1.storeCooldownHint(poolTokenHash("tok-0"), poolCooldownBlob{Kind: cooldownHintKindBan})
	if err := p1.FlushPoolPersist(); err != nil {
		t.Fatalf("FlushPoolPersist: %v", err)
	}

	p2 := newTestPool(t, mock)
	p2.SetPoolPersist(mem)
	p2.RestorePoolPersist()
	if got := p2.usageCount(0); got != 1 {
		t.Fatalf("restored usageCount = %d, want 1 (I3)", got)
	}
	if !p2.cooldownHintFresh(poolTokenHash("tok-0"), time.Now()) {
		t.Fatal("restored hint not fresh (I3)")
	}
}

// TestPoolUnifiedEnvUntouched (I4): with a garbage .env in the cwd, the full
// mutate + flush + restore cycle behaves identically and leaves every file
// byte-identical. The pool takes no file paths, so any file leg (a .env
// write, a re-read) would fail this test.
func TestPoolUnifiedEnvUntouched(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	poison := []byte("API_KEYS=poison\nSESSION_PERSIST=false\n")
	if err := os.WriteFile(envPath, poison, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	mock := testutil.NewMock()
	defer mock.Close()
	mem := newMemPoolPersist()
	p := newTestPool(t, mock)
	p.SetPoolPersist(mem)
	toks := p.roster.Load()
	p.recordChatEntry((*toks)[0])
	p.storeCooldownHint(poolTokenHash("tok-0"), poolCooldownBlob{Kind: cooldownHintKindBan})
	if err := p.FlushPoolPersist(); err != nil {
		t.Fatalf("FlushPoolPersist with poison .env: %v", err)
	}

	p2 := newTestPool(t, mock)
	p2.SetPoolPersist(mem)
	p2.RestorePoolPersist()
	if got := p2.usageCount(0); got != 1 {
		t.Fatalf("restored usageCount with poison .env = %d, want 1", got)
	}
	if !p2.cooldownHintFresh(poolTokenHash("tok-0"), time.Now()) {
		t.Fatal("restored hint with poison .env not fresh")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != ".env" {
		t.Fatalf("cwd entries = %v, want exactly [.env] (no file leg)", entries)
	}
	after, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(poison) {
		t.Fatalf(".env after mutations = %q, want byte-identical %q", after, poison)
	}
}

// TestPoolUnifiedAcquirePathIssuesNoBackendWrites (I2, request path): a live
// Acquire + release cycle (session admission path included) issues zero
// backend writes; the dirty flag carries the work to the background flush.
func TestPoolUnifiedAcquirePathIssuesNoBackendWrites(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mem := newMemPoolPersist()
	p := newTestPool(t, mock)
	p.SetPoolPersist(mem)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	p.LeaseRelease(lease)

	if n := mem.saveCount(); n != 0 {
		t.Fatalf("backend saves across Acquire/LeaseRelease = %d, want 0 (I2)", n)
	}
	if !p.persistDirty.Load() {
		t.Fatal("dirty flag not armed after Acquire (background flush would skip the work)")
	}
}
