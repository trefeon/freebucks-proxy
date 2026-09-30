package pool

import (
	"context"
	"freebuff-proxy/backend/internal/store"
	"freebuff-proxy/backend/internal/testutil"
	"path/filepath"
	"testing"
)

// TestPoolStartRestoresPersistedCounters pins the post-roll zero regression:
// the write path (dirty → spill/Shutdown flush → pool_state) worked, but
// Pool.Start never called RestorePoolPersist, so every restart zeroed
// Messages24h, RequestsPerDay, the spend buckets and Requests. Start must
// restore all of them from the same backend the flush wrote.
func TestPoolStartRestoresPersistedCounters(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mem := newMemPoolPersist()

	p1 := newTestPool(t, mock)
	p1.SetPoolPersist(mem)
	for range 2 {
		lease, err := p1.Acquire(context.Background(), modelA)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		p1.LeaseRelease(lease)
	}
	toks := p1.roster.Load()
	p1.recordChatEntry((*toks)[0])
	p1.recordSpendEntry((*toks)[0], 100)
	if err := p1.FlushPoolPersist(); err != nil {
		t.Fatalf("FlushPoolPersist: %v", err)
	}

	p2 := newTestPool(t, mock)
	p2.SetPoolPersist(mem)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p2.Start(ctx)

	if got := p2.usageCount(0); got != 1 {
		t.Errorf("restored usageCount = %d, want 1 (Messages24h)", got)
	}
	if got := p2.spendSnapshot(0).Day; got != 100 {
		t.Errorf("restored spend day = %d, want 100", got)
	}
	snaps := p2.Snapshot()
	if len(snaps) != 1 {
		t.Fatalf("Snapshot rows = %d, want 1", len(snaps))
	}
	if snaps[0].Messages24h != 1 {
		t.Errorf("restored Messages24h = %d, want 1", snaps[0].Messages24h)
	}
	if snaps[0].Requests != 2 {
		t.Errorf("restored Requests = %d, want 2 (mint total survives)", snaps[0].Requests)
	}

	// Mints continue from the restored base instead of restarting at zero.
	lease, err := p2.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("Acquire after restore: %v", err)
	}
	p2.LeaseRelease(lease)
	if got := p2.Snapshot()[0].Requests; got != 3 {
		t.Errorf("Requests after post-restore mint = %d, want 3", got)
	}
	// Shutdown joins the Start goroutines (cancel alone does not wait).
	p2.Shutdown(context.Background())
}

// TestPoolStartRestoreStoreRestart simulates a process restart at the store
// level: feed → flush → close the DB → reopen → Start on a fresh pool over
// the reopened store → identical numbers.
func TestPoolStartRestoreStoreRestart(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	path := filepath.Join(t.TempDir(), "ledger-restart.db")

	st1, err := store.Open(path)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	p1 := newTestPool(t, mock)
	p1.SetPoolPersist(st1)
	lease, err := p1.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	p1.LeaseRelease(lease)
	toks := p1.roster.Load()
	p1.recordChatEntry((*toks)[0])
	p1.recordSpendEntry((*toks)[0], 250)
	if err := p1.FlushPoolPersist(); err != nil {
		t.Fatalf("FlushPoolPersist: %v", err)
	}
	if err := st1.Close(); err != nil {
		t.Fatalf("store close: %v", err)
	}

	st2, err := store.Open(path)
	if err != nil {
		t.Fatalf("store reopen: %v", err)
	}
	defer func() { _ = st2.Close() }()
	p2 := newTestPool(t, mock)
	p2.SetPoolPersist(st2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p2.Start(ctx)

	if got := p2.usageCount(0); got != 1 {
		t.Errorf("restarted usageCount = %d, want 1", got)
	}
	if got := p2.spendSnapshot(0).Day; got != 250 {
		t.Errorf("restarted spend day = %d, want 250", got)
	}
	if got := p2.Snapshot()[0].Requests; got != 1 {
		t.Errorf("restarted Requests = %d, want 1", got)
	}
	// Shutdown joins the Start goroutines so no sqlite handle is live when
	// the deferred store Close runs (Windows TempDir cleanup needs it).
	p2.Shutdown(context.Background())
}
