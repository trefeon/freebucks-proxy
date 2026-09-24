package pool

import (
	"context"
	"errors"
	"freebucks-proxy/backend/internal/session"
	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCooldownToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	p.CooldownToken(0, time.Hour)
	snap := p.Snapshot()[0]
	if snap.CooldownUntil.Before(time.Now().Add(59 * time.Minute)) {
		t.Errorf("cooldown until = %v, want ~now+1h", snap.CooldownUntil)
	}

	_, err := p.Acquire(context.Background(), modelA)
	if err == nil {
		t.Fatal("want error while the only token is cooling down")
		return
	}
	if !strings.Contains(err.Error(), "cooling down") {
		t.Errorf("error = %q, want cooldown message", err)
	}

	// Out-of-range tokens are ignored without panicking.
	p.CooldownToken(99, time.Hour)
	p.CooldownToken(-1, time.Hour)
}

func TestAcquireBanCooldowns(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.Ban = true
	p := newTestPool(t, mock)

	_, err := p.Acquire(context.Background(), modelA)
	var be *upstream.BanError
	if !errors.As(err, &be) {
		t.Fatalf("want *upstream.BanError, got %v", err)
	}
	if !errors.Is(err, upstream.ErrBanned) {
		t.Errorf("errors.Is(ErrBanned) = false")
	}

	// The token cooled down for the ban window, so subsequent acquires skip
	// it AND still surface the remembered 403 banned.
	snap := p.Snapshot()[0]
	if snap.CooldownUntil.Before(time.Now().Add(59 * time.Minute)) {
		t.Errorf("cooldown until = %v, want ~now+1h", snap.CooldownUntil)
	}
	_, err = p.Acquire(context.Background(), modelA)
	var be2 *upstream.BanError
	if !errors.As(err, &be2) {
		t.Fatalf("second acquire: want *upstream.BanError, got %v", err)
	}
	if !errors.Is(err, upstream.ErrBanned) {
		t.Errorf("second acquire errors.Is(ErrBanned) = false")
	}
}

func TestIdleRotationFinishesRuns(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	cfg := p.cfg.Load()
	cfg.IdleRotationTimeout = 10 * time.Millisecond
	p.cfg.Store(cfg)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)
	if got := mock.StartedRunsSnapshot(); len(got) != 1 {
		t.Fatalf("started runs = %v, want 1", got)
	}

	// Not idle yet: a maintain pass runs normally (no FINISH — no pruner
	// children exist in the newest CLI, so any FINISH here would be a parent run).
	p.maintainTick(context.Background())
	if got := mock.FinishedRunsSnapshot(); len(got) != 0 {
		t.Fatalf("finished runs = %v before idle, want none", got)
	}

	// Past the idle threshold: one pass FINISHes all runs. The threshold is
	// crossed by mutating lastActive (deterministic; a fixed sleep would
	// race the 10ms threshold on slow CI).
	p.lastActiveMu.Lock()
	p.lastActive = time.Now().Add(-time.Second)
	p.lastActiveMu.Unlock()
	p.maintainTick(context.Background())
	finished := mock.FinishedRunsSnapshot()
	if len(finished) != 1 || finished[0].Status != "completed" {
		t.Fatalf("finished runs after idle = %v, want 1 completed", finished)
	}

	// ...and later idle passes stay dormant (no further FINISH or START).
	for i := 0; i < 2; i++ {
		p.maintainTick(context.Background())
	}
	if got := mock.FinishedRunsSnapshot(); len(got) != 1 {
		t.Errorf("finished runs = %v, want still 1 parent (dormant while idle)", got)
	}
	if got := mock.StartedRunsSnapshot(); len(got) != 1 {
		t.Errorf("started runs = %v, want still 1", got)
	}

	// The next request re-creates the run on demand.
	lease, err = p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)
	if got := mock.StartedRunsSnapshot(); len(got) != 2 {
		t.Errorf("started runs = %v, want 2 (re-created on demand)", got)
	}
}

// TestCooldownTokenBanQuarantinesLiveBansOnly pins the quarantine gate: a
// ban only marks the token terminal while it is still live after
// runs.CooldownBan — a hard ban (no resumes_at) stays live forever and
// quarantines, a future resumes_at quarantines for the window, but an
// EXPIRED temporary ban (resumes_at in the past) is already lifted
// upstream and must not quarantine a healthy token.
func TestCooldownTokenBanQuarantinesLiveBansOnly(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)

	// Hard ban: permanent terminal state → quarantined.
	p.CooldownTokenBan(0, &upstream.BanError{Body: "banned"})
	if !p.Snapshot()[0].Quarantined {
		t.Error("hard ban (no resumes_at) did not quarantine the token")
	}

	// Expired temporary ban: already lifted upstream → NOT quarantined.
	p.CooldownTokenBan(1, &upstream.BanError{Body: "banned", ResumesAt: time.Now().Add(-time.Hour)})
	if q := p.Snapshot()[1]; q.Quarantined || q.QuarantineReason != "" {
		t.Errorf("expired temporary ban quarantined the token: %+v", q)
	}

	// The lifted token is immediately usable again.
	_, err := p.Acquire(context.Background(), modelB)
	if err != nil {
		t.Fatalf("acquire on lifted token: %v (want success after expired temp ban)", err)
	}
}

// TestIdleRotationSkipsInflight is the regression guard for the idle
// rotation bug: the idle FINISH pass used to FinishAllRuns every token,
// killing in-flight chats. Tokens holding a lease must be skipped — their
// runs stay live until the lease drains (mirrors the bridge idle sweep's
// busy-entry rule).
func TestIdleRotationSkipsInflight(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	cfg := p.cfg.Load()
	cfg.IdleRotationTimeout = 10 * time.Millisecond
	p.cfg.Store(cfg)

	// Acquire a lease and HOLD it: the run stays in the run manager.
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	defer p.LeaseRelease(lease)
	if got := mock.StartedRunsSnapshot(); len(got) != 1 {
		t.Fatalf("started runs = %v, want 1", got)
	}

	// Past the idle threshold: an idle pass must NOT FINISH the held run.
	// The threshold is crossed by mutating lastActive (deterministic; a
	// fixed sleep would race the 10ms threshold on slow CI).
	p.lastActiveMu.Lock()
	p.lastActive = time.Now().Add(-time.Second)
	p.lastActiveMu.Unlock()
	p.maintainTick(context.Background())
	if got := mock.FinishedRunsSnapshot(); len(got) != 0 {
		t.Fatalf("finished runs = %v, want none (in-flight lease held)", got)
	}

	// The held lease's run is still live in the manager.
	if got := p.Snapshot()[0].ActiveRuns; got != 1 {
		t.Errorf("ActiveRuns = %d, want 1 (run not finished)", got)
	}
}

func TestIdleRotationDisabled(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock) // IdleRotationTimeout = 0: never idle-pauses

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)

	p.maintainTick(context.Background())
	// With idle rotation off no PARENT run may be finished (and no pruner
	// children exist in the newest CLI).
	if got := mock.FinishedRunsSnapshot(); len(got) != 0 {
		t.Fatalf("finished runs = %v with idle rotation disabled, want none", got)
	}
}

func TestMaintainTickSkipsCooldownToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	// An active session + run: a normal maintain pass would poll the
	// session (GET) and may rotate the run. With the token cooling down
	// neither the maintain pass nor the session-poll pass may touch the
	// upstream at all.
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)

	p.CooldownToken(0, time.Hour)

	before := mock.RequestsSnapshot()
	p.maintainTick(context.Background())
	p.sessionPollTick(context.Background())
	if got := mock.RequestsSnapshot(); got != before {
		t.Errorf("upstream requests during cooldown maintain = %d, want %d (no poll/rotate)", got, before)
	}
}

func TestPoolCooldownRateLimitAndBan(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	rle := &upstream.RateLimitError{Body: "rate limit", RetryAfter: 10 * time.Minute}
	p.CooldownTokenRateLimit(0, rle)

	be := &upstream.BanError{Body: "account banned", ResumesAt: time.Now().Add(1 * time.Hour)}
	p.CooldownTokenBan(0, be)

	snap := p.Snapshot()[0]
	if !snap.Quarantined {
		t.Error("token not quarantined during active ban window, want quarantined")
	}
	if snap.BanType != "temporary" {
		t.Errorf("BanType = %q, want temporary (active ban outranks the cooldown)", snap.BanType)
	}
}

func TestMultiTokenRateLimitAndBanFailover(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()

	p := newTestPool(t, mock0, mock1)

	rle := &upstream.RateLimitError{Body: "rate limit", RetryAfter: 10 * time.Minute}
	be := &upstream.BanError{Body: "banned", ResumesAt: time.Now().Add(1 * time.Hour)}

	p.CooldownTokenRateLimit(0, rle)
	p.CooldownTokenRateLimit(1, rle)

	_, err := p.Acquire(context.Background(), modelA)
	if err == nil || !errors.Is(err, upstream.ErrRateLimited) {
		t.Errorf("Acquire with all rate limited = %v, want rate limit error", err)
	}

	p.CooldownTokenBan(0, be)
	p.CooldownTokenBan(1, be)

	_, err = p.Acquire(context.Background(), modelA)
	if err == nil || !errors.Is(err, upstream.ErrBanned) {
		t.Errorf("Acquire with all banned = %v, want ban error", err)
	}
}

// TestAcquirePrecedenceRateOverWaiting pins rate > waiting: with one token
// queued and another rate-limited, the remembered 429 wins.
func TestAcquirePrecedenceRateOverWaiting(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.SessionMode = "queued"
	mock0.QueuePosition = 1
	mock0.QueueDepth = 3
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)
	p.CooldownTokenRateLimit(1, &upstream.RateLimitError{Body: "rate limit", RetryAfter: 10 * time.Minute})

	_, err := p.Acquire(context.Background(), modelA)
	if err == nil || !errors.Is(err, upstream.ErrRateLimited) {
		t.Fatalf("waiting + rate-limited = %v, want rate limit (precedence over waiting)", err)
		return
	}
}

// TestIdleFinishAllRunsHonorsMaintainCtx is the regression guard for the
// context.Background bug in the idle FINISH: Pool.Shutdown cancels the
// maintain ctx first and waits on the maintain goroutine, so a mid-drain
// FinishAllRuns must abort on cancel instead of blocking shutdown for the
// full upstream call timeout.
func TestIdleFinishAllRunsHonorsMaintainCtx(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	cfg := p.cfg.Load()
	cfg.IdleRotationTimeout = time.Millisecond
	p.cfg.Store(cfg)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)

	// Cross the idle threshold by mutating lastActive (deterministic; a
	// fixed sleep would race the 1ms threshold on slow CI).
	p.lastActiveMu.Lock()
	p.lastActive = time.Now().Add(-time.Second)
	p.lastActiveMu.Unlock()

	// Hold every FINISH upstream: only ctx cancellation can end it.
	mock.SetFinishDelay(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.maintainTick(ctx)
		close(done)
	}()

	eventually(t, "idle FINISH in flight", func() bool {
		return mock.FinishesStartedSnapshot() >= 1
	})
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("maintainTick did not return after ctx cancel (FinishAllRuns used context.Background)")
	}
}

// ── Wave 1 issue tests (#81, #77) ───────────────────────────────────────────────────────────────────────

// TestAcquireIpCappedSurfacesDistinctError verifies #81: an ip_capped
// admission refusal surfaces the distinct IpCappedError (never folded into
// the generic rate-limit bucket) carrying the body's retryAfterMs. MASQ
// writes no cooldown for it: a correlative egress-IP refusal would hit the
// same wall on the next account, so there is no failover walk and no
// cooldown memory — every pass re-tries live and surfaces the same shape.
func TestAcquireIpCappedSurfacesDistinctError(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"status":"ip_capped","activeUsersForIp":7,"limit":4,"retryAfterMs":45000}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ended"}`)
	}
	p := newTestPool(t, mock)

	_, err := p.Acquire(context.Background(), modelA)
	if errors.Is(err, upstream.ErrRateLimited) {
		t.Fatal("ip_capped surfaced as ErrRateLimited, want distinct ErrIpCapped")
	}
	var ice *upstream.IpCappedError
	if !errors.As(err, &ice) {
		t.Fatalf("want *upstream.IpCappedError, got %v", err)
	}
	if !errors.Is(err, upstream.ErrIpCapped) {
		t.Error("not unwrap-able to ErrIpCapped")
	}
	if ice.ActiveUsersForIP != 7 || ice.Limit != 4 {
		t.Errorf("IpCappedError = %+v, want ActiveUsersForIP 7 limit 4", ice)
	}
	if ice.RetryAfter != 45*time.Second {
		t.Errorf("RetryAfter = %s, want 45s (bounded to retryAfterMs)", ice.RetryAfter)
	}

	// A second acquire re-tries live (no cooldown memory) and surfaces the
	// same distinct shape, not a generic cooldown 502.
	_, err = p.Acquire(context.Background(), modelA)
	var ice2 *upstream.IpCappedError
	if !errors.As(err, &ice2) {
		t.Fatalf("second acquire: want *upstream.IpCappedError, got %v", err)
	}
}

// TestSessionPollSkipsWhileChatInFlight verifies #77: the session-liveness
// poll is skipped while any run holds an in-flight lease (a poll landing
// mid-chat can kick the active session with 428), and resumes once the lease
// drains.
func TestSessionPollSkipsWhileChatInFlight(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	// Admit an active session; the lease holds InflightCount() > 0.
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	if lease == nil || lease.Run == nil {
		t.Fatal("nil lease/run")
		return
	}

	before := mock.SessionPolls
	p.sessionPollTick(context.Background())
	if got := mock.SessionPolls; got != before {
		t.Errorf("session polls during in-flight chat = %d, want %d (poll skipped)", got, before)
	}

	// Release the lease: the next poll pass polls again.
	p.LeaseRelease(lease)
	p.sessionPollTick(context.Background())
	if got := mock.SessionPolls; got <= before {
		t.Errorf("session polls after release = %d, want > %d (poll resumed)", got, before)
	}
}

// TestSessionPollSchedule pins the liveness-poll cadence helpers
// (upstream/freebuff sdk polling-backoff.ts): the success interval is ~30s
// ±20% jitter capped to remaining+1s near expiry, and the failure backoff
// grows 20s→300s while never scheduling a retry before the server's
// Retry-After floor.
func TestSessionPollSchedule(t *testing.T) {
	t.Run("success interval jittered around 30s", func(t *testing.T) {
		for i := 0; i < 50; i++ {
			d := sessionPollSuccessDelay(session.SessionSnapshot{})
			if d < 24*time.Second || d > 36*time.Second {
				t.Fatalf("success delay = %s, want 30s ±20%%", d)
			}
		}
	})

	t.Run("success interval capped near expiry", func(t *testing.T) {
		rem := 10 * time.Second
		d := sessionPollSuccessDelay(session.SessionSnapshot{ExpiresAt: time.Now().Add(rem)})
		// remaining+1s (clock-drift tolerant: allow a few ms either side of
		// the two time.Now() samples).
		if d < rem || d > rem+2*time.Second {
			t.Errorf("success delay near expiry = %s, want ≈ %s (remaining+1s)", d, rem+time.Second)
		}
	})

	t.Run("failure backoff doubles and caps at 300s", func(t *testing.T) {
		cases := []struct {
			failures int
			min      time.Duration
			max      time.Duration
		}{
			{1, 10 * time.Second, 20 * time.Second},   // 20s, lower-half jitter
			{2, 20 * time.Second, 40 * time.Second},   // 40s
			{3, 40 * time.Second, 80 * time.Second},   // 80s
			{6, 150 * time.Second, 300 * time.Second}, // capped at 300s
		}
		for _, tc := range cases {
			d := sessionPollBackoffDelay(tc.failures, 0)
			if d < tc.min || d > tc.max {
				t.Errorf("backoff(%d) = %s, want [%s, %s]", tc.failures, d, tc.min, tc.max)
			}
		}
	})

	t.Run("failure backoff honors Retry-After floor", func(t *testing.T) {
		for i := 0; i < 50; i++ {
			d := sessionPollBackoffDelay(1, 60*time.Second)
			// retryAfter jittered UP only ([1.0, 1.2]x = [60s, 72s]), max'd
			// with the 20s base backoff — never before the floor the server
			// named (failedPollDelayMs floors at 1.0x, never 0.8x).
			if d < 60*time.Second || d > 300*time.Second {
				t.Errorf("backoff with Retry-After 60s = %s, want ≥ 60s (never before the floor)", d)
			}
			if d > 72*time.Second && d < 300*time.Second {
				// Above the jittered floor only via the 300s cap path is
				// impossible here (60s*1.2=72s << cap); anything in
				// (72s, 300s) is a shape violation.
				t.Errorf("backoff with Retry-After 60s = %s, want ≤ 72s (floor jitter caps at 1.2x)", d)
			}
		}
	})

	t.Run("retry-after extracted from classified errors", func(t *testing.T) {
		if got := sessionPollRetryAfter(&upstream.UpstreamError{Status: 503, RetryAfter: 45 * time.Second}); got != 45*time.Second {
			t.Errorf("UpstreamError RetryAfter = %s, want 45s", got)
		}
		if got := sessionPollRetryAfter(&upstream.RateLimitError{RetryAfter: 90 * time.Second}); got != 90*time.Second {
			t.Errorf("RateLimitError RetryAfter = %s, want 90s", got)
		}
		if got := sessionPollRetryAfter(errors.New("plain")); got != 0 {
			t.Errorf("plain error RetryAfter = %s, want 0", got)
		}
	})
}

// TestBanViewDerivation pins the #198/#199 snapshot ban view: the type is
// read off BanError.ResumesAt (NOT the folded cooldown deadline, which
// runs.CooldownBan sets to now+24h even for hard bans), a temporary ban
// carries its resumes_at deadline, and expired/absent bans yield zero
// values.
func TestBanViewDerivation(t *testing.T) {
	until := time.Now().Add(time.Hour)

	banType, bannedUntil := banView(&upstream.BanError{Body: "banned", ResumesAt: until}, until.Add(24*time.Hour))
	if banType != "temporary" || !bannedUntil.Equal(until) {
		t.Errorf("temporary ban view = %q/%s, want temporary/%s", banType, bannedUntil, until)
	}

	banType, bannedUntil = banView(&upstream.BanError{Body: "banned"}, time.Now().Add(24*time.Hour))
	if banType != "hard" || !bannedUntil.IsZero() {
		t.Errorf("hard ban view = %q/%s, want hard/zero", banType, bannedUntil)
	}

	banType, bannedUntil = banView(nil, time.Time{})
	if banType != "" || !bannedUntil.IsZero() {
		t.Errorf("no-ban view = %q/%s, want empty/zero", banType, bannedUntil)
	}

	expired := time.Now().Add(-time.Minute)
	banType, _ = banView(&upstream.BanError{Body: "banned", ResumesAt: expired}, expired)
	if banType != "" {
		t.Errorf("expired ban view = %q, want empty", banType)
	}
}
