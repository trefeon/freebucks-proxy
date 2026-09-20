package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/modelcat"
	"freebucks-proxy/backend/internal/pool"
	"freebucks-proxy/backend/internal/registry"
	"freebucks-proxy/backend/internal/session"
	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
)

// quotaFor is Freebucks-based like the CLI picker: the wire prices map is
// the only source of cost. A priced row renders "<n> Freebucks/hr",
// referral keeps "referral +1/day", all other rows render "" so tables show
// the em-dash fallback and pickers show bare ids. No label carries session
// counts or the word session.
func TestModelsPageLiveQuotaLabel(t *testing.T) {
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
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = mock.URL()
	client, err := upstream.New("tok-0", &clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	// Seed the token's session manager with the observed live state BEFORE
	// pool construction: wire prices for luna (20/hr) and flash (0/hr).
	// UpdateQuotaFromProbe is the same path the admission/poll response
	// uses to mirror the wire state.
	mgr := session.NewManager(client)
	mgr.UpdateQuotaFromProbe(&upstream.SessionState{
		Freebucks: &upstream.FreebucksInfo{
			Balance: 100,
			Prices: map[string]float64{
				"openai/gpt-5.6-luna":        20,
				"deepseek/deepseek-v4-flash": 0,
			},
		},
	})
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
			ID    string `json:"id"`
			Quota string `json:"quota"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	quotaBy := map[string]string{}
	for _, m := range data.Models {
		quotaBy[m.ID] = m.Quota
	}
	if got, want := quotaBy["openai/gpt-5.6-luna"], "20 Freebucks/hr"; got != want {
		t.Errorf("live quota label = %q, want %q (wire price)", got, want)
	}
	if got, want := quotaBy["deepseek/deepseek-v4-flash"], "0 Freebucks/hr"; got != want {
		t.Errorf("live quota label = %q, want %q (zero wire price)", got, want)
	}
	if q, ok := quotaBy["mimo/mimo-v2.5"]; ok && q != "" {
		t.Errorf("unpriced quota label = %q, want empty", q)
	}
}

// TestModelsPageLiveOfferRow pins the capacity-limited campaign block: the
// row a snapshot reports (the TierOffer row, fable 5.1) carries the live
// remaining/total/user_remaining counts, the vendor's joinable gate and the
// opaque reason, while rows outside the campaign carry no offer block at all
// (omitempty) rather than zero counts.
func TestModelsPageLiveOfferRow(t *testing.T) {
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
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = mock.URL()
	client, err := upstream.New("tok-0", &clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	// The pre-join response carries the live campaign wave; the same
	// UpdateQuotaFromProbe path mirrors it onto the token snapshot.
	mgr := session.NewManager(client)
	mgr.UpdateQuotaFromProbe(&upstream.SessionState{
		LimitedOfferReason: "closed",
		LimitedModelOffers: []upstream.LimitedModelOffer{
			{Model: upstream.FreebuffFable51ModelID, Remaining: 3, Total: 10, UserRemaining: 2},
		},
	})
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
	type offerJSON struct {
		Remaining     int    `json:"remaining"`
		Total         int    `json:"total"`
		UserRemaining int    `json:"user_remaining"`
		Joinable      bool   `json:"joinable"`
		Reason        string `json:"reason"`
	}
	var data struct {
		Models []struct {
			ID    string     `json:"id"`
			Offer *offerJSON `json:"offer"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	offerBy := map[string]*offerJSON{}
	for _, m := range data.Models {
		offerBy[m.ID] = m.Offer
	}
	got, ok := offerBy[upstream.FreebuffFable51ModelID]
	if !ok {
		t.Fatalf("offer row %s missing from /models", upstream.FreebuffFable51ModelID)
	}
	if got == nil {
		t.Fatal("offer row carries no offer block, want the live campaign counts")
	}
	if got.Remaining != 3 || got.Total != 10 || got.UserRemaining != 2 || !got.Joinable || got.Reason != "closed" {
		t.Errorf("offer = %+v, want remaining 3 total 10 user_remaining 2 joinable reason closed", *got)
	}
	if other := offerBy[modelcat.Glm53ModelID]; other != nil {
		t.Errorf("non-campaign row carries an offer block: %+v", *other)
	}
}
