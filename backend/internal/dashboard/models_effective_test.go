package dashboard

import (
	"encoding/json"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestModelsPricesReflectDueChangeWithoutReprobe pins the read-time quote on
// the models table: a repricing that fell due after the last parse renders
// (and sorts) at its effective price with no reprobe, while the stored
// snapshot keeps the parsed numbers.
func TestModelsPricesReflectDueChangeWithoutReprobe(t *testing.T) {
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
	mgr := session.NewManager(client)
	mgr.UpdateQuotaFromProbe(&upstream.SessionState{
		Freebucks: &upstream.FreebucksInfo{
			Balance: 100,
			Prices: map[string]float64{
				"openai/gpt-5.6-luna": 20,
			},
			PriceChanges: []upstream.FreebucksPriceChange{
				{At: "2020-05-05T00:00:00Z", ModelID: "openai/gpt-5.6-luna", Price: 7, Tagline: "repriced"},
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
			ID         string  `json:"id"`
			Quota      string  `json:"quota"`
			Price      float64 `json:"price"`
			PriceLabel string  `json:"price_label"`
			Notice     string  `json:"notice"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	byID := map[string]struct {
		Quota      string
		Price      float64
		PriceLabel string
		Notice     string
	}{}
	for _, m := range data.Models {
		byID[m.ID] = struct {
			Quota      string
			Price      float64
			PriceLabel string
			Notice     string
		}{m.Quota, m.Price, m.PriceLabel, m.Notice}
	}
	row, ok := byID["openai/gpt-5.6-luna"]
	if !ok {
		t.Fatal("luna row missing from /models")
	}
	if row.Quota != "7 Freebucks/hr" {
		t.Errorf("luna quota = %q, want effective %q (due 20 -> 7)", row.Quota, "7 Freebucks/hr")
	}
	if row.Price != 7 {
		t.Errorf("luna price = %v, want effective 7", row.Price)
	}
	if row.Notice != "repriced" {
		t.Errorf("luna notice = %q, want due-change tagline", row.Notice)
	}

	// The read is pure: the stored snapshot keeps the parsed numbers and
	// the unconsumed schedule for the next projection.
	stored := mgr.Snapshot().Freebucks
	if stored == nil {
		t.Fatal("stored Freebucks is nil")
	}
	if stored.Prices["openai/gpt-5.6-luna"] != 20 {
		t.Errorf("stored price = %v, want parsed 20", stored.Prices["openai/gpt-5.6-luna"])
	}
	if len(stored.PriceChanges) != 1 {
		t.Errorf("stored schedule len = %d, want 1 (unconsumed)", len(stored.PriceChanges))
	}
}
