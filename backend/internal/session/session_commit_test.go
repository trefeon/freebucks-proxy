package session

import (
	"context"
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newReviewFixManager builds a manager wired to a temp-file store, mirroring
// newPersistTestManager, for the review-2026-08-31 regression tests. The
// mock upstream is only there so Shutdown's EndSession has somewhere to
// talk to; the tests never drive admission.
func newReviewFixManager(t *testing.T) (*Manager, *Store, string) {
	t.Helper()
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	client, err := upstream.New("tok", &config.Config{
		UpstreamBaseURL:    mock.URL(),
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RotationInterval:   6 * time.Hour,
		RegistryRefresh:    6 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	mgr := NewManagerWithStore(client, store)
	return mgr, store, client.TokenKey()
}

// reviewFixActiveState returns an active cached state bound to instanceID,
// usable immediately (expiry an hour out).
func reviewFixActiveState(instanceID string) *cachedState {
	expiry := time.Now().Add(time.Hour)
	return &cachedState{
		status:            "active",
		instanceID:        instanceID,
		model:             "deepseek/deepseek-v4-flash",
		expiresAt:         expiry,
		gracePeriodEndsAt: expiry.Add(graceWindow),
	}
}

// blockingSessionBackend is a SessionBackend whose writes park until opened.
// It proves the request path never waits on the backend: commits complete
// while the spill consumer is stalled inside a write. Reads stay unblocked
// (restore must serve even while a write is stuck).
type blockingSessionBackend struct {
	mu       sync.Mutex
	rows     map[string][2]string
	writes   int
	entered  chan struct{}
	once     sync.Once
	release  chan struct{}
	openOnce sync.Once
}

func newBlockingSessionBackend() *blockingSessionBackend {
	return &blockingSessionBackend{
		rows:    make(map[string][2]string),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (b *blockingSessionBackend) markEntered() {
	b.once.Do(func() { close(b.entered) })
}

func (b *blockingSessionBackend) SaveSession(tokenHash, sessionData, runsData string) error {
	<-b.release
	b.markEntered()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.writeCount()
	if sessionData == "" && runsData == "" {
		delete(b.rows, tokenHash)
		return nil
	}
	b.rows[tokenHash] = [2]string{sessionData, runsData}
	return nil
}

func (b *blockingSessionBackend) writeCount() { b.writes++ }

func (b *blockingSessionBackend) SaveSessionRuns(tokenHash, runsData string) error {
	<-b.release
	b.markEntered()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.writeCount()
	prev := b.rows[tokenHash]
	b.rows[tokenHash] = [2]string{prev[0], runsData}
	return nil
}

func (b *blockingSessionBackend) DeleteSession(tokenHash string) error {
	<-b.release
	b.markEntered()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.writeCount()
	delete(b.rows, tokenHash)
	return nil
}

func (b *blockingSessionBackend) LoadSession(tokenHash string) (string, string, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.rows[tokenHash]
	if !ok {
		return "", "", false, nil
	}
	return r[0], r[1], true, nil
}

func (b *blockingSessionBackend) open() { b.openOnce.Do(func() { close(b.release) }) }

func (b *blockingSessionBackend) writeTotal() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.writes
}

func newReviewFixManagerWithBackend(t *testing.T, be SessionBackend) (*Manager, *Store, string) {
	t.Helper()
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	client, err := upstream.New("tok", &config.Config{
		UpstreamBaseURL:    mock.URL(),
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RotationInterval:   6 * time.Hour,
		RegistryRefresh:    6 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewStoreWithBackend(filepath.Join(t.TempDir(), "state.json"), be)
	mgr := NewManagerWithStore(client, store)
	return mgr, store, client.TokenKey()
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s blocked over 5s (request path waits on backend I/O)", what)
	}
}

func waitWrites(t *testing.T, be *blockingSessionBackend, what string) {
	t.Helper()
	if be.writeTotal() > 0 {
		return
	}
	select {
	case <-be.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s spill pass never reached the backend", what)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if be.writeTotal() > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s issued zero backend writes after the gate opened", what)
}

// TestCommitSavesOutsideManagerLock pins the unified-store spill contract on
// the admission path: commit swaps memory synchronously (the next Load sees
// it with zero disk I/O on the calling path) while the backend write lands
// behind. With the spill consumer stalled inside a backend write, a commit
// AND a concurrent Snapshot both still complete — nothing on the request
// path waits on disk.
func TestCommitSavesOutsideManagerLock(t *testing.T) {
	be := newBlockingSessionBackend()
	mgr, store, key := newReviewFixManagerWithBackend(t, be)
	store.StartSpill()
	defer func() {
		be.open()
		_ = store.Close()
	}()

	mgr.mu.Lock()
	mgr.commit(reviewFixActiveState("inst-seed"))
	mgr.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		mgr.mu.Lock()
		mgr.commit(reviewFixActiveState("inst-next"))
		mgr.mu.Unlock()
	}()
	waitClosed(t, done, "commit with stalled spill backend")

	snapDone := make(chan struct{})
	go func() {
		defer close(snapDone)
		_ = mgr.Snapshot()
	}()
	waitClosed(t, snapDone, "Snapshot with stalled spill backend")

	if persisted := store.Load(key); persisted == nil || persisted.instanceID != "inst-next" {
		t.Errorf("persisted entry after commit = %+v, want inst-next (commit must still mirror state into the store)", persisted)
	}

	// Durability behind: opening the gate lets the stalled spill pass land.
	be.open()
	waitWrites(t, be, "commit spill")
	if err := store.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	_ = store.Close()
	restarted := NewStoreWithBackend(store.path, be)
	if got := restarted.Load(key); got == nil || got.instanceID != "inst-next" {
		t.Errorf("reopened Load = %+v, want inst-next (spill must persist behind)", got)
	}
}

// TestShutdownSavesAndVerifiesOutsideManagerLock pins the unified-store
// spill contract on the shutdown path: Shutdown's flush applies to memory
// synchronously (the entry survives for restart resume) without waiting on
// backend I/O, and the row lands behind for the next boot.
func TestShutdownSavesAndVerifiesOutsideManagerLock(t *testing.T) {
	be := newBlockingSessionBackend()
	mgr, store, key := newReviewFixManagerWithBackend(t, be)
	store.StartSpill()
	defer func() {
		be.open()
		_ = store.Close()
	}()

	mgr.mu.Lock()
	mgr.commit(reviewFixActiveState("inst-shutdown"))
	mgr.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = mgr.Shutdown(context.Background()) // EndSession's upstream result is irrelevant here
	}()
	waitClosed(t, done, "Shutdown with stalled spill backend")

	if persisted := store.Load(key); persisted == nil || persisted.instanceID != "inst-shutdown" {
		t.Errorf("store entry after Shutdown = %+v, want inst-shutdown kept for restart resume", persisted)
	}

	be.open()
	waitWrites(t, be, "shutdown spill")
	if err := store.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	_ = store.Close()
	restarted := NewStoreWithBackend(store.path, be)
	if got := restarted.Load(key); got == nil || got.instanceID != "inst-shutdown" {
		t.Errorf("reopened Load = %+v, want inst-shutdown (shutdown flush must persist behind)", got)
	}
}
