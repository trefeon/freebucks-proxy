package dashboard

import (
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/pool"
	"freebucks-proxy/backend/internal/registry"
	"freebucks-proxy/backend/internal/session"
	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
	"testing"
	"time"
)

// affordableDurationLabel is floor(remaining / price) rendered as pool time.
func TestAffordableDurationLabel(t *testing.T) {
	cases := []struct {
		remaining, price float64
		want             string
	}{
		{25, 5, "5h"},
		{0, 5, "pool empty"},
		{7.5, 5, "1h 30m"},
		{2, 5, "24m"},
		{5, 0, ""},
	}
	for _, c := range cases {
		if got := affordableDurationLabel(c.remaining, c.price); got != c.want {
			t.Errorf("affordableDurationLabel(%v, %v) = %q, want %q", c.remaining, c.price, got, c.want)
		}
	}
}

// TestMaxSpendableUsesPoolBest pins the pool-wide can-serve semantics: when
// the first snapshot token is drained but a later token is funded, the
// affordable column must use the funded token, not read "pool empty".
func TestMaxSpendableUsesPoolBest(t *testing.T) {
	cfg := &config.Config{
		AuthTokens:         []string{"tok-0", "tok-1"},
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
	clients := make([]*upstream.Client, 2)
	mgrs := make([]*session.Manager, 2)
	balances := []float64{0, 25}
	for i := range clients {
		client, err := upstream.New(cfg.AuthTokens[i], &clientCfg)
		if err != nil {
			t.Fatal(err)
		}
		clients[i] = client
		mgrs[i] = session.NewManager(client)
		mgrs[i].UpdateQuotaFromProbe(&upstream.SessionState{
			Freebucks: &upstream.FreebucksInfo{
				Balance: balances[i],
				Prices:  map[string]float64{"openai/gpt-5.6-luna": 5},
			},
		})
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, clients, mgrs, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := New(func() *config.Config { return cfg }, p, reg, nil, nil)
	if got := d.maxSpendable(); got != 25 {
		t.Errorf("maxSpendable = %v, want 25 (funded second token, drained first)", got)
	}

	// Nil pool: no snapshots, rows read "pool empty".
	dEmpty := New(func() *config.Config { return cfg }, nil, reg, nil, nil)
	if got := dEmpty.maxSpendable(); got != 0 {
		t.Errorf("maxSpendable with nil pool = %v, want 0", got)
	}
}
