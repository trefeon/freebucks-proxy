package modelcat

import (
	"slices"
)

func byID(id string) *ModelInfo {
	for i := range Catalog {
		if Catalog[i].ID == id {
			return &Catalog[i]
		}
	}
	return nil
}

// DisplayName returns the upstream catalog display name for id, or id when
// the model is unknown (mirrors freebuffWithdrawnModelMessage's fallback).
func DisplayName(id string) string {
	if m := byID(id); m != nil {
		return m.DisplayName
	}
	return id
}

// Tagline returns the upstream catalog description for id.
func Tagline(id string) string {
	if m := byID(id); m != nil {
		return m.Tagline
	}
	return ""
}

// TaglineTooltip returns the row tooltip upstream attaches to id's tagline
// ("" when the row carries none).
func TaglineTooltip(id string) string {
	if m := byID(id); m != nil {
		return m.TaglineTooltip
	}
	return ""
}

// Notice returns the upstream warning or special offer for id.
func Notice(id string) string {
	if m := byID(id); m != nil {
		return m.Notice
	}
	return ""
}

// Badges returns the capability/freshness chips for id.
func Badges(id string) []string {
	if m := byID(id); m != nil {
		return slices.Clone(m.Badges)
	}
	return nil
}

// IsServed reports whether id passes the ServedModels gate.
func IsServed(id string) bool {
	if m := byID(id); m != nil {
		return m.Served
	}
	return false
}

// IsPaused reports whether id is upstream-recognized but withdrawn
// (FREEBUFF_PAUSED_FREE_MODEL_IDS): refused at admission, never served.
func IsPaused(id string) bool {
	if m := byID(id); m != nil {
		return m.PausedReplacement != ""
	}
	return false
}

// PausedReplacement returns the model the withdrawn-model refusal copy
// recommends for id ("" when id is not paused).
func PausedReplacement(id string) string {
	if m := byID(id); m != nil {
		return m.PausedReplacement
	}
	return ""
}

// WithdrawnModelMessage mirrors upstream freebuffWithdrawnModelMessage
// (freebuff-models.ts:1685-1697): names the model asked for and what to use
// instead — the client that sends this id is a released binary whose picker
// still lists it, so "unavailable" alone leaves the user staring at a row
// that looks fine and does not work.
func WithdrawnModelMessage(id string) string {
	replacement := PausedReplacement(id)
	if replacement == "" {
		return DisplayName(id) + " is no longer available in Freebuff."
	}
	return DisplayName(id) + " is no longer available in Freebuff. We recommend using " + DisplayName(replacement) + " instead."
}

// IsPremium reports whether id is in the shared daily premium pool
// (FREEBUFF_PREMIUM_MODEL_IDS)
func IsPremium(id string) bool {
	if m := byID(id); m != nil {
		return m.Premium
	}
	return false
}

// SharedPremiumModels returns the ids metered by the shared daily premium
// pool: Luna + Muse Spark 1.2 since 2026-09-07 (1.3 withdrawn that day;
// solar left the pool when its entitlement went unmetered; gemini is
// Pro-paywalled). GPT-6 Luna holds the slot from 2026-09-22: 5.6 left
// FREEBUFF_MODELS, and the generator marks Premium only for served rows.
// GLM 5.3 Flash is unmetered.
func SharedPremiumModels() []string {
	var out []string
	for i := range Catalog {
		if Catalog[i].Premium {
			out = append(out, Catalog[i].ID)
		}
	}
	return out
}

// ContextWindow returns the model's context window in tokens, or
// DefaultContextWindow when the model has no observed entry.
func ContextWindow(id string) int {
	if m := byID(id); m != nil && m.ContextWindow > 0 {
		return m.ContextWindow
	}
	return DefaultContextWindow
}

// PausedMap builds the paused-model map (id → replacement id) from the
// catalog, mirroring upstream FREEBUFF_PAUSED_FREE_MODEL_IDS.
func PausedMap() map[string]string {
	out := make(map[string]string, len(Catalog))
	for i := range Catalog {
		if Catalog[i].PausedReplacement != "" {
			out[Catalog[i].ID] = Catalog[i].PausedReplacement
		}
	}
	return out
}

// ServedMap builds the ServedModels gate map (id → true) from the catalog.
func ServedMap() map[string]bool {
	out := make(map[string]bool, len(Catalog))
	for i := range Catalog {
		if Catalog[i].Served {
			out[Catalog[i].ID] = true
		}
	}
	return out
}

// ServedIDs returns the served model ids in catalog order.
func ServedIDs() []string {
	var out []string
	for i := range Catalog {
		if Catalog[i].Served {
			out = append(out, Catalog[i].ID)
		}
	}
	return out
}

// ServedHelpText formats the served model list for error messages.
func ServedHelpText() string {
	ids := ServedIDs()
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ", "
		}
		out += id
	}
	return out
}

// Tier vocabulary: the upstream sets that can admit a model, in canonical
// order. TierLimited is the free limited-access catalog
// (LIMITED_FREEBUFF_MODEL_IDS), TierFull is full-access membership
// (FREEBUFF_MODELS), TierPaid is the plan-metered catalog
// (FREEBUFF_PLAN_METERED_CATALOG_MODEL_IDS), and TierOffer marks a row offered
// only while its own shared global pool has sessions left
// (FREEBUFF_LIMITED_OFFER_MODEL_IDS).
const (
	TierLimited = "limited"
	TierFull    = "full"
	TierPaid    = "paid"
	TierOffer   = "offer"
)

// Tiers returns the tier sets that admit id in canonical order (nil when no
// tier offers the row, and for unknown ids).
func Tiers(id string) []string {
	if m := byID(id); m != nil {
		return slices.Clone(m.Tiers)
	}
	return nil
}

// HasTier reports whether id is admitted by tier. Unknown ids are never
// members, and neither is a tier string outside the vocabulary above.
func HasTier(id, tier string) bool {
	m := byID(id)
	if m == nil {
		return false
	}
	return slices.Contains(m.Tiers, tier)
}

// OfferedModelIDs returns the ids metered by a capacity-limited offer
// (TierOffer) in catalog order: admitted only while the row's shared global
// pool has sessions left.
func OfferedModelIDs() []string {
	var out []string
	for i := range Catalog {
		if slices.Contains(Catalog[i].Tiers, TierOffer) {
			out = append(out, Catalog[i].ID)
		}
	}
	return out
}

// AutoTouchModelSentinel is the MATURITY_TOUCH_MODEL value (and the
// per-token empty override meaning) that selects automatic resolution:
// the cheapest served unmetered row, never a priced or honeypot row.
const AutoTouchModelSentinel = "auto"

// IsAutoTouchSentinel reports whether s selects automatic touch-model
// resolution ("" or "auto", case-insensitive, surrounding whitespace
// ignored). Explicit provider/model ids are never sentinels.
func IsAutoTouchSentinel(s string) bool {
	t := ""
	for _, r := range s {
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
			t += string(r)
		}
	}
	if t == "" {
		return true
	}
	if len(t) == 4 && (t[0] == 'a' || t[0] == 'A') && (t[1] == 'u' || t[1] == 'U') && (t[2] == 't' || t[2] == 'T') && (t[3] == 'o' || t[3] == 'O') {
		return true
	}
	return false
}

// AutoUnmeteredTouchModel resolves the cheapest unmetered served row in
// catalog order: the first Served, non-premium, non-experimental row whose
// live Freebucks price is 0 (or whose account is quota-exempt). Honeypot,
// god-only, eval, paused, priced, and BETA rows can never win: unserved ids
// fail the IsServed gate, premium-pool rows fail the IsPremium gate,
// live-priced rows fail the price gate, and experimental rows fail the BETA
// gate — an anonymous host can reprice, rename or withdraw the row without
// notice, and retains prompts (upstream carries the row as experimental for
// exactly this reason), so it must never become the automatic touch,
// fallback, or probe model. Explicit user picks still reach it through the
// Served gate. prices is the token's live Freebucks price map (nil = no
// live meter yet: every static unmetered served row is a candidate). It
// returns "" with reason "fallback:no-unmetered-served" when no candidate
// exists so the caller falls back to the configured MATURITY_TOUCH_MODEL
// (or fails closed when that is itself the auto sentinel).
func AutoUnmeteredTouchModel(prices map[string]float64, exempt bool) (string, string) {
	for i := range Catalog {
		id := Catalog[i].ID
		if !Catalog[i].Served {
			continue
		}
		if Catalog[i].Premium {
			continue
		}
		if Catalog[i].Experimental {
			continue
		}
		if prices != nil {
			if p, ok := prices[id]; ok && p > 0 && !exempt {
				continue
			}
		}
		return id, "auto:unmetered"
	}
	return "", "fallback:no-unmetered-served"
}

// CheapestFreeIn resolves the cheapest served unmetered row present in
// models (the registry allowlist intersection), in catalog order: the same
// Served, non-premium, non-experimental, and live-price gates as
// AutoUnmeteredTouchModel plus membership in models. It returns "" when no
// candidate exists so the caller continues down its default chain
// (picker-lead default, then first SERVED) — never an invented or
// unregistered id.
func CheapestFreeIn(models []string, prices map[string]float64, exempt bool) string {
	if len(models) == 0 {
		return ""
	}
	set := make(map[string]struct{}, len(models))
	for _, id := range models {
		set[id] = struct{}{}
	}
	for i := range Catalog {
		id := Catalog[i].ID
		if _, ok := set[id]; !ok {
			continue
		}
		if !Catalog[i].Served {
			continue
		}
		if Catalog[i].Premium {
			continue
		}
		if Catalog[i].Experimental {
			continue
		}
		if prices != nil {
			if p, ok := prices[id]; ok && p > 0 && !exempt {
				continue
			}
		}
		return id
	}
	return ""
}
