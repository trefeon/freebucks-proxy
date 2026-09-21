package session

// Wave-3 session tests: pre-emptive async re-admit before expiry (#99) and
// the admission probe cache TTL (#60).

import (
	"context"
	"encoding/json"
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func newTestSession(t *testing.T, mock *testutil.MockUpstream) *Manager {
	t.Helper()
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
	return NewManager(client)
}

// TestReAdmitTriggersAsyncAndServesNewInstance pins issue #99 with the
// anti-supersede handover: with the re-admit lead larger than the session's
// remaining life, the request that trips the trigger is served by the NEW
// instance once the admission lands. It must NOT be handed the row it just
// superseded — upstream rotates the account's single seat on every admission
// and 409s the disowned instance's next completion, which is exactly the
// live 2026-09-21 503 session_superseded pair. Exactly one upstream create
// pays for the handover.
func TestReAdmitTriggersAsyncAndServesNewInstance(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var creates atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusOK, map[string]any{"status": "active", "instanceId": "inst-x", "expiresAt": time.Now().Add(30 * time.Minute).Format(time.RFC3339)})
			return
		}
		n := creates.Add(1)
		id := "inst-1"
		if n >= 2 {
			id = "inst-2"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "active",
			"instanceId": id,
			"expiresAt":  time.Now().Add(10 * time.Second).Format(time.RFC3339),
		})
	}
	m := newTestSession(t, mock)
	m.SetReAdmitLead(time.Minute) // 10s remaining << 60s lead

	first, err := m.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != "inst-1" {
		t.Fatalf("first instance = %q, want inst-1", first)
	}
	// The second call trips the re-admit and is served by the instance it
	// lands: the caller waits on the single-flight refresh instead of riding
	// the superseded row.
	second, err := m.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second != "inst-2" {
		t.Fatalf("trigger request instance = %q, want inst-2 (serve the admission it tripped)", second)
	}
	if n := creates.Load(); n != 2 {
		t.Fatalf("session creates = %d, want 2 (the handover is one admission)", n)
	}
}

// TestReAdmitFailureRidesOldSession pins the failure path: when the async
// re-admit fails, a request that parks on the single-flight refresh must
// ride the still-usable old session instead of surfacing the error.
func TestReAdmitFailureRidesOldSession(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var creates atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusOK, map[string]any{"status": "active", "instanceId": "inst-1", "expiresAt": time.Now().Add(30 * time.Minute).Format(time.RFC3339)})
			return
		}
		if creates.Add(1) >= 2 {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"status": "rate_limited"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "active",
			"instanceId": "inst-1",
			"expiresAt":  time.Now().Add(10 * time.Second).Format(time.RFC3339),
		})
	}
	m := newTestSession(t, mock)
	m.SetReAdmitLead(time.Minute)

	first, err := m.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != "inst-1" {
		t.Fatalf("first = %q", first)
	}
	// The second call hits the cached-active branch and triggers the async
	// re-admit (which fails) while riding the old session.
	if got, err := m.EnsureSession(context.Background()); err != nil || got != "inst-1" {
		t.Fatalf("second EnsureSession = %q/%v, want inst-1 ride", got, err)
	}
	eventually(t, "re-admit failure recorded", func() bool { return creates.Load() >= 2 })
	// The old session is still usable: the next request rides it.
	got, err := m.EnsureSession(context.Background())
	if err != nil {
		t.Fatalf("EnsureSession after failed re-admit = %v, want ride old", err)
	}
	if got != "inst-1" {
		t.Fatalf("instance after failed re-admit = %q, want inst-1", got)
	}
}

// TestReAdmitDisabledByDefault pins that the re-admit lead is opt-in: with
// the default (0) no extra session create happens on a fresh session.
func TestReAdmitDisabledByDefault(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	m := newTestSession(t, mock)
	first, err := m.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first == "" {
		t.Fatal("no instance")
	}
	time.Sleep(50 * time.Millisecond)
	if mock.SessionCreates != 1 {
		t.Fatalf("session creates = %d, want 1 (no async re-admit)", mock.SessionCreates)
	}
}

// TestGraceWindowTriggersAsyncReAdmitAndHandsOver pins issue #163 with the
// anti-supersede handover: when a request arrives while the cached session is
// being ridden only through its grace drain (a long stream crossed the
// expiresAt boundary), it trips the re-admit and is served by the FRESH
// instance the handover lands — no synchronous admission (waiting room) at
// grace end, no request riding the row the handover supersedes, and exactly
// one create.
func TestGraceWindowTriggersAsyncReAdmitAndHandsOver(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var creates atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		creates.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "active",
			"instanceId": "inst-new",
			"expiresAt":  time.Now().Add(30 * time.Minute).Format(time.RFC3339),
		})
	}
	m := newTestSession(t, mock)
	m.SetReAdmitLead(time.Minute)

	// Seed an expired-but-in-grace cache: the shape left by a long stream
	// that crossed expiresAt while chat was still being served.
	m.mu.Lock()
	m.commit(&cachedState{
		status:            "active",
		instanceID:        "inst-grace",
		model:             "deepseek/deepseek-v4-pro",
		expiresAt:         time.Now().Add(-10 * time.Minute),
		gracePeriodEndsAt: time.Now().Add(20 * time.Minute),
	})
	m.mu.Unlock()

	// The first request trips the grace handover and is served by the fresh
	// instance (the old row is the one being superseded).
	instance, err := m.EnsureSessionForModel(context.Background(), "deepseek/deepseek-v4-pro")
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-new" {
		t.Fatalf("first instance = %q, want inst-new (served by the handover it tripped)", instance)
	}
	eventually(t, "grace re-admit create", func() bool { return creates.Load() >= 1 })
	// The next request also gets the new instance.
	eventually(t, "fresh instance served after handover", func() bool {
		id, err := m.EnsureSessionForModel(context.Background(), "deepseek/deepseek-v4-pro")
		return err == nil && id == "inst-new"
	})
	// No storm: the handover consumed exactly one create.
	time.Sleep(150 * time.Millisecond)
	if got := creates.Load(); got != 1 {
		t.Errorf("creates = %d, want 1 (once per expiry, no storm)", got)
	}
}

// TestGraceWindowReAdmitRefusedRidesOldOnce pins the anti-supersede rule
// (issue #163): a grace-window re-admit that the upstream refuses (the old
// session is still authoritative — the create returns ended/superseded/
// none) keeps the old row riding through the drain and fires at most ONCE
// per expiry — no create storm.
func TestGraceWindowReAdmitRefusedRidesOldOnce(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var creates atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		creates.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{"status": "ended"})
	}
	m := newTestSession(t, mock)
	m.SetReAdmitLead(time.Minute)

	m.mu.Lock()
	m.commit(&cachedState{
		status:            "active",
		instanceID:        "inst-grace",
		model:             "deepseek/deepseek-v4-pro",
		expiresAt:         time.Now().Add(-10 * time.Minute),
		gracePeriodEndsAt: time.Now().Add(20 * time.Minute),
	})
	m.mu.Unlock()

	// First request triggers the re-admit (refused upstream) and rides.
	if got, err := m.EnsureSessionForModel(context.Background(), "deepseek/deepseek-v4-pro"); err != nil || got != "inst-grace" {
		t.Fatalf("first = %q/%v, want inst-grace ride", got, err)
	}
	eventually(t, "refused re-admit attempted", func() bool { return creates.Load() >= 1 })
	// The refused re-admit left the old row authoritative: every later
	// request keeps riding it with no re-trigger and no error.
	for i := 0; i < 5; i++ {
		if got, err := m.EnsureSessionForModel(context.Background(), "deepseek/deepseek-v4-pro"); err != nil || got != "inst-grace" {
			t.Fatalf("ride %d = %q/%v, want inst-grace", i, got, err)
		}
	}
	time.Sleep(150 * time.Millisecond)
	if got := creates.Load(); got != 1 {
		t.Errorf("creates = %d, want 1 (refused re-admit fires once)", got)
	}
}

// TestGraceWindowReAdmitQueuedRidesOldOnce pins the queued side of the
// anti-supersede rule (issue #163): a grace-window re-admit that lands in
// the upstream waiting room must NOT displace the ridden session — the
// next request keeps riding the old row through the drain instead of
// surfacing waiting-room latency, and the re-admit fires only once.
func TestGraceWindowReAdmitQueuedRidesOldOnce(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var creates atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		creates.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"status":          "queued",
			"instanceId":      "inst-q",
			"position":        3,
			"queueDepth":      7,
			"estimatedWaitMs": 60000,
			"pollAt":          time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	}
	m := newTestSession(t, mock)
	m.SetReAdmitLead(time.Minute)

	m.mu.Lock()
	m.commit(&cachedState{
		status:            "active",
		instanceID:        "inst-grace",
		model:             "deepseek/deepseek-v4-pro",
		expiresAt:         time.Now().Add(-10 * time.Minute),
		gracePeriodEndsAt: time.Now().Add(20 * time.Minute),
	})
	m.mu.Unlock()

	// First request triggers the re-admit (lands queued) and rides.
	if got, err := m.EnsureSessionForModel(context.Background(), "deepseek/deepseek-v4-pro"); err != nil || got != "inst-grace" {
		t.Fatalf("first = %q/%v, want inst-grace ride", got, err)
	}
	eventually(t, "queued re-admit attempted", func() bool { return creates.Load() >= 1 })
	// The queued admission was NOT cached: the next request keeps riding
	// the old row — no WaitingRoomError, no re-trigger.
	for i := 0; i < 5; i++ {
		if got, err := m.EnsureSessionForModel(context.Background(), "deepseek/deepseek-v4-pro"); err != nil || got != "inst-grace" {
			t.Fatalf("ride %d = %q/%v, want inst-grace (no waiting room)", i, got, err)
		}
	}
	time.Sleep(150 * time.Millisecond)
	if got := creates.Load(); got != 1 {
		t.Errorf("creates = %d, want 1 (queued re-admit fires once)", got)
	}
}

// TestPollSkipsWithinProbeTTL pins issue #60(a): session poll GETs within
// the admission probe cache TTL of a successful session response are
// skipped; after the TTL the GET happens.
func TestPollSkipsWithinProbeTTL(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	m := newTestSession(t, mock)
	m.SetAdmissionProbeTTL(time.Hour)

	if _, err := m.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	pollsAfterAdmit := mock.SessionPolls
	if err := m.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionPolls != pollsAfterAdmit {
		t.Errorf("poll within probe TTL issued a GET (polls %d → %d)", pollsAfterAdmit, mock.SessionPolls)
	}
}

// TestPollAfterProbeTTLPolls pins the other side: once the TTL elapses, the
// session poll GET fires.
func TestPollAfterProbeTTLPolls(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	m := newTestSession(t, mock)
	m.SetAdmissionProbeTTL(30 * time.Millisecond)

	if _, err := m.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond) // age past the TTL
	polls := mock.SessionPolls
	if err := m.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionPolls != polls+1 {
		t.Errorf("poll after TTL = %d polls, want %d+1", mock.SessionPolls, polls)
	}
}

// writeJSON renders a JSON response (local helper; testutil's is
// unexported).
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}
