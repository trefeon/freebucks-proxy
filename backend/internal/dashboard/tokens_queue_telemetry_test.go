package dashboard_test

// Queue telemetry payload tests: the operator reads "is this account
// saturated?" from /admin/api/tokens alone. The payload must therefore carry
// the per-token live-turn/queue numbers (live_turns, queued_waiters,
// oldest_waiter_ms) plus the authoritative posture knobs (queue_wait,
// queue_depth, token_max_concurrent, routing_smart), so the console can say
// who holds a slot and who is still waiting without inventing a value.

import (
	"context"
	"encoding/json"
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/dashboard"
	"freebucks-proxy/backend/internal/pool"
	"freebucks-proxy/backend/internal/registry"
	"freebucks-proxy/backend/internal/session"
	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// queueTelemetryServer mounts the tokens JSON handlers over one pooled token
// with the smart-routing slot wall enabled (cap 1, QUEUE_WAIT 30s), and
// returns the pool so a test can hold or park a live turn.
func queueTelemetryServer(t *testing.T) (*httptest.Server, *pool.Pool) {
	t.Helper()
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	cfg := &config.Config{
		AuthTokens:         []string{"tok-queue-0"},
		ListenAddr:         "127.0.0.1:3457",
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		SlotsPerAccount:    1,
		QueueWait:          30 * time.Second,
		QueueDepth:         16,
	}
	client, err := upstream.New("tok-queue-0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	sess := session.NewManager(client)
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, []*session.Manager{sess}, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, nil)
	mux := http.NewServeMux()
	mux.Handle("GET /admin/api/tokens", d.APIHandler("tokens"))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, p
}

// firstTokenCard returns tokens[0] of a decoded tokens payload.
func firstTokenCard(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	toks, _ := body["tokens"].([]any)
	if len(toks) != 1 {
		t.Fatalf("tokens length = %d, want 1: %v", len(toks), body)
	}
	card, _ := toks[0].(map[string]any)
	if card == nil {
		t.Fatalf("tokens[0] is not an object: %v", toks[0])
	}
	return card
}

// TestTokensPayloadCarriesQueueTelemetry pins the additive payload contract:
// the queue numbers ride every token card (full AND live view, since they
// change per second) and the posture knobs ride the payload top level.
func TestTokensPayloadCarriesQueueTelemetry(t *testing.T) {
	ts, _ := queueTelemetryServer(t)

	for _, url := range []string{ts.URL + "/admin/api/tokens", ts.URL + "/admin/api/tokens?view=live"} {
		body := getJSON(t, url)
		if got := body["queue_wait"]; got != "30s" {
			t.Errorf("%s: queue_wait = %v, want \"30s\"", url, got)
		}
		if got := body["queue_depth"]; got != float64(16) {
			t.Errorf("%s: queue_depth = %v, want 16", url, got)
		}
		if got := body["slots_per_account"]; got != float64(1) {
			t.Errorf("%s: slots_per_account = %v, want 1", url, got)
		}
		if got := body["max_spill_accounts"]; got != float64(0) {
			t.Errorf("%s: max_spill_accounts = %v, want 0", url, got)
		}
		card := firstTokenCard(t, body)
		for _, key := range []string{"live_turns", "queued_waiters", "oldest_waiter_ms"} {
			if _, ok := card[key]; !ok {
				t.Errorf("%s: token card missing %q: %v", url, key, card)
			}
		}
	}
}

// TestTokensPayloadShowsSaturatedAccount proves the numbers are real, not
// placeholders: with the lane cap at 1, the account holding the live turn
// reports live_turns=1, and once a second request parks behind it the
// payload reports the waiting request and how long it has waited.
func TestTokensPayloadShowsSaturatedAccount(t *testing.T) {
	ts, p := queueTelemetryServer(t)
	const model = "deepseek/deepseek-v4-flash"

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	holder, err := p.Acquire(ctx, model)
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}
	t.Cleanup(func() { p.LeaseRelease(holder) })

	// Park a second request behind the live turn.
	parkedDone := make(chan struct{})
	go func() {
		defer close(parkedDone)
		_, _ = p.Acquire(ctx, model)
	}()
	t.Cleanup(func() {
		cancel()
		<-parkedDone
	})

	deadline := time.Now().Add(5 * time.Second)
	var card map[string]any
	for {
		card = firstTokenCard(t, getJSON(t, ts.URL+"/admin/api/tokens"))
		live, _ := card["live_turns"].(float64)
		queued, _ := card["queued_waiters"].(float64)
		oldest, _ := card["oldest_waiter_ms"].(float64)
		// The wait is reported at millisecond resolution, so it reads 0 for
		// the very first instant after parking: require the payload to
		// catch up with the real elapsed park rather than accepting a
		// constant. 20ms is well inside the 5s deadline.
		if live == 1 && queued >= 1 && oldest >= 20 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tokens payload never reported the parked waiter: live_turns=%v queued_waiters=%v oldest_waiter_ms=%v (%v)",
				card["live_turns"], card["queued_waiters"], card["oldest_waiter_ms"], card)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The parked request's own body must decode too (no shape drift).
	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) {
		t.Fatalf("token card is not valid JSON: %s", raw)
	}
}
