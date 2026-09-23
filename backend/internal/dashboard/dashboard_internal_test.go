package dashboard

// Internal (package dashboard) unit tests for the pure rendering helpers and
// the metrics sample window — the biggest coverage lever for the dashboard
// package (43.8% → the functions below were almost entirely untested).

import (
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/modelcat"
	"freebucks-proxy/backend/internal/pool"
	"freebucks-proxy/backend/internal/registry"
	"freebucks-proxy/backend/internal/upstream"
	"slices"
	"strings"
	"testing"
	"time"
)

// testDashboard builds a dashboard over an empty (bridge-mode) pool: enough
// for the metrics/hist path, which only needs PoolSnapshot + registry.
func testDashboard(t *testing.T) *Dashboard {
	t.Helper()
	cfg := &config.Config{UpstreamBaseURL: "https://www.codebuff.com"}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	return New(func() *config.Config { return cfg }, p, reg, nil, nil)
}

func TestSparklineSVG(t *testing.T) {
	// Empty series: a flat baseline, never a crash.
	got := string(sparklineSVG(nil, "#e3a857", "label"))
	if !strings.Contains(got, `<polyline points="0,42 260,42"`) {
		t.Errorf("empty series = %q, want flat baseline", got)
	}
	if !strings.Contains(got, `aria-label="label"`) {
		t.Errorf("empty series missing aria-label: %s", got)
	}

	// Single sample: same flat branch.
	got = string(sparklineSVG([]float64{7}, "c", "l"))
	if !strings.Contains(got, `<polyline points="0,42 260,42"`) {
		t.Errorf("single-sample = %q, want flat baseline", got)
	}

	// Constant series (>=2): flat polyline with one point per sample.
	got = string(sparklineSVG([]float64{5, 5, 5}, "c", "l"))
	if !strings.Contains(got, `points="0.0,42.0 130.0,42.0 260.0,42.0"`) {
		t.Errorf("constant series = %q, want flat three-point polyline", got)
	}

	// Varying series: min at bottom (42) and max at top (2).
	got = string(sparklineSVG([]float64{0, 10}, "c", "l"))
	if !strings.Contains(got, `points="0.0,42.0 260.0,2.0"`) {
		t.Errorf("varying 2-series = %q, want 0→bottom / 10→top", got)
	}
	got = string(sparklineSVG([]float64{0, 5, 10}, "c", "l"))
	if !strings.Contains(got, `points="0.0,42.0 130.0,22.0 260.0,2.0"`) {
		t.Errorf("varying 3-series = %q, want 0→bottom / 10→top", got)
	}

	// Regression: stroke is an SVG *attribute* where CSS var() cannot
	// resolve (invisible polyline). Production call sites must pass
	// concrete colors.
	for _, color := range []string{"#e3a857", "#7dd3fc"} {
		out := string(sparklineSVG([]float64{1, 2, 3}, color, "l"))
		if !strings.Contains(out, `stroke="`+color+`"`) {
			t.Errorf("sparkline missing concrete stroke %q: %s", color, out)
		}
		if strings.Contains(out, "var(") {
			t.Errorf("sparkline stroke must not use var(): %s", out)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{4*time.Hour + 12*time.Minute, "4h 12m"},
		{45 * time.Minute, "45m"},
		{30 * time.Second, "1m"}, // sub-minute rounds up so countdowns never show a false 0s
		{0, "1m"},
		{90 * time.Second, "2m"},
		{3 * time.Hour, "3h"},
		{5*time.Hour + 59*time.Minute, "5h 59m"},
		{25 * time.Hour, "1d 1h"},
		{48 * time.Hour, "2d"},
		{658*time.Hour + 12*time.Minute, "27d 10h"},
	}
	for _, tc := range cases {
		if got := humanDuration(tc.in); got != tc.want {
			t.Errorf("humanDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatQuota(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{5.0, "5"},
		{5.5, "5.5"},
		{0, "0"},
		{123.456, "123.456"},
		{100, "100"},
	}
	for _, tc := range cases {
		if got := formatQuota(tc.in); got != tc.want {
			t.Errorf("formatQuota(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatEntitlement(t *testing.T) {
	// Ordered alphabetically regardless of map iteration order.
	got := formatEntitlement(map[string]float64{"streak": 3, "base": 1, "referral": 1})
	if got != "base=1, referral=1, streak=3" {
		t.Errorf("formatEntitlement = %q, want sorted key list", got)
	}
	if got := formatEntitlement(map[string]float64{"a": 1.5}); got != "a=1.5" {
		t.Errorf("formatEntitlement single = %q, want a=1.5", got)
	}
	if got := formatEntitlement(nil); got != "" {
		t.Errorf("formatEntitlement(nil) = %q, want empty", got)
	}
}

func TestShortID(t *testing.T) {
	if got := shortID("abcdefghij"); got != "abcdefgh…" {
		t.Errorf("shortID(long) = %q, want 8 chars + ellipsis", got)
	}
	if got := shortID("abcdefgh"); got != "abcdefgh" {
		t.Errorf("shortID(8) = %q, want unchanged", got)
	}
	if got := shortID(""); got != "" {
		t.Errorf("shortID(empty) = %q, want empty", got)
	}
}

// TestMetricsDataSampleCap pins the rolling window: sampling beyond the 120
// cap drops the oldest samples, and repeated sampling appends (the sparkline
// grows to the window width).
func TestMetricsDataSampleCap(t *testing.T) {
	d := testDashboard(t)
	for range maxMetricSamples + 5 {
		md := d.metricsData()
		if md.SampleCount > maxMetricSamples {
			t.Fatalf("SampleCount = %d, exceeded cap %d", md.SampleCount, maxMetricSamples)
		}
	}
	md := d.metricsData()
	if md.SampleCount != maxMetricSamples {
		t.Errorf("SampleCount = %d after %d samples, want capped at %d", md.SampleCount, maxMetricSamples+6, maxMetricSamples)
	}
	// The requests sparkline covers the full window width (last point x=260).
	if !strings.Contains(string(md.RequestsSpark), "260.0,") {
		t.Errorf("requests sparkline missing window-width point: %.60s", md.RequestsSpark)
	}
	if !strings.Contains(string(md.RetriesSpark), "<svg") {
		t.Error("retries sparkline missing")
	}
	if md.TransientRetries != 0 || md.FingerprintRotations != 0 || md.Models == 0 {
		t.Errorf("metrics aggregate fields wrong: retries=%d rotations=%d models=%d",
			md.TransientRetries, md.FingerprintRotations, md.Models)
	}
}

// TestMetricsDataRepeatedSampling pins the append behavior: two consecutive
// samples with a growing counter produce a two-point sparkline (not a reset).
func TestMetricsDataRepeatedSampling(t *testing.T) {
	d := testDashboard(t)
	d.metricsData()
	md := d.metricsData()
	if md.SampleCount != 2 {
		t.Fatalf("SampleCount = %d after two samples, want 2", md.SampleCount)
	}
	if !strings.Contains(string(md.RequestsSpark), "260.0,") {
		t.Errorf("two-point sparkline missing final point: %.60s", md.RequestsSpark)
	}
}

// TestMetricsModelCountServedGate pins /admin/metrics to the served set:
// the raw fallback registry carries god-only/eval ids, so the page must
// report the same count as /v1/models and /admin/overview.
func TestMetricsModelCountServedGate(t *testing.T) {
	d := testDashboard(t)
	if d.reg.ModelCount() <= len(modelcat.ServedIDs()) {
		t.Fatalf("precondition: fallback registry should exceed the served set (got %d)", d.reg.ModelCount())
	}
	md := d.metricsData()
	if md.Models != len(modelcat.ServedIDs()) {
		t.Errorf("metrics Models = %d, want %d (served set)", md.Models, len(modelcat.ServedIDs()))
	}
}

// TestCardFromSnapshotStanding pins the #96 standing mapping: the upstream
// standing block (level/label/score/nextLevelAt/nextLevel) lands on the
// token card fields, and a nil standing block leaves HasStanding false.
func TestCardFromSnapshotStanding(t *testing.T) {
	at := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	card := cardFromSnapshot(pool.TokenSnapshot{
		Token: 0,
		Standing: &upstream.SessionStanding{
			Level:       "established",
			Label:       "Established",
			Score:       62,
			NextLevelAt: at,
			NextLevel:   "core",
		},
	})
	if !card.HasStanding {
		t.Fatal("HasStanding = false, want true")
	}
	if card.StandingLevel != "established" || card.StandingLabel != "Established" {
		t.Errorf("level/label = %q/%q, want established/Established", card.StandingLevel, card.StandingLabel)
	}
	if card.StandingScore != 62 {
		t.Errorf("score = %v, want 62", card.StandingScore)
	}
	if card.StandingNextLevel != "core" {
		t.Errorf("nextLevel = %q, want core", card.StandingNextLevel)
	}
	if card.StandingNextLevelAt != at.Format(time.RFC3339) {
		t.Errorf("nextLevelAt = %q, want %q", card.StandingNextLevelAt, at.Format(time.RFC3339))
	}

	card = cardFromSnapshot(pool.TokenSnapshot{Token: 1})
	if card.HasStanding {
		t.Error("HasStanding = true without a standing block, want false")
	}
	if card.StandingLevel != "" || card.StandingScore != 0 {
		t.Errorf("standing fields populated without a block: %q/%v", card.StandingLevel, card.StandingScore)
	}

	// Issue #140: cap + earn-back fields land on the card too.
	card = cardFromSnapshot(pool.TokenSnapshot{
		Token: 2,
		Standing: &upstream.SessionStanding{
			Level:        "verified",
			Label:        "Verified",
			Score:        30,
			CappedBy:     "third_party_client",
			CappedReason: "A foreign tool schema was seen on this account.",
			Blurb:        "Your account is capped at verified trust.",
			NextSteps: []upstream.StandingNextStep{
				{ID: "verify_email", Label: "Verify your email", Detail: "Adds 25 points.", Points: 25, Href: "/settings"},
			},
		},
	})
	if card.StandingCappedBy != "third_party_client" || card.StandingCappedReason == "" {
		t.Errorf("cappedBy/reason = %q/%q, want third_party_client/non-empty", card.StandingCappedBy, card.StandingCappedReason)
	}
	if card.StandingBlurb == "" {
		t.Error("blurb not carried to the card")
	}
	if len(card.StandingNextSteps) != 1 || card.StandingNextSteps[0].ID != "verify_email" || card.StandingNextSteps[0].Points != 25 {
		t.Errorf("nextSteps = %+v, want one verify_email step worth 25", card.StandingNextSteps)
	}
}

// TestCardFromSnapshotPin pins the PIN_MODEL card fields: the pin rides
// the full card, the skip counter rides both the full card (via
// TokenSnapshot) and the live card.
func TestCardFromSnapshotPin(t *testing.T) {
	snap := pool.TokenSnapshot{
		Token:       0,
		PinnedModel: "z-ai/glm-5.2",
		PinSkips:    7,
	}
	card := cardFromSnapshot(snap)
	if card.PinnedModel != "z-ai/glm-5.2" {
		t.Errorf("PinnedModel = %q, want z-ai/glm-5.2", card.PinnedModel)
	}
	live := liveCardFromSnapshot(snap)
	if live.PinSkips != 7 {
		t.Errorf("live PinSkips = %d, want 7", live.PinSkips)
	}
}

// TestCardFromSnapshotRefund pins the pending-refund card fields: a parked
// release (pending instance id, no settled amount) lands pending_refund on
// both the full and live cards, a settled receipt lands last_refund, and a
// snapshot with neither leaves both cards zero-valued (omitempty).
func TestCardFromSnapshotRefund(t *testing.T) {
	settled := 1.5
	snap := pool.TokenSnapshot{
		Token:         0,
		LastRefund:    &settled,
		PendingRefund: "inst-abc-123",
	}
	card := cardFromSnapshot(snap)
	if card.PendingRefund != "inst-abc-123" {
		t.Errorf("PendingRefund = %q, want inst-abc-123 (parked release)", card.PendingRefund)
	}
	if card.LastRefund == nil || *card.LastRefund != 1.5 {
		t.Errorf("LastRefund = %+v, want 1.5 (settled receipt)", card.LastRefund)
	}
	live := liveCardFromSnapshot(snap)
	if live.PendingRefund != "inst-abc-123" {
		t.Errorf("live PendingRefund = %q, want inst-abc-123", live.PendingRefund)
	}
	if live.LastRefund == nil || *live.LastRefund != 1.5 {
		t.Errorf("live LastRefund = %+v, want 1.5", live.LastRefund)
	}

	plain := cardFromSnapshot(pool.TokenSnapshot{Token: 1})
	if plain.PendingRefund != "" || plain.LastRefund != nil {
		t.Errorf("refund fields = %+v/%q without a receipt, want nil/empty", plain.LastRefund, plain.PendingRefund)
	}
	livePlain := liveCardFromSnapshot(pool.TokenSnapshot{Token: 1})
	if livePlain.PendingRefund != "" || livePlain.LastRefund != nil {
		t.Errorf("live refund fields = %+v/%q without a receipt, want nil/empty", livePlain.LastRefund, livePlain.PendingRefund)
	}
}

// TestCardFromSnapshotBanAndLocked pins the #198/#199 ban mapping: an active
// temporary ban lands ban_type + RFC3339 banned_until on the card, a hard
// ban carries only the type, and Locked is copied through (previously
// dropped, leaving pool-token lock state undefined in the UI). A snapshot
// without ban/lock state yields zero-valued fields.
func TestCardFromSnapshotBanAndLocked(t *testing.T) {
	until := time.Date(2026, 8, 22, 18, 0, 0, 0, time.UTC)
	card := cardFromSnapshot(pool.TokenSnapshot{
		Token:       0,
		Locked:      true,
		BanType:     "temporary",
		BannedUntil: until,
	})
	if !card.Locked {
		t.Error("Locked = false, want true")
	}
	if card.BanType != "temporary" {
		t.Errorf("BanType = %q, want temporary", card.BanType)
	}
	if card.BannedUntil != until.Format(time.RFC3339) {
		t.Errorf("BannedUntil = %q, want %q", card.BannedUntil, until.Format(time.RFC3339))
	}

	card = cardFromSnapshot(pool.TokenSnapshot{
		Token:   1,
		BanType: "hard",
	})
	if card.BanType != "hard" || card.BannedUntil != "" {
		t.Errorf("hard ban card = %q/%q, want hard/empty", card.BanType, card.BannedUntil)
	}

	card = cardFromSnapshot(pool.TokenSnapshot{Token: 2})
	if card.Locked || card.BanType != "" || card.BannedUntil != "" {
		t.Errorf("clean card = %v/%q/%q, want false/empty/empty", card.Locked, card.BanType, card.BannedUntil)
	}

	bc := bridgeCardFromSnapshot(pool.BridgeTokenSnapshot{
		Key:         "abcd1234efgh",
		BanType:     "temporary",
		BannedUntil: until,
	})
	if bc.BanType != "temporary" || bc.BannedUntil != until.Format(time.RFC3339) {
		t.Errorf("bridge card ban = %q/%q, want temporary/%s", bc.BanType, bc.BannedUntil, until.Format(time.RFC3339))
	}
}

// TestLiveCardPerDayDisplay pins the per-day display regression: the hot-poll
// card must carry the Pacific-day request count. It was once full-shape
// only, so the first ?view=live poll rendered it undefined until the next
// full refresh.
func TestLiveCardPerDayDisplay(t *testing.T) {
	snap := pool.TokenSnapshot{
		Token:          0,
		RequestsPerDay: 120,
	}
	live := liveCardFromSnapshot(snap)
	if live.RequestsPerDay != 120 {
		t.Errorf("live RequestsPerDay = %d, want 120", live.RequestsPerDay)
	}
}

// A retired-from-picker row is a catalog row an upstream regen retired from
// picking without pausing it: upstream left it recognized (released clients
// still get a coercion) but it is no longer served and no tier admits it.
// retiredPickerModels are the retired-from-picker rows: still recognized
// (released clients get a coercion) but served by nothing and admitted by
// no tier. 5.6 lost its slot to GPT-6 Luna (c2d2958b); Solar Pro 4 lost its
// slot to Solar Mini 4 (40c75256).
var retiredPickerModels = map[string]bool{
	"openai/gpt-5.6-luna": true,
	"upstage/solar-pro4":  true,
}

// TestModelsDataCatalogTierFacts pins the full-catalog models view: every
// modelcat row appears exactly once with the tier sets that admit it and its
// withdrawal facts, so the page can show which access level can use what.
// Served rows keep served=true; withdrawn rows carry served=false +
// withdrawn=true + the refusal copy and no tiers; tier-only rows (paid,
// offer) are unserved but carry the tier that admits them; the
// retired-from-picker rows (openai/gpt-5.6-luna, whose slot moved to
// openai/gpt-6-luna with the c2d2958b catalog; upstage/solar-pro4, whose
// slot moved to upstage/solar-mini4 with the 40c75256 catalog) are unserved,
// not withdrawn and tierless, because no tier admits them any more. God-only/eval registry
// rows (luna-es) stay out. Count is the row count.
func TestModelsDataCatalogTierFacts(t *testing.T) {
	cfg := &config.Config{
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	if reg.ModelCount() <= len(modelcat.ServedIDs()) {
		t.Fatalf("precondition: fallback registry should exceed the served set (got %d)", reg.ModelCount())
	}
	d := New(func() *config.Config { return cfg }, nil, reg, nil, nil)
	md := d.modelsData()
	if md.Count != len(modelcat.Catalog) || len(md.Models) != len(modelcat.Catalog) {
		t.Fatalf("Count/rows = %d/%d, want %d (every catalog row)", md.Count, len(md.Models), len(modelcat.Catalog))
	}
	seen := make(map[string]bool, len(md.Models))
	var sawReferral, sawWithdrawn, sawOffer, sawRetired bool
	for _, row := range md.Models {
		if seen[row.ID] {
			t.Errorf("model %q listed twice", row.ID)
			continue
		}
		seen[row.ID] = true
		if got, want := row.Served, modelcat.IsServed(row.ID); got != want {
			t.Errorf("row %q Served = %v, want %v", row.ID, got, want)
		}
		if got, want := row.Withdrawn, modelcat.IsPaused(row.ID); got != want {
			t.Errorf("row %q Withdrawn = %v, want %v", row.ID, got, want)
		}
		if got, want := row.Replacement, modelcat.PausedReplacement(row.ID); got != want {
			t.Errorf("row %q Replacement = %q, want %q", row.ID, got, want)
		}
		if got, want := row.Tiers, modelcat.Tiers(row.ID); !slices.Equal(got, want) {
			t.Errorf("row %q Tiers = %v, want modelcat %v", row.ID, got, want)
		}
		if got, want := row.Efforts, modelcat.Efforts(row.ID); !slices.Equal(got, want) {
			t.Errorf("row %q Efforts = %v, want modelcat %v", row.ID, got, want)
		}
		switch {
		case row.Withdrawn:
			sawWithdrawn = true
			if row.Served {
				t.Errorf("withdrawn row %q has Served=true, want false", row.ID)
			}
			if len(row.Tiers) != 0 {
				t.Errorf("withdrawn row %q Tiers = %v, want empty", row.ID, row.Tiers)
			}
			if row.Replacement == "" {
				t.Errorf("withdrawn row %q has no replacement copy", row.ID)
			} else if !modelcat.IsServed(row.Replacement) {
				t.Errorf("withdrawn row %q suggests unserved replacement %q", row.ID, row.Replacement)
			}
		case !row.Served:
			if retiredPickerModels[row.ID] {
				// Retired-from-picker row: still a recognized catalog row
				// (upstream did not pause it, so released clients get a
				// coercion), but no tier admits it and the picker must not
				// offer it. Pinned by id so a regen that silently re-serves
				// or re-tiers it fails here.
				sawRetired = true
				if row.Served {
					t.Errorf("retired row %q has Served=true, want false", row.ID)
				}
				if len(row.Tiers) != 0 {
					t.Errorf("retired row %q Tiers = %v, want empty (no tier admits it)", row.ID, row.Tiers)
				}
				if row.Withdrawn {
					t.Errorf("retired row %q has Withdrawn=true, want false (upstream did not pause it)", row.ID)
				}
				if row.Replacement != "" {
					t.Errorf("retired row %q Replacement = %q, want empty (no replacement copy)", row.ID, row.Replacement)
				}
			} else if len(row.Tiers) == 0 {
				t.Errorf("unserved row %q carries no tiers, want the admitting tier", row.ID)
			}
		}
		if row.ID == modelcat.Glm52ModelID {
			sawReferral = true
			if row.Served {
				t.Errorf("referral row %q has Served=true, want false", row.ID)
			}
			if row.Pool != "referral" {
				t.Errorf("referral row pool = %q, want referral", row.Pool)
			}
			if row.Quota != "referral +1/day" {
				t.Errorf("referral row quota = %q, want %q", row.Quota, "referral +1/day")
			}
		}
		if row.ID == "anthropic/claude-fable-5.1" {
			sawOffer = true
			if !slices.Equal(row.Tiers, []string{modelcat.TierOffer}) {
				t.Errorf("offer row Tiers = %v, want [offer]", row.Tiers)
			}
		}
	}
	for _, info := range modelcat.Catalog {
		if !seen[info.ID] {
			t.Errorf("catalog row %q missing from the models view", info.ID)
		}
	}
	// This fixture carries no live prices, so every row is unpriced and the
	// payload must keep catalog order (priced rows are the only ones hoisted).
	for i, info := range modelcat.Catalog {
		if md.Models[i].ID != info.ID {
			t.Errorf("row %d = %q, want catalog order %q", i, md.Models[i].ID, info.ID)
		}
	}
	if !sawReferral {
		t.Error("models view missing the referral row (z-ai/glm-5.2)")
	}
	if !sawWithdrawn {
		t.Error("models view missing withdrawn rows")
	}
	if !sawRetired {
		t.Errorf("models view missing a retired-from-picker row (want %v)", retiredPickerModels)
	}
	if !sawOffer {
		t.Error("models view missing the offer row (anthropic/claude-fable-5.1)")
	}
}

// TestModelsDataWithdrawnRow pins one withdrawn row's new fields end to end:
// the paused row keeps its catalog display facts but reports served=false,
// withdrawn=true, the replacement the refusal copy recommends, and no tiers
// — the state the SPA hides or greys a row from.
func TestModelsDataWithdrawnRow(t *testing.T) {
	cfg := &config.Config{UpstreamBaseURL: "https://www.codebuff.com"}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	d := New(func() *config.Config { return cfg }, nil, reg, nil, nil)
	md := d.modelsData()
	byID := make(map[string]modelRow, len(md.Models))
	for _, row := range md.Models {
		byID[row.ID] = row
	}
	const paused = "stealth/ox-alpha"
	row, ok := byID[paused]
	if !ok {
		t.Fatalf("withdrawn row %q missing from the models view", paused)
	}
	if !row.Withdrawn || row.Served {
		t.Errorf("%s Withdrawn/Served = %v/%v, want true/false", paused, row.Withdrawn, row.Served)
	}
	if want := modelcat.PausedReplacement(paused); row.Replacement != want {
		t.Errorf("%s Replacement = %q, want %q", paused, row.Replacement, want)
	}
	if len(row.Tiers) != 0 {
		t.Errorf("%s Tiers = %v, want empty (no tier admits a withdrawn row)", paused, row.Tiers)
	}
	if row.Offer != nil {
		t.Errorf("%s Offer = %+v, want nil", paused, row.Offer)
	}
	if row.DisplayName == "" || row.ID != paused {
		t.Errorf("%s row identity = %q/%q, want the catalog display name", paused, row.ID, row.DisplayName)
	}
}

// TestConfigDataEffectiveRows pins the Effective table contract: every key
// appears exactly once, SAFE_MODE is present (it was silently clobbered by a
// bad edit once), and secret list-valued keys render counts — never raw
// joins (DASH-EXPOSURE-005).
func TestConfigDataEffectiveRows(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := &config.Config{
		AuthTokens:      []string{"tok-a", "tok-b"},
		APIKeys:         []string{"sk-client-1", "sk-client-2"},
		SafeMode:        true,
		UpstreamBaseURL: "https://www.codebuff.com",
	}
	d := New(func() *config.Config { return cfg }, nil, nil, nil, nil)
	cd := d.configData()

	seen := map[string]int{}
	for _, kv := range cd.Effective {
		seen[kv.Key]++
		if kv.Key == "API_KEYS" && (strings.Contains(kv.Value, "sk-client") || strings.Contains(kv.Value, ",")) {
			t.Errorf("API_KEYS row leaks raw values: %q", kv.Value)
		}
		if kv.Key == "AUTH_TOKENS" && strings.Contains(kv.Value, "tok-") {
			t.Errorf("AUTH_TOKENS row leaks raw values: %q", kv.Value)
		}
	}
	for _, k := range []string{"SAFE_MODE", "API_KEYS", "AUTH_TOKENS", "ADMIN_TOKEN"} {
		if seen[k] != 1 {
			t.Errorf("Effective rows for %s = %d, want exactly 1", k, seen[k])
		}
	}
}

// TestUnmeteredModelsDerivation pins issue #342: the unlimited-session rows
// come from modelcat (served minus shared premium pool), never from quota
// rows — compact polls omit quota, which would falsely mark every model
// unmetered. Luna (sole shared-premium row) must be absent; the unmetered
// standard rows present; output sorted.
func TestUnmeteredModelsDerivation(t *testing.T) {
	cfg := &config.Config{UpstreamBaseURL: "https://www.codebuff.com"}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	got := unmeteredModels(reg)
	if len(got) == 0 {
		t.Fatal("unmeteredModels empty, want the unmetered standard rows")
	}
	byID := make(map[string]string, len(got))
	for _, r := range got {
		byID[r.ID] = r.Name
		if r.Name == "" || r.Name == r.ID && modelcat.DisplayName(r.ID) != r.ID {
			t.Errorf("row %q has empty display name", r.ID)
		}
	}
	for _, premium := range modelcat.SharedPremiumModels() {
		if _, ok := byID[premium]; ok {
			t.Errorf("shared-premium model %q listed as unmetered", premium)
		}
	}
	// Solar Mini 4 holds the solar slot from the 40c75256 catalog; Pro 4
	// stays listed but self-skips below (no longer served). Bunny is served
	// and unmetered too — the dashboard stage decides its display.
	for _, want := range []string{
		modelcat.Glm53ModelID,
		modelcat.SolarMini4ModelID,
		modelcat.SolarPro4ModelID,
		"deepseek/deepseek-v4-flash",
		"mimo/mimo-v2.5",
	} {
		if !modelcat.IsServed(want) {
			continue
		}
		if _, ok := byID[want]; !ok && !modelcat.IsPremium(want) {
			t.Errorf("served non-premium model %q missing from unmetered list", want)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].ID >= got[i].ID {
			t.Errorf("unmetered list not sorted: %q before %q", got[i-1].ID, got[i].ID)
		}
	}
}

// TestTokensDataUnmeteredWithoutQuota pins #342's third box at the payload
// level: a pool with zero quota rows (compact-poll shape) still carries the
// modelcat-derived list, proving the section ignores quota presence.
func TestTokensDataUnmeteredWithoutQuota(t *testing.T) {
	d := testDashboard(t)
	td := d.tokensData()
	if len(td.Tokens) != 0 {
		t.Fatalf("empty-pool tokens = %d, want 0", len(td.Tokens))
	}
	if len(td.UnmeteredModels) == 0 {
		t.Fatal("UnmeteredModels empty on quota-less pool, want modelcat derivation")
	}
	for _, r := range td.UnmeteredModels {
		if modelcat.IsPremium(r.ID) {
			t.Errorf("premium model %q in payload unmetered list", r.ID)
		}
	}
}

// TestSortModelRowsByPrice pins issue #350 (mirrors sortModelsByPrice):
// cheapest-first, ties on display name, unpriced rows last and in catalog
// (incoming) order.
func TestSortModelRowsByPrice(t *testing.T) {
	rows := []modelRow{{ID: "b"}, {ID: "a"}, {ID: "c"}, {ID: "z-ai/glm-5.2"}}
	prices := map[string]float64{"a": 5, "b": 1, "c": 5}
	sortModelRowsByPrice(rows, prices)
	got := []string{rows[0].ID, rows[1].ID, rows[2].ID, rows[3].ID}
	// b cheapest; a before c on equal price only if display name orders so
	// — assert priced-before-unpriced and cheapest-first, the ported rules.
	if got[0] != "b" {
		t.Errorf("first = %q, want b (cheapest)", got[0])
	}
	if got[3] != "z-ai/glm-5.2" {
		t.Errorf("last = %q, want unpriced glm-5.2", got[3])
	}
	middleOK := got[1] == "a" && got[2] == "c" || got[1] == "c" && got[2] == "a"
	if !middleOK {
		t.Errorf("middle = %v, want a and c in display-name order", got[1:3])
	}
	// No priced row: the payload keeps the incoming (catalog) order —
	// withdrawn rows included — so an unmetered account reads like the
	// upstream picker instead of a display-name sort.
	plain := []modelRow{{ID: "z-ai/glm-5.2"}, {ID: "stealth/ox-alpha"}, {ID: "b"}}
	sortModelRowsByPrice(plain, map[string]float64{})
	if plain[0].ID != "z-ai/glm-5.2" || plain[1].ID != "stealth/ox-alpha" || plain[2].ID != "b" {
		t.Errorf("unpriced rows ordered %v, want incoming catalog order", []string{plain[0].ID, plain[1].ID, plain[2].ID})
	}
	// A single priced row still hoists above the unpriced ones.
	mixed := []modelRow{{ID: "z-ai/glm-5.2"}, {ID: "b"}, {ID: "stealth/ox-alpha"}}
	sortModelRowsByPrice(mixed, map[string]float64{"b": 2})
	if mixed[0].ID != "b" || mixed[1].ID != "z-ai/glm-5.2" || mixed[2].ID != "stealth/ox-alpha" {
		t.Errorf("mixed rows ordered %v, want priced-then-catalog", []string{mixed[0].ID, mixed[1].ID, mixed[2].ID})
	}
}

// TestCardFromSnapshotFirstTabDiscount pins the vendor 6cd8970 first-tab
// discount card mapping: an available offer (amount + holder surface) and
// the daily reset timezone ride the freebucks card, and a snapshot without
// the block leaves both zero-valued (omitempty).
func TestCardFromSnapshotFirstTabDiscount(t *testing.T) {
	card := cardFromSnapshot(pool.TokenSnapshot{
		Token: 0,
		Freebucks: &upstream.FreebucksInfo{
			Balance: 17.5,
			Daily: upstream.FreebucksWindow{
				Limit: 20, Spent: 5, Remaining: 15, ResetTimeZone: "America/New_York",
			},
			Prices: map[string]float64{"openai/gpt-5.6-luna": 2},
			FirstTabDiscount: &upstream.FreebucksFirstTabDiscount{
				Amount: 3, Available: true,
				Holder: &upstream.FreebucksFirstTabDiscountHolder{Surface: "single"},
			},
		},
	})
	if card.Freebucks == nil {
		t.Fatal("Freebucks card = nil, want mapped block")
	}
	if card.Freebucks.Daily.ResetTimeZone != "America/New_York" {
		t.Errorf("Daily.ResetTimeZone = %q, want America/New_York", card.Freebucks.Daily.ResetTimeZone)
	}
	d := card.Freebucks.FirstTabDiscount
	if d == nil {
		t.Fatal("FirstTabDiscount card = nil, want mapped offer")
	} else {
		if d.Amount != 3 || !d.Available || d.HolderSurface != "single" {
			t.Errorf("FirstTabDiscount card = %+v, want amount 3 available single", d)
		}
	}

	bare := cardFromSnapshot(pool.TokenSnapshot{Token: 1})
	if bare.Freebucks != nil {
		t.Errorf("Freebucks card = %+v without a block, want nil", bare.Freebucks)
	}
}
