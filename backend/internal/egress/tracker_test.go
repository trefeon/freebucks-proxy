package egress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// countingTraceServer serves a Cloudflare-trace body and counts requests, so
// tracker tests can assert on probe cadence without sleeping blindly.
func countingTraceServer(t *testing.T, body string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts, &hits
}

// waitFor polls cond until it holds or the deadline passes, and returns
// whether it held. Every tracker assertion uses it so the test is bounded by
// a deadline rather than by a fixed sleep.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

// TestTrackerReadsBeforeStartNeverProbe guards the request-path contract:
// Country and Result answer from what is already known, and asking before
// Start must not touch the network at all.
func TestTrackerReadsBeforeStartNeverProbe(t *testing.T) {
	ts, hits := countingTraceServer(t, "ip=203.0.113.7\nloc=SG\n")
	orig := ProbeURL
	ProbeURL = ts.URL
	defer func() { ProbeURL = orig }()

	cache := NewCache()
	tr := NewTracker(cache, Path{Key: "direct", Dialer: DirectDialer(5 * time.Second)}, time.Minute)

	if got := tr.Country(); got != "" {
		t.Errorf("Country before Start = %q, want empty", got)
	}
	if res, ok := tr.Result(); ok {
		t.Errorf("Result before Start = %+v, true, want not ok", res)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("reads before Start probed %d times, want 0", n)
	}
}

// TestTrackerProbesImmediatelyRefreshesAndStops guards the whole lifecycle:
// the first probe lands at once and is visible through the cache and the
// tracker, the interval produces further probes, cancelling ctx stops the
// loop before Start returns, and the last detected country survives the
// cancellation.
func TestTrackerProbesImmediatelyRefreshesAndStops(t *testing.T) {
	ts, hits := countingTraceServer(t, "ip=203.0.113.7\nloc=SG\n")
	orig := ProbeURL
	ProbeURL = ts.URL
	defer func() { ProbeURL = orig }()

	cache := NewCache()
	tr := NewTracker(cache, Path{Key: "direct", Dialer: DirectDialer(5 * time.Second)}, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tr.Start(ctx)
	}()

	if !waitFor(5*time.Second, func() bool { return hits.Load() >= 1 }) {
		t.Fatal("tracker never probed")
	}
	if !waitFor(5*time.Second, func() bool {
		r, ok := cache.Get("direct")
		return ok && r.Country == "SG"
	}) {
		t.Fatal("first probe never landed in the cache")
	}
	if got := tr.Country(); got != "SG" {
		t.Errorf("Country = %q, want SG", got)
	}
	if res, ok := tr.Result(); !ok || res.Country != "SG" {
		t.Errorf("Result = %+v, %v, want a SG result", res, ok)
	}

	// Refresh: the loop must probe again on its own, not only once.
	first := hits.Load()
	if !waitFor(5*time.Second, func() bool { return hits.Load() > first }) {
		t.Fatalf("no refresh probe after the first (%d hits)", first)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after ctx cancel")
	}

	// Nothing may probe after the loop has returned.
	settled := hits.Load()
	if waitFor(100*time.Millisecond, func() bool { return hits.Load() > settled }) {
		t.Errorf("tracker kept probing after Start returned (%d -> %d)", settled, hits.Load())
	}
	if got := tr.Country(); got != "SG" {
		t.Errorf("Country after shutdown = %q, want the last detected SG", got)
	}
}

// TestTrackerSerializesConcurrentProbes guards the no-double-probe rule: a
// handler slow relative to the interval must never be entered twice at once,
// which proves both the loop's sequencing and the probe mutex.
func TestTrackerSerializesConcurrentProbes(t *testing.T) {
	var inflight, maxInflight, hits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inflight.Add(1)
		for {
			old := maxInflight.Load()
			if n <= old || maxInflight.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		inflight.Add(-1)
		hits.Add(1)
		_, _ = w.Write([]byte("ip=203.0.113.7\nloc=SG\n"))
	}))
	t.Cleanup(ts.Close)

	orig := ProbeURL
	ProbeURL = ts.URL
	defer func() { ProbeURL = orig }()

	tr := NewTracker(NewCache(), Path{Key: "direct", Dialer: DirectDialer(5 * time.Second)}, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tr.Start(ctx)
	}()

	if !waitFor(5*time.Second, func() bool { return hits.Load() >= 3 }) {
		t.Fatalf("tracker did not probe repeatedly (hits=%d)", hits.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after ctx cancel")
	}
	if got := maxInflight.Load(); got > 1 {
		t.Errorf("max concurrent probes = %d, want at most 1", got)
	}
}

// TestTrackerCancelAbortsInFlightProbe guards prompt shutdown: a probe stuck
// against a handler that never answers must be cut short by ctx, so Start
// returns without waiting out ProbeTimeout.
func TestTrackerCancelAbortsInFlightProbe(t *testing.T) {
	release := make(chan struct{})
	var hits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		ts.Close()
	})

	orig := ProbeURL
	ProbeURL = ts.URL
	defer func() { ProbeURL = orig }()

	tr := NewTracker(NewCache(), Path{Key: "direct", Dialer: DirectDialer(5 * time.Second)}, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tr.Start(ctx)
	}()

	if !waitFor(5*time.Second, func() bool { return hits.Load() >= 1 }) {
		t.Fatal("tracker never probed")
	}
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return while a probe was stuck")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Start took %v to return after cancel, want prompt", elapsed)
	}
}

// TestTrackerUsesCacheFromElsewhere guards the shared-cache read path: a
// region another surface (health, doctor) stored in the cache is visible
// through the tracker's reads before the tracker itself has probed.
func TestTrackerUsesCacheFromElsewhere(t *testing.T) {
	cache := NewCache()
	cache.Set("direct", Result{IP: "203.0.113.7", Country: "JP"})
	tr := NewTracker(cache, Path{Key: "direct", Dialer: DirectDialer(time.Second)}, time.Minute)

	if got := tr.Country(); got != "JP" {
		t.Errorf("Country = %q, want JP from the shared cache", got)
	}
	if res, ok := tr.Result(); !ok || res.Country != "JP" {
		t.Errorf("Result = %+v, %v, want the cached JP result", res, ok)
	}
}
