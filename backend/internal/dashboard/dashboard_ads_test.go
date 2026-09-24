package dashboard_test

// Ads endpoint tests: GET /admin/api/ads/summary + /admin/api/ads/legs
// serve the fixed contract shapes with zero URL leakage.

import (
	"encoding/json"
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/dashboard"
	"freebucks-proxy/backend/internal/pool"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func adsTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := &config.Config{}
	d := dashboard.New(func() *config.Config { return cfg }, nil, nil, slog.Default(), nil)
	mux := http.NewServeMux()
	mux.Handle("GET /admin/api/ads/summary", d.APIHandler("ads/summary"))
	mux.Handle("GET /admin/api/ads/legs", d.APIHandler("ads/legs"))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func seedAdLegs() {
	pool.ResetAdLedger()
	pool.RecordAdLeg(pool.AdLegEvent{TS: "2026-09-24T10:00:00Z", Surface: pool.AdSurfaceWaitingRoom, Provider: "gravity", Leg: pool.AdLegAuction, Title: "T1", Brand: "B1", Credits: 5})
	pool.RecordAdLeg(pool.AdLegEvent{TS: "2026-09-24T10:00:01Z", Surface: pool.AdSurfaceWaitingRoom, Provider: "gravity", Leg: pool.AdLegImpression, Title: "T1", Brand: "B1"})
	pool.RecordAdLeg(pool.AdLegEvent{TS: "2026-09-24T10:00:02Z", Surface: pool.AdSurfaceChat, Provider: "zeroclick", Leg: pool.AdLegStreak, Error: "streak status 500"})
}

func TestAdsSummaryShape(t *testing.T) {
	seedAdLegs()
	ts := adsTestServer(t)

	resp, err := http.Get(ts.URL + "/admin/api/ads/summary")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("summary status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"totals", "creditsGranted", "byProvider", "bySurface", "lastEventAt", "errors"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("summary keys %v missing %q", keysOf(body), k)
		}
	}
	totals := body["totals"].(map[string]any)
	if totals["auction"] != 1.0 || totals["impression"] != 1.0 || totals["streak"] != 1.0 {
		t.Fatalf("totals = %v, want {1 1 1}", totals)
	}
	if body["creditsGranted"] != 5.0 {
		t.Fatalf("creditsGranted = %v, want 5", body["creditsGranted"])
	}
	if body["errors"] != 1.0 {
		t.Fatalf("errors = %v, want 1", body["errors"])
	}
	if body["lastEventAt"] != "2026-09-24T10:00:02Z" {
		t.Fatalf("lastEventAt = %v, want last ts", body["lastEventAt"])
	}
	raw, _ := json.Marshal(body)
	if leaked := urlLeak(raw); leaked != "" {
		t.Fatalf("summary leaks %q: %s", leaked, raw)
	}
}

func TestAdsLegsShape(t *testing.T) {
	seedAdLegs()
	ts := adsTestServer(t)

	resp, err := http.Get(ts.URL + "/admin/api/ads/legs?limit=2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var legs []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&legs); err != nil {
		t.Fatal(err)
	}
	if len(legs) != 2 {
		t.Fatalf("len = %d, want 2", len(legs))
	}
	if legs[0]["ts"] != "2026-09-24T10:00:02Z" || legs[1]["ts"] != "2026-09-24T10:00:01Z" {
		t.Fatalf("legs not newest-first: %v", legs)
	}
	if legs[0]["leg"] != "streak" || legs[0]["error"] == "" {
		t.Fatalf("first leg = %v, want streak with error", legs[0])
	}

	resp2, err := http.Get(ts.URL + "/admin/api/ads/legs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var all []map[string]any
	if err := json.NewDecoder(resp2.Body).Decode(&all); err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("default limit len = %d, want 3", len(all))
	}
	raw, _ := json.Marshal(all)
	if leaked := urlLeak(raw); leaked != "" {
		t.Fatalf("legs leak %q: %s", leaked, raw)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func urlLeak(raw []byte) string {
	lower := strings.ToLower(string(raw))
	for _, banned := range []string{"impurl", "clickurl", "http://", "https://"} {
		if strings.Contains(lower, banned) {
			return banned
		}
	}
	return ""
}
