package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/pool"
	"freebucks-proxy/backend/internal/registry"
	"freebucks-proxy/backend/internal/session"
	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
)

// TestModelsPlanRequiredViewer pins the plan-lock column (json plan_required)
// on the models table to the server's per-viewer verdict (vendor c2d2958b,
// mirroring freebuffPlanRequired in common/src/util/freebuff-model-selection.ts):
// a verdict carried by the session snapshots decides every row — so a US
// viewer's GPT-6 Luna row, which the static fallback list locks, is drawn
// open — while a nil verdict keeps the static fallback, a present-but-empty
// verdict locks nothing, and a live paid plan locks nothing.
func TestModelsPlanRequiredViewer(t *testing.T) {
	const (
		gemini = "google/gemini-3.8-flash"
		mimo   = "mimo/mimo-v2.6-pro"
		luna   = "openai/gpt-6-luna"
	)
	cases := []struct {
		name  string
		state upstream.SessionState
		want  map[string]bool
	}{
		{
			// The US-viewer case: the server does not gate the US-exempt
			// Luna row for this viewer, so it is drawn open here while the
			// two every-surface rows the verdict names stay locked.
			name: "verdict present decides every row",
			state: upstream.SessionState{Freebucks: &upstream.FreebucksInfo{
				Balance:              100,
				PlanRequiredModelIDs: []string{gemini, mimo},
			}},
			want: map[string]bool{gemini: true, mimo: true, luna: false},
		},
		{
			name: "verdict gates the exempt row outside the exemption",
			state: upstream.SessionState{Freebucks: &upstream.FreebucksInfo{
				Balance:              100,
				PlanRequiredModelIDs: []string{gemini, mimo, luna},
			}},
			want: map[string]bool{gemini: true, mimo: true, luna: true},
		},
		{
			// Present-but-empty is the server saying "no row is gated for
			// this viewer": it must NOT fall back to the static list.
			name: "empty verdict locks nothing",
			state: upstream.SessionState{Freebucks: &upstream.FreebucksInfo{
				Balance:              100,
				PlanRequiredModelIDs: []string{},
			}},
			want: map[string]bool{gemini: false, mimo: false, luna: false},
		},
		{
			name: "absent verdict falls back to the static list",
			state: upstream.SessionState{Freebucks: &upstream.FreebucksInfo{
				Balance: 100,
			}},
			want: map[string]bool{gemini: true, mimo: true, luna: true},
		},
		{
			name: "live paid plan locks nothing",
			state: upstream.SessionState{
				SubscriptionTierID: "plan_pro",
				Freebucks: &upstream.FreebucksInfo{
					Balance:              100,
					PlanRequiredModelIDs: []string{gemini, mimo, luna},
				},
			},
			want: map[string]bool{gemini: false, mimo: false, luna: false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := modelsPlanRequiredRows(t, tc.state)
			for id, want := range tc.want {
				got, ok := rows[id]
				if !ok {
					t.Fatalf("model %s missing from /models", id)
				}
				if got != want {
					t.Errorf("%s plan_required = %v, want %v", id, got, want)
				}
			}
		})
	}
}

// modelsPlanRequiredRows serves the admin models endpoint over a pool whose
// only token has observed the given session state, returning the rendered
// id -> plan_required map. UpdateQuotaFromProbe is the same path an
// admission/poll response uses to mirror wire state into the snapshots.
func modelsPlanRequiredRows(t *testing.T, state upstream.SessionState) map[string]bool {
	t.Helper()
	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		ListenAddr:         "127.0.0.1:3457",
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
	}
	mock := testutil.NewMock()
	defer mock.Close()
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = mock.URL()
	client, err := upstream.New("tok-0", &clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	mgr := session.NewManager(client)
	mgr.UpdateQuotaFromProbe(&state)
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, []*session.Manager{mgr}, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := New(func() *config.Config { return cfg }, p, reg, nil, nil)
	ts := httptest.NewServer(d.APIHandler("models"))
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var data struct {
		Models []struct {
			ID           string `json:"id"`
			PlanRequired bool   `json:"plan_required"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	out := make(map[string]bool, len(data.Models))
	for _, m := range data.Models {
		out[m.ID] = m.PlanRequired
	}
	return out
}
