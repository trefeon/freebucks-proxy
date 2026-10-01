package pool

import (
	"context"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// All-locked pool must name the lock, not the generic combined error (which
// the server maps to 502 upstream_unavailable — an outage reading for an
// operator lock, with zero upstream contact attempted).
func TestAllLockedFailsFastNamingLock(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)

	if err := p.LockToken(0); err != nil {
		t.Fatal(err)
	}
	if err := p.LockToken(1); err != nil {
		t.Fatal(err)
	}
	_, err := p.Acquire(context.Background(), modelA)
	if err == nil {
		t.Fatal("acquire on all-locked pool succeeded, want lock error")
	}
	if !strings.Contains(err.Error(), "administratively locked") {
		t.Errorf("error = %q, want it to name the administrative lock", err)
	}
	if strings.Contains(err.Error(), "unable to acquire run from any token") {
		t.Errorf("error = %q, want the lock fast-fail, not the generic combined error", err)
	}
	if mock0.SessionCreatesSnapshot()+mock1.SessionCreatesSnapshot() != 0 {
		t.Error("all-locked pool contacted upstream, want zero admissions")
	}
}

// One locked lane records its skip: a later generic failure still names the
// lock in the combined error instead of going context-free.
func TestLockedSkipRecordedInWalk(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)

	if err := p.LockToken(0); err != nil {
		t.Fatal(err)
	}
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("acquire with one locked lane: %v", err)
	}
	if lease.Token != 1 {
		t.Errorf("lease token = %d, want 1 (locked lane rotates)", lease.Token)
	}
	p.LeaseRelease(lease)
}
