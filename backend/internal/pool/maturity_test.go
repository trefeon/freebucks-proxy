package pool

import (
	"context"
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/registry"
	"freebucks-proxy/backend/internal/upstream"
	"testing"
)

type mockHistorySink struct {
	events []MaturityHistoryEvent
}

func (m *mockHistorySink) RecordMaturity(e MaturityHistoryEvent) {
	m.events = append(m.events, e)
}

func TestMaturityWindow(t *testing.T) {
	cfg := &config.Config{MaturityEnabled: true}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	start, end := p.MaturityWindow()
	if start.IsZero() || end.IsZero() {
		t.Fatalf("MaturityWindow returned zero time: start=%v end=%v", start, end)
	}
	if !end.After(start) {
		t.Fatalf("MaturityWindow end %v not after start %v", end, start)
	}
	diff := end.Sub(start)
	if diff != maturityRunWindow {
		t.Fatalf("MaturityWindow span %v != %v", diff, maturityRunWindow)
	}
}

func TestMaturitySnapshotDefaults(t *testing.T) {
	entry := &tokenEntry{}
	snap := entry.MaturitySnapshot()
	if snap == nil {
		t.Fatal("expected non-nil default MaturitySnapshot")
	}
	if !snap.Enabled {
		t.Error("expected default Enabled to be true")
	}
	if snap.LastResult != "pending" {
		t.Errorf("expected LastResult to be 'pending', got %q", snap.LastResult)
	}

	// When TodayUsed is true upstream, default reports skip:today-used
	entry.SetStreak(&upstream.StreakInfo{Streak: 3, TodayUsed: true})
	snap2 := entry.MaturitySnapshot()
	if snap2.LastResult != "skip:today-used" {
		t.Errorf("expected LastResult to be 'skip:today-used', got %q", snap2.LastResult)
	}
}

func TestForceMaturityTouchSkipsActiveOrUsed(t *testing.T) {
	cfg := &config.Config{
		MaturityEnabled: true,
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &mockHistorySink{}
	p.SetHistorySink(sink)

	// Add 2 mock tokens
	_, err = p.AddToken("tok-active-1")
	if err != nil {
		t.Fatalf("AddToken failed: %v", err)
	}
	_, err = p.AddToken("tok-active-2")
	if err != nil {
		t.Fatalf("AddToken failed: %v", err)
	}

	toks := p.roster.Load()
	if toks == nil || len(*toks) != 2 {
		t.Fatalf("expected 2 tokens in roster, got %v", toks)
	}

	// Mark token 0 as used today via streak
	(*toks)[0].SetStreak(&upstream.StreakInfo{Streak: 5, TodayUsed: true})

	// Run ForceMaturityTouch without forceAll
	results := p.ForceMaturityTouch(context.Background(), false)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	if results[0].Status != "skipped" || results[0].Reason != "skip:today-used" {
		t.Errorf("token 0 expected skip:today-used, got %+v", results[0])
	}
}
