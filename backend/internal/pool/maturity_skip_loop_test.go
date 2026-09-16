package pool

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/modelcat"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// maturitySkipLoopGate returns a clock 4m30s before the next Pacific
// midnight: inside the nightly window AND inside its 5m firing gate, with
// room for a run of 60s maintain ticks that all stay in the gate — the prod
// pattern of 2026-09-15 (one fire attempt per minute, 23:55:49..23:59:49).
func maturitySkipLoopGate(now time.Time) time.Time {
	_, end := maturityWindowFor(now)
	return end.Add(-4*time.Minute - 30*time.Second)
}

// maturitySkipLoopTouchEvents returns the captured history events of kind
// "touch" (the only kind the fire path emits).
func maturitySkipLoopTouchEvents(t *testing.T, sink *recordingSink) []MaturityHistoryEvent {
	t.Helper()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var out []MaturityHistoryEvent
	for _, e := range sink.got {
		if e.Kind == "touch" {
			out = append(out, e)
		}
	}
	return out
}

// maturitySkipLoopPriceAllUnmetered prices every served unmetered row on
// one token's live meter, so the automatic touch-model pick has no
// price-0 candidate left (the prod state behind the empty model).
func maturitySkipLoopPriceAllUnmetered(p *Pool, token int) {
	allPriced := map[string]float64{}
	for _, id := range modelcat.ServedIDs() {
		if !modelcat.IsPremium(id) {
			allPriced[id] = 5
		}
	}
	toks := p.roster.Load()
	(*toks)[token].sessionMgr().UpdateQuotaFromProbe(&upstream.SessionState{
		Freebucks: &upstream.FreebucksInfo{Balance: 10, Prices: allPriced},
	})
}

// A night whose touch model cannot resolve records the fire-path refusal
// ONCE, not once per 60s maintain tick. Prod: MATURITY_ENABLED=1,
// MATURITY_TOUCH_MODEL=auto, and a token whose meter priced every served
// unmetered row — maturity_events grew by kind=touch
// detail="admit skip:touch-model model=" at exactly 60s intervals for the
// whole firing gate (37 rows for that one token). A config/meter refusal is
// a window-level fact: one record, one log, no per-minute growth.
func TestMaturityRefusalRecordsOncePerWindow(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock)
	p.cfg.Load().MaturityTouchModel = "auto"
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	maturitySkipLoopPriceAllUnmetered(p, 0)
	sink := &recordingSink{}
	p.SetHistorySink(sink)
	var logs bytes.Buffer
	p.logger = slog.New(slog.NewTextHandler(&logs, nil))
	gate := maturitySkipLoopGate(time.Now())
	seedStreak(p, 0, 2, false, gate)
	setMaturitySlot(p, 0, gate.Add(-time.Hour), laDay(gate))

	// Four consecutive maintain ticks inside the firing gate.
	for i := range 4 {
		p.maturityTickAt(context.Background(), gate.Add(time.Duration(i)*time.Minute))
	}

	events := maturitySkipLoopTouchEvents(t, sink)
	if len(events) != 1 {
		t.Fatalf("touch events = %d (%v), want 1 for the window", len(events), events)
	}
	if got, want := events[0].Detail, "admit skip:touch-model reason=fallback:no-unmetered-served"; got != want {
		t.Errorf("refusal detail = %q, want %q", got, want)
	}
	if got := strings.Count(logs.String(), "no admittable"); got != 1 {
		t.Errorf("refusal log lines = %d, want 1", got)
	}
	// The dashboard's last-run line keeps naming the refusal.
	if _, result := maturityResult(p, 0); result != "skip:touch-model" {
		t.Errorf("ledger result = %q, want skip:touch-model", result)
	}
	if got := mock.SessionCreatesSnapshot() + mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("upstream calls = %d, want 0 (refused before admission)", got)
	}
}

// The bound is per distinct refusal content, never per tick: when the
// refusal really changes inside the same night (operator pins an override
// their meter prices), the new fact records too — the bound must not hide a
// transition, only the repeat.
func TestMaturityChangedRefusalRecordsAgain(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock)
	p.cfg.Load().MaturityTouchModel = "auto"
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	maturitySkipLoopPriceAllUnmetered(p, 0)
	sink := &recordingSink{}
	p.SetHistorySink(sink)
	gate := maturitySkipLoopGate(time.Now())
	seedStreak(p, 0, 2, false, gate)
	setMaturitySlot(p, 0, gate.Add(-time.Hour), laDay(gate))

	p.maturityTickAt(context.Background(), gate)
	if err := p.SetMaturity(0, true, 7, MaturityModeUnmetered, modelB); err != nil {
		t.Fatal(err)
	}
	p.maturityTickAt(context.Background(), gate.Add(time.Minute))
	p.maturityTickAt(context.Background(), gate.Add(2*time.Minute))

	events := maturitySkipLoopTouchEvents(t, sink)
	if len(events) != 2 {
		t.Fatalf("touch events = %d (%v), want 2 (nothing resolved, then priced)", len(events), events)
	}
	if !strings.Contains(events[0].Detail, "skip:touch-model") {
		t.Errorf("first refusal = %q, want skip:touch-model", events[0].Detail)
	}
	if !strings.HasPrefix(events[1].Detail, "admit skip:touch-priced") {
		t.Errorf("second refusal = %q, want admit skip:touch-priced", events[1].Detail)
	}
	if _, result := maturityResult(p, 0); result != "skip:touch-priced" {
		t.Errorf("ledger result = %q, want skip:touch-priced", result)
	}
	if got := mock.SessionCreatesSnapshot(); got != 0 {
		t.Errorf("SessionCreates = %d, want 0 (both refusals precede admission)", got)
	}
}

// The refusal bound survives a restart: a process that reboots inside the
// firing gate must not re-record tonight's refusal, exactly like the
// touchDay/slotDay idempotency — otherwise a crash loop inside the window
// would still storm the table.
func TestMaturityRefusalBoundSurvivesRestart(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mem := newMemMaturityStore()

	p1 := newMaturityPool(t, mock)
	p1.SetMaturityStore(mem)
	p1.cfg.Load().MaturityTouchModel = "auto"
	if err := p1.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	maturitySkipLoopPriceAllUnmetered(p1, 0)
	sink1 := &recordingSink{}
	p1.SetHistorySink(sink1)
	gate := maturitySkipLoopGate(time.Now())
	seedStreak(p1, 0, 2, false, gate)
	setMaturitySlot(p1, 0, gate.Add(-time.Hour), laDay(gate))
	p1.maturityTickAt(context.Background(), gate)
	if got := len(maturitySkipLoopTouchEvents(t, sink1)); got != 1 {
		t.Fatalf("pre-restart refusal events = %d, want 1", got)
	}

	p2 := newMaturityPool(t, mock)
	p2.SetMaturityStore(mem)
	p2.cfg.Load().MaturityTouchModel = "auto"
	if err := p2.RestoreMaturity(); err != nil {
		t.Fatalf("RestoreMaturity: %v", err)
	}
	maturitySkipLoopPriceAllUnmetered(p2, 0)
	sink2 := &recordingSink{}
	p2.SetHistorySink(sink2)
	seedStreak(p2, 0, 2, false, gate)
	setMaturitySlot(p2, 0, gate.Add(-time.Hour), laDay(gate))
	p2.maturityTickAt(context.Background(), gate.Add(time.Minute))

	if got := len(maturitySkipLoopTouchEvents(t, sink2)); got != 0 {
		t.Errorf("post-restart refusal events = %d, want 0 (bound persisted)", got)
	}
	if _, result := maturityResult(p2, 0); result != "skip:touch-model" {
		t.Errorf("post-restart ledger = %q, want skip:touch-model", result)
	}
}

// Regression guard for the healthy night: a resolvable model still touches
// exactly once per window across the same run of in-gate ticks, so the
// refusal bound can never swallow a real fire or let a second touch slip.
func TestMaturityHealthyWindowTouchesOnce(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.StreakBody = streakBody(2, false)
	p := newMaturityPool(t, mock)
	sink := &recordingSink{}
	p.SetHistorySink(sink)
	gate := maturitySkipLoopGate(time.Now())
	seedStreak(p, 0, 2, false, gate)
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	setMaturitySlot(p, 0, gate.Add(-time.Hour), laDay(gate))

	for i := range 4 {
		p.maturityTickAt(context.Background(), gate.Add(time.Duration(i)*time.Minute))
	}

	if got := mock.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("SessionCreates = %d, want 1 (one touch per window)", got)
	}
	events := maturitySkipLoopTouchEvents(t, sink)
	if len(events) != 1 || !strings.Contains(events[0].Detail, "admit ok") {
		t.Errorf("touch events = %v, want exactly one admit-ok fire", events)
	}
	if _, result := maturityResult(p, 0); result != "ok" {
		t.Errorf("ledger result = %q, want ok", result)
	}
}
