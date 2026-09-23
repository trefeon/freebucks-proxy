package pool

import (
	"context"
	"strings"
	"testing"
	"time"

	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/registry"
	"freebucks-proxy/backend/internal/upstream"
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
	if snap.TouchDay != "" || snap.SlotDay != "" {
		t.Errorf("expected empty TouchDay/SlotDay on pending default, got touch=%q slot=%q", snap.TouchDay, snap.SlotDay)
	}
	// When TodayUsed is true upstream, a nil ledger still reports a
	// day-less preview: previews carry no run-day claim.
	entry.SetStreak(&upstream.StreakInfo{Streak: 3, TodayUsed: true})
	snap2 := entry.MaturitySnapshot()
	if snap2.LastResult != "pending" {
		t.Errorf("expected LastResult to be 'pending', got %q", snap2.LastResult)
	}
	if snap2.ResultDay != "" || snap2.TouchDay != "" || snap2.SlotDay != "" {
		t.Errorf("expected day-less preview, got result=%q touch=%q slot=%q", snap2.ResultDay, snap2.TouchDay, snap2.SlotDay)
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

func TestMaturityTickDoesNotStampSkipsOutsideWindow(t *testing.T) {
	cfg := &config.Config{MaturityEnabled: true}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.AddToken("tok-preview-1"); err != nil {
		t.Fatalf("AddToken failed: %v", err)
	}
	if _, err := p.AddToken("tok-preview-2"); err != nil {
		t.Fatalf("AddToken failed: %v", err)
	}
	toks := p.roster.Load()
	if toks == nil || len(*toks) != 2 {
		t.Fatalf("expected 2 tokens in roster, got %v", toks)
	}
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	nowLA := time.Now().In(la)
	tickNow := time.Date(nowLA.Year(), nowLA.Month(), nowLA.Day(), 12, 0, 0, 0, la)
	if maturityInWindow(tickNow) {
		t.Fatalf("tickNow %v unexpectedly inside maintenance window", tickNow)
	}
	today := pacificDayKey(tickNow)
	// Token 0: proxy-active today (client traffic), streak cache fresh so
	// step 2 never hits the network.
	(*toks)[0].SetStreak(&upstream.StreakInfo{Streak: 5, TodayUsed: false, UpdatedAt: tickNow})
	p.roster.recordChatEntry((*toks)[0])
	// Token 1: upstream used today, streak cache fresh.
	(*toks)[1].SetStreak(&upstream.StreakInfo{Streak: 5, TodayUsed: true, UpdatedAt: tickNow})
	p.maturityTickAt(context.Background(), tickNow)
	for i, tok := range *toks {
		snap := tok.MaturitySnapshot()
		if snap == nil {
			t.Fatalf("token %d: nil snapshot", i)
		}
		if strings.HasPrefix(snap.LastResult, "skip:") && snap.ResultDay == today {
			t.Errorf("token %d: background tick stamped run-day claim %q for %q outside window", i, snap.LastResult, snap.ResultDay)
		}
		if snap.LastResult != "pending" {
			t.Errorf("token %d: expected preview LastResult pending, got %q", i, snap.LastResult)
		}
		if snap.ResultDay != "" || snap.TouchDay != "" || snap.SlotDay != "" {
			t.Errorf("token %d: expected day-less preview, got result=%q touch=%q slot=%q", i, snap.ResultDay, snap.TouchDay, snap.SlotDay)
		}
	}
}
