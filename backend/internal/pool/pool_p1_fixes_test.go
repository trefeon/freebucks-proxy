package pool

import (
	"context"
	"testing"
	"time"

	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
)

// TestPastResumesFloorProbesBeforePost pins the lift-then-burn fix (prod
// 20:53-21:05): a past resumes_at keeps 60s of ban memory so the quarantine
// gate fires, and the first post-lift admission probes (zero-cost GET)
// before any full EnsureSessionForModel POST.
func TestPastResumesFloorProbesBeforePost(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	// Past ban keeps floor memory and quarantines (gate fires).
	p.CooldownTokenBan(0, &upstream.BanError{Body: "banned", ResumesAt: time.Now().Add(-time.Hour)})
	toks := p.roster.Load()
	if toks == nil || len(*toks) == 0 {
		t.Fatal("no roster entry")
	}
	tok := (*toks)[0]
	if q := tok.quarantine.Load(); q == nil {
		t.Fatal("past resumes_at did not quarantine (want floor memory)")
	}
	if be := tok.runs.BanError(); be == nil {
		t.Fatal("BanError() = nil for past ban (want 60s floor)")
	}

	// Simulate floor expiry without waiting: age the quarantine lift and
	// clear the runs floor window. The next walk lifts and probes first.
	tok.quarantine.Load().liftAt = time.Now().Add(-time.Second)
	tok.runs.ClearCooldowns()

	probesBefore := mock.SessionProbesSnapshot()
	createsBefore := mock.SessionCreates

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("acquire after floor lift: %v (want success via probe-first)", err)
	}
	p.LeaseRelease(lease)

	if got := mock.SessionProbesSnapshot(); got <= probesBefore {
		t.Errorf("session probes = %d, want > %d (lift admission must GET-probe before POST)", got, probesBefore)
	}
	if got := mock.SessionCreates; got <= createsBefore {
		t.Errorf("session creates = %d, want > %d (probe-healthy lane must still admit)", got, createsBefore)
	}
	if q := p.Snapshot()[0]; q.Quarantined {
		t.Errorf("still quarantined after probe-first lift: %+v", q)
	}
}

// TestWarmReuseBypassBound pins the token=1x3 stickiness fix: the warm-reuse
// bypass revalidates after N decisive serves or T minutes so a cold lane can
// go warm after refill.
func TestWarmReuseBypassBound(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)
	toks := p.roster.Load()
	entry := (*toks)[0]

	zeroFb := &upstream.FreebucksInfo{
		Balance: 0,
		Daily:   upstream.FreebucksWindow{Limit: 20, Spent: 20, Remaining: 0, ResetAt: time.Now().Add(6 * time.Hour)},
		Wallet:  upstream.FreebucksWallet{},
		Prices:  map[string]float64{modelA: 2},
	}
	entry.session.UpdateQuotaFromProbe(&upstream.SessionState{Freebucks: zeroFb})

	snap := entry.session.Snapshot()
	if !isReusableSession(snap, modelA) {
		t.Fatalf("session not reusable after acquire: %+v", snap)
	}
	if capped, _ := freebucksCappedNoBypass(snap, modelA); !capped {
		t.Fatalf("zero-balance snapshot not capped without bypass (test setup broken)")
	}
	if capped, _ := freebucksCapped(entry, modelA); capped {
		t.Fatal("capped on first reuse (want bypass)")
	}

	// N decisive bypass serves hit the bound: the next gate revalidates.
	for i := 0; i < bypassMaxServes; i++ {
		noteBypassServe(entry)
	}
	if capped, _ := freebucksCapped(entry, modelA); !capped {
		t.Errorf("not capped after %d bypass serves (want bound revalidation)", bypassMaxServes)
	}

	// Reset re-arms the bypass (live revalidation proved health).
	resetBypassServe(entry)
	if capped, _ := freebucksCapped(entry, modelA); capped {
		t.Error("capped right after reset (want bypass re-armed)")
	}

	// T-minutes age bound also forces revalidation.
	noteBypassServe(entry)
	entry.bypassStart.Store(time.Now().Add(-bypassMaxAge - time.Minute).UnixNano())
	if capped, _ := freebucksCapped(entry, modelA); !capped {
		t.Error("not capped after bypass age exceeded (want time-bound revalidation)")
	}
	resetBypassServe(entry)
}

// TestZoneBucketsResolveAccountZone pins the MeterHistory zone fix: day
// buckets resolve in the account reset zone (LoadLocation) with Pacific
// fallback — spend buckets, reqDay counters, and probe resume fallbacks.
func TestZoneBucketsResolveAccountZone(t *testing.T) {
	// 04:00 UTC July 15: UTC day = July 15, Pacific day = July 14 (PDT).
	now := time.Date(2026, time.July, 15, 4, 0, 0, 0, time.UTC)
	utcStart := bucketStartInZone(now, "day", "UTC")
	wantUTC := time.Date(2026, time.July, 15, 0, 0, 0, 0, time.UTC).Unix()
	if utcStart != wantUTC {
		t.Errorf("UTC bucketStart = %v, want %v", time.Unix(utcStart, 0).UTC(), time.Unix(wantUTC, 0).UTC())
	}
	pacStart := bucketStart(now, "day")
	wantPac := time.Date(2026, time.July, 14, 7, 0, 0, 0, time.UTC).Unix()
	if pacStart != wantPac {
		t.Errorf("Pacific bucketStart = %v, want %v", time.Unix(pacStart, 0).UTC(), time.Unix(wantPac, 0).UTC())
	}
	if utcStart == pacStart {
		t.Errorf("UTC and Pacific day starts equal (%v): zones not resolved", time.Unix(utcStart, 0).UTC())
	}
	// Unknown zone falls back to Pacific, never a fixed UTC hour.
	if got := bucketStartInZone(now, "day", "Bogus/Zone"); got != pacStart {
		t.Errorf("bogus-zone bucketStart = %v, want Pacific %v", time.Unix(got, 0).UTC(), time.Unix(pacStart, 0).UTC())
	}
	if got := nextMidnightInZone(now, "Bogus/Zone"); got.Equal(nextPacificMidnight(now)) == false {
		t.Errorf("bogus-zone midnight = %v, want Pacific %v", got, nextPacificMidnight(now))
	}

	// reqDay counters roll at the account zone's midnight: across the UTC
	// midnight (23:59 July 14 -> 00:01 July 15 UTC) the UTC bucket rolls
	// while the Pacific bucket accumulates (both instants are July 14 PDT).
	t1 := time.Date(2026, time.July, 14, 23, 59, 0, 0, time.UTC)
	t2 := time.Date(2026, time.July, 15, 0, 1, 0, 0, time.UTC)
	lUTC := newAccountLedger()
	lUTC.recordDayRequestInZone(t1, "UTC")
	lUTC.recordDayRequestInZone(t2, "UTC")
	if got := lUTC.dayRequestCountInZone(t2, "UTC"); got != 1 {
		t.Errorf("UTC day count = %d, want 1 (rolled at UTC midnight)", got)
	}
	lPac := newAccountLedger()
	lPac.recordDayRequest(t1)
	lPac.recordDayRequest(t2)
	if got := lPac.dayRequestCount(t2); got != 2 {
		t.Errorf("Pacific day count = %d, want 2 (same Pacific day)", got)
	}

	// Spend buckets follow the same zone: identical adds land in different
	// day windows.
	sUTC := newSpendLedger()
	sUTC.addInZone(10, t2, "UTC")
	sPac := newSpendLedger()
	sPac.add(10, t2)
	if sUTC.dayStart == sPac.dayStart {
		t.Errorf("spend dayStart equal (%v): zone not applied", time.Unix(sUTC.dayStart, 0).UTC())
	}

	// Probe resume fallback honors the explicit instant, else the zone
	// midnight (ResetZone-carried, Pacific fallback for "").
	future := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	outcome := ProbeTokenOutcome{ResetAt: future.Format(time.RFC3339), ResetZone: "UTC"}
	if got := smartProbeExhaustedResume(outcome, time.Now()); !got.Equal(future) {
		t.Errorf("exhausted resume = %v, want explicit %v", got, future)
	}
	empty := ProbeTokenOutcome{ResetZone: "UTC"}
	if got, want := smartProbeExhaustedResume(empty, now), nextMidnightInZone(now, "UTC"); !got.Equal(want) {
		t.Errorf("UTC resume fallback = %v, want %v", got, want)
	}
	if got, want := smartProbeExhaustedResume(ProbeTokenOutcome{}, now), nextPacificMidnight(now); !got.Equal(want) {
		t.Errorf("empty-zone resume = %v, want Pacific %v", got, want)
	}
}
