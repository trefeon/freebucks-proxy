package pool

import (
	"context"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
)

// Every ledger write stamps tonight's Pacific day: a pre-flight skip, a
// window-exhausted skip, and a live fire all serve result_day == tonight.
func TestMaturityResultDayStampedOnWrites(t *testing.T) {
	t.Run("preflight skip", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		p := newMaturityPool(t, mock)
		now := fireGateClock(10 * time.Minute)
		seedFireReady(t, p, 0, 2, now)

		p.maturityTickAt(context.Background(), now)

		if _, result := maturityResult(p, 0); result != "skip:preflight" {
			t.Fatalf("result = %q, want skip:preflight", result)
		}
		if got := p.Snapshot()[0].Maturity.ResultDay; got != laDay(now) {
			t.Errorf("result_day = %q, want %q (tonight)", got, laDay(now))
		}
	})

	t.Run("window-exhausted skip", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		p := newMaturityPool(t, mock)
		pre := fireGateClock(10 * time.Minute)
		seedFireReady(t, p, 0, 2, pre)
		p.maturityTickAt(context.Background(), pre)

		p.maturityTickAt(context.Background(), fireGateClock(30*time.Second))

		if _, result := maturityResult(p, 0); result != "skip:window-exhausted" {
			t.Fatalf("result = %q, want skip:window-exhausted", result)
		}
		if got := p.Snapshot()[0].Maturity.ResultDay; got != laDay(pre) {
			t.Errorf("result_day = %q, want %q (tonight)", got, laDay(pre))
		}
	})

	t.Run("live fire", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		p := newMaturityPool(t, mock)
		now := windowNow()
		seedStreak(p, 0, 2, false, now)
		if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
			t.Fatal(err)
		}
		setMaturitySlot(p, 0, now.Add(-time.Hour), laDay(now))

		p.maturityTickAt(context.Background(), now)

		if action, result := maturityResult(p, 0); action != "admit" || result != "ok" {
			t.Fatalf("touch = %q/%q, want admit/ok", action, result)
		}
		snap := p.Snapshot()[0].Maturity
		if snap.TouchDay == "" || snap.ResultDay != snap.TouchDay {
			t.Errorf("result_day = %q touch_day = %q, want equal after a same-day touch", snap.ResultDay, snap.TouchDay)
		}
	})
}

// A restart restores the result day with the ledger: the dashboard keeps
// scoping the row to the right night across reboots.
func TestMaturityResultDaySurvivesRestart(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mem := newMemMaturityStore()

	p1 := newMaturityPool(t, mock)
	p1.SetMaturityStore(mem)
	now := windowNow()
	seedStreak(p1, 0, 2, false, now)
	if err := p1.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	setMaturitySlot(p1, 0, now.Add(-time.Hour), laDay(now))
	p1.maturityTickAt(context.Background(), now)
	if _, result := maturityResult(p1, 0); result != "ok" {
		t.Fatalf("p1 result = %q, want ok", result)
	}

	p2 := newMaturityPool(t, mock)
	p2.SetMaturityStore(mem)
	if err := p2.RestoreMaturity(); err != nil {
		t.Fatalf("RestoreMaturity: %v", err)
	}
	snap := p2.Snapshot()[0].Maturity
	if snap == nil {
		t.Fatal("restored snapshot is nil, want the pre-restart touch")
	}
	if snap.ResultDay != laDay(now) {
		t.Errorf("restored result_day = %q, want %q (tonight)", snap.ResultDay, laDay(now))
	}
}

// Pre-upgrade rows carry no result_day: they must still unmarshal (empty
// day, legacy display fallback) instead of failing the restore.
func TestMaturityOldRowWithoutResultDayUnmarshals(t *testing.T) {
	st, err := unmarshalMaturity(`{"enabled":true,"target":7,"mode":"unmetered","last_action":"admit","last_result":"skip:cooling"}`)
	if err != nil {
		t.Fatalf("unmarshalMaturity old row: %v", err)
	}
	if st.resultDay != "" {
		t.Errorf("resultDay = %q, want empty for a pre-upgrade row", st.resultDay)
	}
	if st.lastResult != "skip:cooling" {
		t.Errorf("lastResult = %q, want skip:cooling", st.lastResult)
	}
}

// A prior night's ledger survives untouched outside the window: the
// backend never rewrites history, it only stamps tonight's writes — the
// dashboard scopes the stale skip to Pending from the served day.
func TestMaturityPriorDaySkipUntouchedOutsideWindow(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock)
	_, end := maturityWindowFor(time.Now())
	now := end.Add(time.Minute)
	seedFireReady(t, p, 0, 2, now)
	yesterday := laDay(now.AddDate(0, 0, -1))
	toks := p.roster.Load()
	tok := (*toks)[0]
	tok.maturityMu.Lock()
	tok.maturity.lastResult = "skip:cooling"
	tok.maturity.resultDay = yesterday
	tok.maturityMu.Unlock()

	p.maturityTickAt(context.Background(), now)

	if _, result := maturityResult(p, 0); result != "skip:cooling" {
		t.Errorf("result = %q, want the prior night's skip:cooling kept", result)
	}
	snap := p.Snapshot()[0].Maturity
	if snap == nil || snap.ResultDay != yesterday {
		t.Errorf("snapshot = %+v, want result_day %q preserved", snap, yesterday)
	}
}
