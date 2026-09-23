package server

// Tier-aware admission tests (2026-09-20): the gateway used to serve exactly
// the ServedModels registry rows; it now also admits the catalog rows a live
// pool entitlement covers (paid plan / live capacity-limited offer / full or
// free access tier) and annotates the /v1/models surface with the row's tiers
// plus why a listed tier row cannot run (plan_required, offer_unavailable,
// trial_used). Withdrawn ids are never listed — they keep the withdrawn
// refusal copy and are asserted absent here.
//
// These tests live inside package server because they drive the pure tier
// helpers directly and inject pool state the mock upstream cannot carry
// (subscription tier id, limited-offer block); the token's session manager is
// the injection point. The aggregation under test is pool-wide and
// optimistic (any token satisfying one branch admits) — per-token tier
// ROUTING is out of scope and nothing here pins it.
//
// NOTE: the codex slug assertion in server_models_test.go also pins that the
// strict ModelInfo rows carry only the admitted ids.

import (
	"bytes"
	"encoding/json"
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/modelcat"
	"freebucks-proxy/backend/internal/pool"
	"freebucks-proxy/backend/internal/registry"
	"freebucks-proxy/backend/internal/session"
	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const (
	tierGemini = "google/gemini-3.8-flash"
	tierFable  = "anthropic/claude-fable-5.1"
	tierLuna   = "openai/gpt-6-luna"
	// tierWithdrawn is the paused row every withdrawn assertion uses.
	tierWithdrawn = "minimax/minimax-m3"
)

// tierStack wires one pool token whose session state the test can inject
// directly. The mock upstream cannot carry a subscription tier id or an
// offer block, and those two fields are exactly what the tier branches read.
func tierStack(t *testing.T, mut func(*config.Config)) (*Server, *session.Manager, *testutil.MockUpstream) {
	t.Helper()
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
	}
	if mut != nil {
		mut(cfg)
	}
	client, err := upstream.New("tok-0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	mgr := session.NewManager(client)
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, []*session.Manager{mgr}, reg)
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, p, reg, nil, nil, ""), mgr, mock
}

// tierOfferBlock is the vendor FreebuffLimitedModelOffer shape the tier
// branches read, with the personal allowance the test names.
func tierOfferBlock(userRemaining int) []upstream.LimitedModelOffer {
	return []upstream.LimitedModelOffer{
		{Model: tierFable, Remaining: 5, Total: 10, UserRemaining: userRemaining},
	}
}

func tierDo(t *testing.T, ts *httptest.Server, method, path string, body []byte, hdr map[string]string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

// tierOffer is the additive offer object the /v1/models row carries on an
// advertised offer row.
type tierOffer struct {
	Remaining     int  `json:"remaining"`
	Total         int  `json:"total"`
	UserRemaining int  `json:"user_remaining"`
	Joinable      bool `json:"joinable"`
}

// tierModelRow is the /v1/models entry with the tier fields decoded.
type tierModelRow struct {
	ID        string     `json:"id"`
	Available bool       `json:"available"`
	Status    string     `json:"status"`
	Tiers     []string   `json:"tiers"`
	Offer     *tierOffer `json:"offer"`
}

// fetchTierRows returns the decoded rows plus the raw row objects (so a test
// can assert which keys are ABSENT, e.g. the offer block on an offer row the
// pool is not advertising).
func fetchTierRows(t *testing.T, ts *httptest.Server) ([]tierModelRow, []map[string]json.RawMessage) {
	t.Helper()
	status, data := tierDo(t, ts, http.MethodGet, "/v1/models", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("models status = %d, want 200: %s", status, data)
	}
	var out struct {
		Data []tierModelRow `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("models not JSON: %v: %s", err, data)
	}
	var raw struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("models raw not JSON: %v: %s", err, data)
	}
	return out.Data, raw.Data
}

func tierRowByID(t *testing.T, rows []tierModelRow, id string) tierModelRow {
	t.Helper()
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("model %q missing from /v1/models", id)
	return tierModelRow{}
}

func tierEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestModelTierAdmitsBranches is the admission matrix: one case per tier
// branch (offer live/absent/used/spent, paid present/absent, full on a
// full/free/limited/unknown pool, limited, withdrawn). It asserts the pure
// decision AND the reason the annotation copies, because those are the same
// decision read the other way.
func TestModelTierAdmitsBranches(t *testing.T) {
	live := []pool.TokenSnapshot{{LimitedModelOffers: tierOfferBlock(1)}}
	used := []pool.TokenSnapshot{{LimitedModelOffers: tierOfferBlock(0)}}
	spent := []pool.TokenSnapshot{{LimitedModelOffers: []upstream.LimitedModelOffer{
		{Model: tierFable, Remaining: 0, Total: 10, UserRemaining: 1},
	}}}

	tests := []struct {
		name       string
		id         string
		snaps      []pool.TokenSnapshot
		wantAdmit  bool
		wantReason string
	}{
		{"offer live", tierFable, live, true, ""},
		{"offer absent", tierFable, nil, false, "offer_unavailable"},
		{"offer used", tierFable, used, false, "trial_used"},
		{"offer pool spent", tierFable, spent, false, "offer_unavailable"},
		{"paid present", tierGemini, []pool.TokenSnapshot{{SubscriptionTierID: "plan_pro"}}, true, ""},
		{"paid absent", tierGemini, nil, false, "plan_required"},
		{"paid absent on a limited pool", tierGemini, []pool.TokenSnapshot{{AccessTier: "limited"}}, false, "plan_required"},
		{"full row on a full pool", tierLuna, []pool.TokenSnapshot{{AccessTier: "full"}}, true, ""},
		{"full row on a free pool", tierLuna, []pool.TokenSnapshot{{AccessTier: "free"}}, true, ""},
		// Luna is SERVED, so its tier reason stays "" (the limited-tier
		// demotion surfaces as region_limited through modelAvailability).
		{"limited-only pool refuses a full row", tierLuna, []pool.TokenSnapshot{{AccessTier: "limited"}}, false, ""},
		{"full row with no access tier reported", tierLuna, nil, false, ""},
		// The free limited catalog needs no pool signal.
		{"limited row", "mimo/mimo-v2.5", nil, true, ""},
		// A withdrawn row carries no tier, so no tier can admit it and no
		// tier reason exists for it (it is never listed; the refusal copy
		// names the replacement instead).
		{"withdrawn row refused", tierWithdrawn, nil, false, ""},
		{"unknown id refused", "vendor/not-a-model", nil, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelTierAdmits(tc.id, tc.snaps); got != tc.wantAdmit {
				t.Errorf("modelTierAdmits(%s) = %v, want %v", tc.id, got, tc.wantAdmit)
			}
			if got := modelTierReason(tc.id, tc.snaps); got != tc.wantReason {
				t.Errorf("modelTierReason(%s) = %q, want %q", tc.id, got, tc.wantReason)
			}
		})
	}
}

// TestModelAdmissibleFastPathNeedsNoPool pins the served fast path and the
// tierless refusal: a zero Server has a nil pool, so any pool read on these
// two paths would panic. A non-served tier row is NOT asserted here — that
// path is expected to consult the pool (and panics without one).
func TestModelAdmissibleFastPathNeedsNoPool(t *testing.T) {
	s := &Server{}
	if !s.modelAdmissible("mimo/mimo-v2.5") {
		t.Error("served row refused on the fast path, want admitted")
	}
	if s.modelAdmissible(tierWithdrawn) {
		t.Error("withdrawn row admitted, want refused")
	}
	if s.modelAdmissible("deepseek/deepseek-v4-flash-max") {
		t.Error("blocked -max variant admitted, want refused")
	}
	if !modelcat.IsServed("mimo/mimo-v2.5") || modelcat.IsServed(tierLuna+"-max") {
		t.Fatal("served-gate sanity check failed")
	}
}

// TestModelsTierAnnotationShape pins the /v1/models tier fields: tiers is the
// row's canonical tier array, offer appears only while the wave is
// advertised, the status vocabulary follows the pool state, and withdrawn ids
// stay absent from the list (refused with the withdrawn copy instead).
func TestModelsTierAnnotationShape(t *testing.T) {
	t.Run("plain pool", func(t *testing.T) {
		srv, _, mock := tierStack(t, nil)
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)

		rows, raw := fetchTierRows(t, ts)
		if len(rows) != 10 {
			t.Fatalf("rows = %d, want 10 (7 served + 3 tier rows)", len(rows))
		}
		luna := tierRowByID(t, rows, tierLuna)
		if !luna.Available || luna.Status != "unknown" || !tierEqual(luna.Tiers, []string{"full", "paid"}) || luna.Offer != nil {
			t.Errorf("luna row = %+v, want available/unknown tiers [full paid] and no offer", luna)
		}
		flash := tierRowByID(t, rows, "deepseek/deepseek-v4-flash")
		if !tierEqual(flash.Tiers, []string{"limited", "full", "paid"}) {
			t.Errorf("flash tiers = %v, want canonical [limited full paid]", flash.Tiers)
		}
		gemini := tierRowByID(t, rows, tierGemini)
		if gemini.Available || gemini.Status != "plan_required" || !tierEqual(gemini.Tiers, []string{"full", "paid"}) {
			t.Errorf("gemini row = %+v, want unavailable/plan_required tiers [full paid]", gemini)
		}
		fable := tierRowByID(t, rows, tierFable)
		if fable.Available || fable.Status != "offer_unavailable" || !tierEqual(fable.Tiers, []string{"offer"}) || fable.Offer != nil {
			t.Errorf("fable row = %+v, want unavailable/offer_unavailable tiers [offer] and no offer block", fable)
		}
		// The offer block is present only while the wave is advertised: the
		// raw row must not carry the key at all.
		for _, rawRow := range raw {
			var id string
			_ = json.Unmarshal(rawRow["id"], &id)
			if id != tierFable {
				continue
			}
			if _, ok := rawRow["offer"]; ok {
				t.Error("fable raw row carries an offer key, want it omitted while unadvertised")
			}
		}
		// Withdrawn ids stay off the wire catalog: absent here, refused with
		// upstream's withdrawn copy on chat, and never an upstream call.
		for _, id := range []string{tierWithdrawn, "z-ai/glm-5.2", "meta/muse-spark-1.3-contributor", "deepseek/deepseek-v4-pro", "stealth/ox-alpha"} {
			for _, r := range rows {
				if r.ID == id {
					t.Errorf("/v1/models listed withdrawn %q, want it unadvertised", id)
				}
			}
		}
		status, data := tierDo(t, ts, http.MethodPost, "/v1/chat/completions",
			[]byte(`{"model":"`+tierWithdrawn+`","messages":[{"role":"user","content":"hi"}]}`), nil)
		if status != http.StatusBadRequest || !bytes.Contains(data, []byte(modelcat.WithdrawnModelMessage(tierWithdrawn))) {
			t.Errorf("withdrawn chat refusal = %d %s, want 400 carrying the withdrawn copy", status, data)
		}
		if n := len(mock.RecordedChatBodies); n != 0 {
			t.Errorf("upstream chat calls = %d, want 0 for a withdrawn row", n)
		}
	})

	t.Run("paid plan", func(t *testing.T) {
		srv, mgr, _ := tierStack(t, nil)
		mgr.UpdateQuotaFromProbe(&upstream.SessionState{SubscriptionTierID: "plan_pro"})
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)

		rows, _ := fetchTierRows(t, ts)
		gemini := tierRowByID(t, rows, tierGemini)
		if !gemini.Available || gemini.Status != "unknown" || !tierEqual(gemini.Tiers, []string{"full", "paid"}) {
			t.Errorf("gemini row with a plan = %+v, want available/unknown tiers [full paid]", gemini)
		}
		// 2026-09-22 (vendor 0.0.183): the other Pro-only row behaves the same
		// way, without a plan.
		pro := tierRowByID(t, rows, "mimo/mimo-v2.6-pro")
		if !pro.Available || pro.Status != "unknown" || !tierEqual(pro.Tiers, []string{"full", "paid"}) {
			t.Errorf("mimo pro row with a plan = %+v, want available/unknown tiers [full paid]", pro)
		}
		// The offered-only row is unaffected by a plan.
		fable := tierRowByID(t, rows, tierFable)
		if fable.Available || fable.Status != "offer_unavailable" {
			t.Errorf("fable row with a plan = %+v, want still unavailable/offer_unavailable", fable)
		}
	})

	t.Run("offer advertised", func(t *testing.T) {
		srv, mgr, _ := tierStack(t, nil)
		mgr.UpdateQuotaFromProbe(&upstream.SessionState{LimitedModelOffers: tierOfferBlock(2)})
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)

		rows, _ := fetchTierRows(t, ts)
		fable := tierRowByID(t, rows, tierFable)
		if !fable.Available || fable.Status != "unknown" || fable.Offer == nil {
			t.Fatalf("fable row with a live offer = %+v, want available with an offer block", fable)
		}
		if fable.Offer.Remaining != 5 || fable.Offer.Total != 10 || fable.Offer.UserRemaining != 2 || !fable.Offer.Joinable {
			t.Errorf("offer block = %+v, want remaining 5 total 10 user_remaining 2 joinable true", *fable.Offer)
		}
	})

	t.Run("offer allowance spent", func(t *testing.T) {
		srv, mgr, _ := tierStack(t, nil)
		mgr.UpdateQuotaFromProbe(&upstream.SessionState{LimitedModelOffers: tierOfferBlock(0)})
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)

		rows, _ := fetchTierRows(t, ts)
		fable := tierRowByID(t, rows, tierFable)
		if fable.Available || fable.Status != "trial_used" || fable.Offer == nil {
			t.Fatalf("fable row with a spent allowance = %+v, want unavailable/trial_used still carrying the offer block", fable)
		}
		if fable.Offer.Joinable || fable.Offer.UserRemaining != 0 {
			t.Errorf("offer block = %+v, want joinable false user_remaining 0", *fable.Offer)
		}
	})

	t.Run("hide unavailable prunes the new statuses", func(t *testing.T) {
		srv, _, _ := tierStack(t, func(c *config.Config) { c.ModelsHideUnavailable = true })
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)

		rows, _ := fetchTierRows(t, ts)
		served := map[string]bool{
			"deepseek/deepseek-v4-flash":      true,
			"openai/gpt-6-luna":               true,
			"upstage/solar-mini4":             true,
			"stealth/space-bunny-alpha":       true,
			"meta/muse-spark-1.2-contributor": true,
			"z-ai/glm-5.3-flash":              true,
			"mimo/mimo-v2.5":                  true,
		}
		if len(rows) != len(served) {
			t.Fatalf("rows = %d, want %d (every unavailable row pruned)", len(rows), len(served))
		}
		for _, r := range rows {
			if !r.Available || !served[r.ID] {
				t.Errorf("row %s (available %v) survived hide-unavailable, want only the served rows: %+v", r.ID, r.Available, r)
			}
		}
	})

	t.Run("retrieve carries the same shape", func(t *testing.T) {
		srv, mgr, _ := tierStack(t, nil)
		mgr.UpdateQuotaFromProbe(&upstream.SessionState{
			SubscriptionTierID: "plan_pro",
			LimitedModelOffers: tierOfferBlock(2),
		})
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)

		status, data := tierDo(t, ts, http.MethodGet, "/v1/models/"+tierFable, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("retrieve fable status = %d, want 200: %s", status, data)
		}
		var row tierModelRow
		if err := json.Unmarshal(data, &row); err != nil {
			t.Fatalf("retrieve not JSON: %v: %s", err, data)
		}
		if !row.Available || !tierEqual(row.Tiers, []string{"offer"}) || row.Offer == nil || !row.Offer.Joinable {
			t.Errorf("retrieve fable row = %+v, want available with tiers [offer] and a joinable offer block", row)
		}
		// A tier row the pool cannot admit still 404s on retrieve.
		status, _ = tierDo(t, ts, http.MethodGet, "/v1/models/"+tierWithdrawn, nil, nil)
		if status != http.StatusNotFound {
			t.Errorf("retrieve withdrawn status = %d, want 404 (retrieve gate unchanged)", status)
		}
	})
}

// TestModelRefusalCopyPerTierReason pins the refusal copy per tier reason on
// every refusal surface (chat, responses, messages, count_tokens): withdrawn
// rows keep upstream's own copy byte-identical, tier rows name what is
// missing, and MODELS_ALLOW exclusions keep the supported-list dump.
func TestModelRefusalCopyPerTierReason(t *testing.T) {
	genericDeepseek := "Model 'deepseek/deepseek-v4-flash' is not available. Supported models: " + modelcat.ServedHelpText()

	tests := []struct {
		name    string
		id      string
		mut     func(*config.Config)
		inject  func(*session.Manager)
		want    string
		wantCT  int // count_tokens status (paused ids stay recognized)
		allowed bool
	}{
		{
			name: "paid plan missing",
			id:   tierGemini,
			want: "Model 'google/gemini-3.8-flash' requires a paid Freebuff plan; this account has no active subscription.",
		},
		{
			name: "offer not advertised",
			id:   tierFable,
			want: "Model 'anthropic/claude-fable-5.1' is a capacity-limited trial that is not being offered right now.",
		},
		{
			name: "offer allowance spent",
			id:   tierFable,
			inject: func(m *session.Manager) {
				m.UpdateQuotaFromProbe(&upstream.SessionState{LimitedModelOffers: tierOfferBlock(0)})
			},
			want: "Model 'anthropic/claude-fable-5.1' is a capacity-limited trial and this account has already used its allowance.",
		},
		{
			name:   "withdrawn row",
			id:     tierWithdrawn,
			want:   modelcat.WithdrawnModelMessage(tierWithdrawn),
			wantCT: http.StatusOK,
		},
		{
			name: "outside MODELS_ALLOW",
			id:   "deepseek/deepseek-v4-flash",
			mut:  func(c *config.Config) { c.ModelsAllow = []string{"mimo/mimo-v2.5"} },
			want: genericDeepseek,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctStatus := tc.wantCT
			if ctStatus == 0 {
				ctStatus = http.StatusBadRequest
			}
			srv, mgr, mock := tierStack(t, tc.mut)
			if tc.inject != nil {
				tc.inject(mgr)
			}
			ts := httptest.NewServer(srv.Handler())
			t.Cleanup(ts.Close)

			chatBody := []byte(`{"model":"` + tc.id + `","messages":[{"role":"user","content":"hi"}]}`)
			msgBody := []byte(`{"model":"` + tc.id + `","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`)
			respBody := []byte(`{"model":"` + tc.id + `","input":"hi"}`)
			ctBody := []byte(`{"model":"` + tc.id + `","messages":[{"role":"user","content":"hi"}]}`)
			anthropicHdr := map[string]string{"anthropic-version": "2023-06-01"}

			for _, surf := range []struct {
				name   string
				path   string
				body   []byte
				hdr    map[string]string
				status int
			}{
				{"chat", "/v1/chat/completions", chatBody, nil, http.StatusBadRequest},
				{"responses", "/v1/responses", respBody, nil, http.StatusBadRequest},
				{"messages", "/v1/messages", msgBody, anthropicHdr, http.StatusBadRequest},
				{"count_tokens", "/v1/messages/count_tokens", ctBody, anthropicHdr, ctStatus},
			} {
				status, data := tierDo(t, ts, http.MethodPost, surf.path, surf.body, surf.hdr)
				if status != surf.status {
					t.Errorf("%s status = %d, want %d: %s", surf.name, status, surf.status, data)
					continue
				}
				if surf.status != http.StatusBadRequest {
					continue
				}
				var env struct {
					Error struct {
						Message string `json:"message"`
					} `json:"error"`
				}
				if err := json.Unmarshal(data, &env); err != nil {
					t.Errorf("%s body not JSON: %v: %s", surf.name, err, data)
					continue
				}
				if env.Error.Message != tc.want {
					t.Errorf("%s message = %q, want %q", surf.name, env.Error.Message, tc.want)
				}
			}
			if n := len(mock.RecordedChatBodies); n != 0 {
				t.Errorf("upstream chat calls = %d, want 0 (the refusal precedes any lease)", n)
			}
		})
	}
}

// TestChatTierRowAdmittedFromPoolState pins the other half of the gate: a
// tier row the pool actually entitles is admitted end-to-end (the request
// reaches upstream) instead of being refused with the tier copy.
func TestChatTierRowAdmittedFromPoolState(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		inject func(*session.Manager)
	}{
		{
			name:  "paid plan admits the paid-only row",
			model: tierGemini,
			inject: func(m *session.Manager) {
				m.UpdateQuotaFromProbe(&upstream.SessionState{SubscriptionTierID: "plan_pro"})
			},
		},
		{
			name:  "live offer admits the offer row",
			model: tierFable,
			inject: func(m *session.Manager) {
				m.UpdateQuotaFromProbe(&upstream.SessionState{LimitedModelOffers: tierOfferBlock(1)})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, mgr, mock := tierStack(t, nil)
			tc.inject(mgr)
			ts := httptest.NewServer(srv.Handler())
			t.Cleanup(ts.Close)

			status, data := tierDo(t, ts, http.MethodPost, "/v1/chat/completions",
				[]byte(`{"model":"`+tc.model+`","messages":[{"role":"user","content":"hi"}]}`), nil)
			if status != http.StatusOK {
				t.Fatalf("chat %s status = %d, want 200: %s", tc.model, status, data)
			}
			if len(mock.RecordedChatBodies) == 0 {
				t.Error("no upstream chat recorded: the tier row was admitted but never used")
			}
			rows, _ := fetchTierRows(t, ts)
			row := tierRowByID(t, rows, tc.model)
			if !row.Available || row.Status != "unknown" {
				t.Errorf("%s row after admission = %+v, want available/unknown", tc.model, row)
			}
		})
	}
}
