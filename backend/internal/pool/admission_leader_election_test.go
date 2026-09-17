// admission_leader_election_test.go — concurrency tests for cold-path
// admission dedup: concurrent cold-path Acquire calls for the same model
// converge on a single session admission (via the session manager's
// per-entry single-flight) instead of creating duplicate sessions.
package pool

import (
	"context"
	"fmt"
	"freebuff-proxy/backend/internal/testutil"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLeaderElection_BasicLeaderFollower verifies the fundamental flow:
// the first Acquire becomes the leader and creates a session; the second
// Acquire blocks on the leader's channel, then follows the leader's token.
func TestLeaderElection_BasicLeaderFollower(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()

	mock0.SessionCreateDelay = 50 * time.Millisecond
	p := newTestPool(t, mock0, mock1)
	const model = "deepseek/deepseek-v4-pro"

	// First acquire: leader, creates session on Token 0.
	lease1, err := p.Acquire(context.Background(), model)
	if err != nil {
		t.Fatalf("leader acquire failed: %v", err)
	}
	if lease1.Token != 0 {
		t.Fatalf("leader token = %d, want 0", lease1.Token)
	}
	p.LeaseRelease(lease1)

	// Second acquire: should follow leader's token (hot path now).
	lease2, err := p.Acquire(context.Background(), model)
	if err != nil {
		t.Fatalf("follower acquire failed: %v", err)
	}
	if lease2.Token != 0 {
		t.Errorf("follower token = %d, want 0 (should follow leader)", lease2.Token)
	}
	p.LeaseRelease(lease2)

	if mock0.SessionCreates != 1 {
		t.Errorf("mock0 session creates = %d, want 1 (only leader creates)", mock0.SessionCreates)
	}
	if mock1.SessionCreates != 0 {
		t.Errorf("mock1 session creates = %d, want 0", mock1.SessionCreates)
	}
}

// TestLeaderElection_ConcurrentFollowersAllLandOnLeader verifies that when N
// concurrent requests arrive for the same cold model, ALL of them end up on
// the same token (Token 0, strict spill order) and exactly ONE session is
// created.
//
// Determinism: cold-path dedup rides the session manager's per-entry
// single-flight (acquire_route.go), not the old per-model election gate, so
// the exactly-one-session assertion is schedule-independent — a caller
// arriving after the leader completes just rides the hot session on the
// same token.
func TestLeaderElection_ConcurrentFollowersAllLandOnLeader(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()

	hold := make(chan struct{})
	leaderParked := make(chan struct{}, 1)
	p := newTestPool(t, mock0, mock1)
	const model = "deepseek/deepseek-v4-pro"

	const goroutines = 8

	// Park the leader's session create on hold; leaderParked proves the
	// leader is in flight upstream. Followers pile onto the per-entry
	// single-flight behind it (no gate registration to observe).
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/freebuff/session") {
			select {
			case leaderParked <- struct{}{}:
			default:
			}
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Second):
				http.Error(w, "test hold timeout", http.StatusInternalServerError)
				return
			}
			mock0.SessionCreates++
			w.Header().Set("Content-Type", "application/json")
			expiresAt := time.Now().Add(30 * time.Minute).UTC().Format("2006-01-02T15:04:05.000Z07:00")
			_, _ = fmt.Fprintf(w, `{"status":"active","instanceId":"inst-abc-123","expiresAt":"%s"}`, expiresAt)
			return
		}
		if r.Header.Get("x-freebuff-instance-id") == "" {
			mock0.SessionProbes++
		} else {
			mock0.SessionPolls++
		}
		w.Header().Set("Content-Type", "application/json")
		expiresAt := time.Now().Add(30 * time.Minute).UTC().Format("2006-01-02T15:04:05.000Z07:00")
		_, _ = fmt.Fprintf(w, `{"status":"active","instanceId":"inst-abc-123","expiresAt":"%s"}`, expiresAt)
	}

	var wg sync.WaitGroup
	var errs atomic.Int32
	var tokenCounts [2]atomic.Int32

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := p.Acquire(context.Background(), model)
			if err != nil {
				errs.Add(1)
				return
			}
			tokenCounts[lease.Token].Add(1)
			p.LeaseRelease(lease)
		}()
	}
	// The leader must be parked upstream before its response is released;
	// the settle lets followers arrive and park on the single-flight so the
	// exactly-one-create assertion exercises the dedup. The outcome holds
	// regardless — a late follower rides the hot session on token 0.
	select {
	case <-leaderParked:
	case <-time.After(10 * time.Second):
		close(hold)
		t.Fatal("leader session create never reached upstream")
	}
	time.Sleep(200 * time.Millisecond)
	close(hold)
	wg.Wait()

	if errs.Load() > 0 {
		t.Fatalf("%d acquires failed", errs.Load())
	}
	// Direct field reads are safe after wg.Wait (happens-before); snapshot
	// helper would race with unsynchronized SessionHandler increment.
	totalCreates := mock0.SessionCreates + mock1.SessionCreates
	if totalCreates != 1 {
		t.Errorf("total session creates = %d, want 1 (single-flight) mock0=%d mock1=%d", totalCreates, mock0.SessionCreates, mock1.SessionCreates)
	}
	c0 := tokenCounts[0].Load()
	c1 := tokenCounts[1].Load()
	if c0+c1 != goroutines {
		t.Errorf("total leases = %d, want %d (c0=%d c1=%d)", c0+c1, goroutines, c0, c1)
	}
	if c0 != 0 && c0 != goroutines {
		t.Errorf("token 0 leases = %d, want 0 or %d (all on same token)", c0, goroutines)
	}
	if c1 != 0 && c1 != goroutines {
		t.Errorf("token 1 leases = %d, want 0 or %d (all on same token)", c1, goroutines)
	}
	if mock0.SessionCreates == 1 && c0 != goroutines {
		t.Errorf("mock0 created but token 0 leases = %d, want %d", c0, goroutines)
	}
	if mock1.SessionCreates == 1 && c1 != goroutines {
		t.Errorf("mock1 created but token 1 leases = %d, want %d", c1, goroutines)
	}
}

// TestLeaderElection_LeaderFailureFollowersFallThrough verifies that when the
// leader's session creation fails, the gate channel is closed and followers
// fall through to the normal path (trying other tokens).
func TestLeaderElection_LeaderFailureFollowersFallThrough(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()

	// Token 0 rejects session creation (simulates upstream refusal).
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"status":"rate_limited","limit":1,"recentCount":1,"period":"pacific_day","resetAt":"2099-01-01T00:00:00Z","retryAfterMs":3600000}`)
	}
	mock0.SessionCreateDelay = 50 * time.Millisecond

	p := newTestPool(t, mock0, mock1)
	const model = "deepseek/deepseek-v4-pro"

	var wg sync.WaitGroup
	var tokenCounts [2]atomic.Int32
	var errs atomic.Int32

	// Launch 3 concurrent requests. The leader (Token 0) will fail;
	// followers should fall through and land on Token 1.
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := p.Acquire(context.Background(), model)
			if err != nil {
				errs.Add(1)
				return
			}
			tokenCounts[lease.Token].Add(1)
			p.LeaseRelease(lease)
		}()
	}
	wg.Wait()

	// At least one request should succeed on Token 1.
	if tokenCounts[1].Load() == 0 {
		t.Errorf("token 1 leases = 0, want >= 1 (followers should fall through after leader failure)")
	}
	// No request should land on Token 0 (it failed admission).
	if c := tokenCounts[0].Load(); c != 0 {
		t.Errorf("token 0 leases = %d, want 0 (leader failed)", c)
	}
	// Exactly 1 session on Token 1 (the first follower that became leader).
	if mock1.SessionCreates != 1 {
		t.Errorf("mock1 session creates = %d, want 1", mock1.SessionCreates)
	}
}

// TestLeaderElection_IndependentModels verifies that leader election is
// per-model: two concurrent requests for DIFFERENT models elect independent
// leaders and do not block each other.
func TestLeaderElection_IndependentModels(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()

	mock0.SessionCreateDelay = 100 * time.Millisecond
	mock1.SessionCreateDelay = 100 * time.Millisecond

	p := newTestPool(t, mock0, mock1)
	const modelA = "deepseek/deepseek-v4-pro"
	const modelB = "openai/gpt-5.6-luna"

	var wg sync.WaitGroup
	var leaseA, leaseB *Lease
	var errA, errB error

	// Both should run in parallel (not serialized).
	wg.Add(2)
	go func() {
		defer wg.Done()
		leaseA, errA = p.Acquire(context.Background(), modelA)
	}()
	go func() {
		defer wg.Done()
		leaseB, errB = p.Acquire(context.Background(), modelB)
	}()
	wg.Wait()

	if errA != nil {
		t.Fatalf("modelA acquire failed: %v", errA)
	}
	if errB != nil {
		t.Fatalf("modelB acquire failed: %v", errB)
	}
	p.LeaseRelease(leaseA)
	p.LeaseRelease(leaseB)

	// Each model should have created exactly 1 session (on different tokens
	// due to round-robin offset, but at least 1 each).
	totalCreates := mock0.SessionCreates + mock1.SessionCreates
	if totalCreates != 2 {
		t.Errorf("total session creates = %d, want 2 (one per model)", totalCreates)
	}
}

// TestLeaderElection_SequentialAcquiresDoNotBlock verifies that sequential
// acquires never block each other: the second reuses the hot session, so
// exactly one session is created across both.
func TestLeaderElection_SequentialAcquiresDoNotBlock(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()

	p := newTestPool(t, mock0, mock1)
	const model = "deepseek/deepseek-v4-pro"

	// First acquire: cold admit.
	lease1, err := p.Acquire(context.Background(), model)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease1)

	// Second acquire: must not block.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lease2, err := p.Acquire(ctx, model)
	if err != nil {
		t.Fatalf("second acquire blocked or failed: %v", err)
	}
	p.LeaseRelease(lease2)

	if mock0.SessionCreates != 1 {
		t.Errorf("mock0 session creates = %d, want 1 (second acquire reuses the hot session)", mock0.SessionCreates)
	}
}

// TestLeaderElection_HotPathReusesSession verifies that when a token
// already holds an active session for the model, the acquire reuses it
// with no new admission.
func TestLeaderElection_HotPathReusesSession(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()

	p := newTestPool(t, mock0, mock1)
	const model = "deepseek/deepseek-v4-pro"

	// Cold admit on Token 0.
	lease1, err := p.Acquire(context.Background(), model)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease1)

	// Second acquire: hot path (session exists).
	lease2, err := p.Acquire(context.Background(), model)
	if err != nil {
		t.Fatal(err)
	}
	if lease2.Token != 0 {
		t.Errorf("hot path token = %d, want 0", lease2.Token)
	}
	p.LeaseRelease(lease2)

	// Only 1 session created (cold admit), second was hot reuse.
	if mock0.SessionCreates != 1 {
		t.Errorf("mock0 session creates = %d, want 1 (hot path reuses the session)", mock0.SessionCreates)
	}
}

// TestLeaderElection_RapidSequentialAcquires verifies that a rapid sequence
// of acquires never gets stuck: the first cold-admits, the rest reuse the
// hot session.
func TestLeaderElection_RapidSequentialAcquires(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()

	p := newTestPool(t, mock0, mock1)
	const model = "deepseek/deepseek-v4-pro"

	for i := range 10 {
		lease, err := p.Acquire(context.Background(), model)
		if err != nil {
			t.Fatalf("acquire %d failed: %v", i, err)
		}
		if lease.Token != 0 {
			t.Errorf("acquire %d token = %d, want 0", i, lease.Token)
		}
		p.LeaseRelease(lease)
	}

	// Exactly 1 session created (first cold admit, rest hot reuse).
	if mock0.SessionCreates != 1 {
		t.Errorf("mock0 session creates = %d, want 1", mock0.SessionCreates)
	}
}

// TestLeaderElection_ThreeTokensConcurrent verifies leader election with 3
// tokens: all concurrent requests converge on the leader's token.
func TestLeaderElection_ThreeTokensConcurrent(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mock2 := testutil.NewMock()
	defer mock2.Close()

	mock0.SessionCreateDelay = 100 * time.Millisecond
	p := newTestPool(t, mock0, mock1, mock2)
	const model = "deepseek/deepseek-v4-pro"

	const goroutines = 6
	var wg sync.WaitGroup
	var errs atomic.Int32
	var counts [3]atomic.Int32

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := p.Acquire(context.Background(), model)
			if err != nil {
				errs.Add(1)
				return
			}
			counts[lease.Token].Add(1)
			p.LeaseRelease(lease)
		}()
	}
	wg.Wait()

	if errs.Load() > 0 {
		t.Fatalf("%d acquires failed", errs.Load())
	}
	// Exactly 1 session created across all 3 tokens.
	totalCreates := mock0.SessionCreates + mock1.SessionCreates + mock2.SessionCreates
	if totalCreates != 1 {
		t.Errorf("total session creates = %d, want 1 (single-flight across 3 tokens)", totalCreates)
	}
	// All leases should be on the same token (the leader's).
	leaderToken := -1
	for i := range counts {
		if counts[i].Load() > 0 {
			if leaderToken == -1 {
				leaderToken = i
			} else if i != leaderToken {
				t.Errorf("leases split across tokens %d and %d (want all on one)", leaderToken, i)
			}
		}
	}
	if leaderToken == -1 {
		t.Error("no leases acquired")
	}
}

// TestLeaderElection_ContextCanceledDuringWait verifies that a follower whose
// context is canceled while waiting on the leader's channel returns an error
// instead of hanging forever.
func TestLeaderElection_ContextCanceledDuringWait(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()

	// Slow leader: 2s delay on session create.
	mock0.SessionCreateDelay = 2 * time.Second

	p := newTestPool(t, mock0, mock1)
	const model = "deepseek/deepseek-v4-pro"

	// Start leader (will block for 2s on session create).
	go func() {
		lease, err := p.Acquire(context.Background(), model)
		if err == nil {
			p.LeaseRelease(lease)
		}
	}()

	// Give leader time to register.
	time.Sleep(20 * time.Millisecond)

	// Follower with short timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := p.Acquire(ctx, model)
	// Should either fail with context error or succeed if leader finishes fast.
	// The key assertion: it must NOT hang forever.
	if err != nil && ctx.Err() == nil {
		t.Errorf("unexpected error: %v", err)
	}
}
