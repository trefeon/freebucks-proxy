package upstream

import (
	"sort"
	"time"
)

// FreebucksWindow is one window of a Freebucks allowance.
type FreebucksWindow struct {
	Limit     float64   `json:"limit"`
	Spent     float64   `json:"spent"`
	Remaining float64   `json:"remaining"`
	ResetAt   time.Time `json:"resetAt"`
	// ResetTimeZone is the IANA zone the daily pool refills in (vendor
	// 6cd8970, FreebuffFreebucksWindow.resetTimeZone): the account's zone,
	// no longer always Pacific. "" on older servers.
	ResetTimeZone string `json:"resetTimeZone,omitempty"`
}

// FreebucksWallet is the never-expiring Freebucks store (issue #321 wire
// drift): plan bonuses land here (monthlyBonus at nextBonusAt; 0 on free).
type FreebucksWallet struct {
	Balance      float64   `json:"balance"`
	MonthlyBonus float64   `json:"monthlyBonus"`
	NextBonusAt  time.Time `json:"nextBonusAt,omitempty"`
}

// FreebucksSpendCeiling is the settled-USD daily spend cap for the account's
// tier (issue #321 wire drift). Upstream deprecated the wire field at
// abd1eed4a ("Provider-spend caps are no longer enforced or displayed"):
// new servers omit spend, the raw pointer stays nil, and Spend keeps its
// zero value. The dashboard passthrough renders that zero, never a refusal.
type FreebucksSpendCeiling struct {
	LimitUsd float64   `json:"limitUsd"`
	ResetAt  time.Time `json:"resetAt,omitempty"`
}

// FreebucksMonthlyAllowance is the monthly dollar allowance (wire drift
// 2026-09-04, issue #330): provider spend for the period at which fresh
// sessions stop. Pointer in FreebucksInfo: absent on servers that predate
// it, and clients must render nothing (not a zero) in that case.
type FreebucksMonthlyAllowance struct {
	LimitUsd     float64   `json:"limitUsd"`
	SpentUsd     float64   `json:"spentUsd"`
	RemainingUsd float64   `json:"remainingUsd"`
	ResetAt      time.Time `json:"resetAt"`
}

// FreebucksPriceChange is one server-announced scheduled repricing (wire
// drift 2026-09-05, issue #350): applied only once due, never to admitted
// sessions, and only to models already on the meter.
type FreebucksPriceChange struct {
	At      string  `json:"at"`
	ModelID string  `json:"modelId"`
	Price   float64 `json:"price"`
	Tagline string  `json:"tagline"`
}

// FreebuffOffPeakPrice mirrors FreebuffOffPeakPrice (vendor 3420c99): one
// model id's off-peak offer — the daily [startHourUtc, endHourUtc) window
// (end may be on the next day) priced at price against regularPrice.
// Server-owned recurring policy for new sessions; admitted charges never
// change.
type FreebuffOffPeakPrice struct {
	StartHourUtc int     `json:"startHourUtc"`
	EndHourUtc   int     `json:"endHourUtc"`
	Price        float64 `json:"price"`
	RegularPrice float64 `json:"regularPrice"`
}

// FreebucksUpgrade is the upstream upgrade nudge carried on the Freebucks
// block (vendor af898dc): kind limited_offer is the DeepSeek discount for an
// unpaid limited account (modelId names the discounted row), kind upgrade is
// the plain prompt for an unpaid full-access account.
type FreebucksUpgrade struct {
	Kind    string `json:"kind"`
	CTA     string `json:"cta"`
	Tooltip string `json:"tooltip"`
	ModelID string `json:"modelId,omitempty"`
}

// FreebucksFirstTabDiscountHolder identifies the session holding the
// account-wide first-tab offer (vendor 6cd8970): the offer re-arms when the
// holding session ends. InstanceID is nil for a JSON null (unbound offer).
// Display/quote state only — the proxy never matches holders.
type FreebucksFirstTabDiscountHolder struct {
	InstanceID *string `json:"instanceId"`
	Surface    string  `json:"surface"`
	ExpiresAt  string  `json:"expiresAt"`
}

// FreebucksFirstTabDiscount is the account-wide first-tab offer (vendor
// 6cd8970, FreebuffFreebucksInfo.firstTabDiscount): Amount off one session
// at a time while Available. The server already folds it into prices, so
// this is display state, never a second subtraction. Pointer in
// FreebucksInfo: absent on servers that predate it.
type FreebucksFirstTabDiscount struct {
	Amount    float64                          `json:"amount"`
	Available bool                             `json:"available"`
	Holder    *FreebucksFirstTabDiscountHolder `json:"holder,omitempty"`
}

// FreebucksInfo is the caller's Freebucks position (issue #232, shape
// issue #321): spendable balance (= daily.remaining + wallet.balance) +
// the daily pool + the never-expiring wallet + the USD spend ceiling +
// the plan id ("" when the account is on the free allowance) +
// the server-authorized quota exemption + per-model prices with their
// display copy + the pre-discount list prices and the off-peak policy
// (vendor 3420c99) + the announced repricing schedule (issue #350) +
// claimable earned grants and the upgrade nudge (vendor af898dc) +
// the server's per-viewer plan-lock verdict (vendor c2d2958b).
type FreebucksInfo struct {
	Balance float64         `json:"balance"`
	Daily   FreebucksWindow `json:"daily"`
	Wallet  FreebucksWallet `json:"wallet"`
	// Spend is the deprecated provider-spend cap (upstream abd1eed4a omits
	// it; nil raw leaves this zero). Monthly is the deprecated monthly
	// allowance (upstream abd1eed4a omits it; nil raw leaves this nil).
	Spend   FreebucksSpendCeiling      `json:"spend"`
	Monthly *FreebucksMonthlyAllowance `json:"monthly,omitempty"`
	PlanID  string                     `json:"planId,omitempty"`
	// QuotaExempt: new sessions stay usable at zero balance (server-sent;
	// the meter's canStart is exempt || balance >= price).
	QuotaExempt bool               `json:"quotaExempt,omitempty"`
	Prices      map[string]float64 `json:"prices"`
	// ListPrices mirrors FreebuffFreebucksInfo.listPrices (vendor 3420c99):
	// the per-model list price BEFORE the first-tab discount, for display
	// cross-out beside the discounted prices entry. Present only on a
	// quote that carries firstTabDiscount; never used for admission or
	// charging.
	ListPrices map[string]float64 `json:"listPrices,omitempty"`
	// PriceNotices overrides the static model tagline with price-resolved
	// copy (mirrors taglineFor in freebuff-model-selector.tsx).
	PriceNotices map[string]string      `json:"priceNotices,omitempty"`
	PriceChanges []FreebucksPriceChange `json:"priceChanges,omitempty"`
	// PlanRequiredModelIDs is the server's per-viewer verdict on the rows
	// to draw plan-locked (vendor c2d2958b,
	// FreebuffFreebucksInfo.planRequiredModelIds): the rows THIS viewer must
	// buy a plan to open, decided SERVER-SIDE because it turns on the
	// viewer's resolved country (the US-or-paid rows) and no client is told
	// its country. nil when the server predates the field or gates nothing
	// for this viewer, in which case the static paid-only list
	// (modelcat.SubscriptionProModelIDs) is the fallback — what every client
	// drew before 2026-09-22. Display only: admission is refused server-side
	// on every surface regardless.
	PlanRequiredModelIDs []string `json:"planRequiredModelIds,omitempty"`
	// ClaimableGrant mirrors claimableGrantFreebucks (vendor af898dc):
	// eligible earned grants admission may convert. Excluded from the
	// server's spendable balance display but counted toward canStart
	// (getFreebucksModelMeter: balance + claimable >= price).
	ClaimableGrant float64 `json:"claimableGrantFreebucks,omitempty"`
	// Upgrade mirrors the upgrade nudge (vendor af898dc); nil when the
	// server sends none.
	Upgrade *FreebucksUpgrade `json:"upgrade,omitempty"`
	// FirstTabDiscount mirrors the account-wide first-tab offer (vendor
	// 6cd8970); nil when the server sends none. Display state only: the
	// server already folds an available offer into prices.
	FirstTabDiscount *FreebucksFirstTabDiscount `json:"firstTabDiscount,omitempty"`
	// OffPeak mirrors FreebuffFreebucksInfo.offPeak (vendor 3420c99):
	// server-owned recurring prices for new sessions. Display/policy
	// state only — admitted charges never change.
	OffPeak map[string]FreebuffOffPeakPrice `json:"offPeak,omitempty"`
}

// Spendable is the admission-time spendable amount: the server-computed
// balance plus eligible earned grants admission may convert (mirrors
// getFreebucksModelMeter in freebuff-session.ts: canStart when exempt or
// balance + claimableGrantFreebucks >= price). Nil-safe: no block means
// nothing spendable.
func (f *FreebucksInfo) Spendable() float64 {
	if f == nil {
		return 0
	}
	return f.Balance + f.ClaimableGrant
}

// DiscountedSessionPrice mirrors discountedSessionPrice in
// freebuff-price-changes.ts (vendor 6cd8970): the session price minus the
// available first-tab amount, floored at zero.
func DiscountedSessionPrice(price, discount float64) float64 {
	if out := price - discount; out > 0 {
		return out
	}
	return 0
}

// ApplyFreebucksPriceChanges applies the server's announced repricing
// schedule to already-parsed info (mirrors applyFreebucksPriceChanges in
// freebuff-price-changes.ts): due changes (at <= now) apply in chronological
// order, reprice only models already on the meter, refresh their notice
// copy, and are consumed; future changes are kept for the next call.
// A due repricing lands discount-adjusted while the first-tab offer is
// available (vendor 6cd8970).
func ApplyFreebucksPriceChanges(fb *FreebucksInfo, now time.Time) {
	if fb == nil || len(fb.PriceChanges) == 0 {
		return
	}
	var pending []FreebucksPriceChange
	var ready []FreebucksPriceChange
	for _, c := range fb.PriceChanges {
		at, err := time.Parse(time.RFC3339, c.At)
		if err != nil || at.After(now) {
			pending = append(pending, c)
			continue
		}
		ready = append(ready, c)
	}
	if len(ready) == 0 {
		return
	}
	sort.Slice(ready, func(i, j int) bool {
		ai, _ := time.Parse(time.RFC3339, ready[i].At)
		aj, _ := time.Parse(time.RFC3339, ready[j].At)
		return ai.Before(aj)
	})
	if fb.Prices == nil {
		fb.Prices = map[string]float64{}
	}
	if fb.PriceNotices == nil && len(ready) > 0 {
		fb.PriceNotices = map[string]string{}
	}
	for _, c := range ready {
		if _, ok := fb.Prices[c.ModelID]; !ok {
			continue
		}
		discount := 0.0
		if fb.FirstTabDiscount != nil && fb.FirstTabDiscount.Available {
			discount = fb.FirstTabDiscount.Amount
		}
		fb.Prices[c.ModelID] = DiscountedSessionPrice(c.Price, discount)
		fb.PriceNotices[c.ModelID] = c.Tagline
	}
	fb.PriceChanges = pending
}

type rawFreebucksWindow struct {
	Limit         float64 `json:"limit"`
	Spent         float64 `json:"spent"`
	Remaining     float64 `json:"remaining"`
	ResetAt       any     `json:"resetAt"`
	ResetTimeZone string  `json:"resetTimeZone"`
}

type rawFreebucksWallet struct {
	Balance      float64 `json:"balance"`
	MonthlyBonus float64 `json:"monthlyBonus"`
	NextBonusAt  any     `json:"nextBonusAt"`
}

type rawFreebucksSpendCeiling struct {
	LimitUsd float64 `json:"limitUsd"`
	ResetAt  any     `json:"resetAt"`
}
type rawFreebucksMonthlyAllowance struct {
	LimitUsd     float64 `json:"limitUsd"`
	SpentUsd     float64 `json:"spentUsd"`
	RemainingUsd float64 `json:"remainingUsd"`
	ResetAt      any     `json:"resetAt"`
}

// rawFreebucksFirstTabDiscountHolder mirrors the holder arm of
// FreebuffFreebucksInfo.firstTabDiscount (vendor 6cd8970).
type rawFreebucksFirstTabDiscountHolder struct {
	InstanceID *string `json:"instanceId"`
	Surface    string  `json:"surface"`
	ExpiresAt  string  `json:"expiresAt"`
}

// rawFreebucksFirstTabDiscount mirrors
// FreebuffFreebucksInfo.firstTabDiscount (vendor 6cd8970).
type rawFreebucksFirstTabDiscount struct {
	Amount    float64                             `json:"amount"`
	Available bool                                `json:"available"`
	Holder    *rawFreebucksFirstTabDiscountHolder `json:"holder"`
}

// rawFreebucksUpgrade mirrors FreebuffFreebucksUpgrade (vendor af898dc).
type rawFreebucksUpgrade struct {
	Kind    string `json:"kind"`
	CTA     string `json:"cta"`
	Tooltip string `json:"tooltip"`
	ModelID string `json:"modelId"`
}

// rawFreebucks mirrors upstream FreebuffFreebucksInfo (issue #321 wire
// drift, #350 for exemption/notices/schedule, af898dc for claimable
// grants and the upgrade nudge, 3420c99 for listPrices/offPeak):
// spendable balance + the daily pool window + the never-expiring wallet
// + the USD spend ceiling + the monthly allowance + the plan id + the
// quota exemption + per-model prices with their pre-discount list prices
// + price-notice copy + the off-peak policy + the announced schedule +
// the per-viewer plan-lock verdict (c2d2958b).
type rawFreebucks struct {
	Balance          float64                             `json:"balance"`
	ClaimableGrant   float64                             `json:"claimableGrantFreebucks"`
	Daily            rawFreebucksWindow                  `json:"daily"`
	Wallet           *rawFreebucksWallet                 `json:"wallet"`
	Spend            *rawFreebucksSpendCeiling           `json:"spend"`
	Monthly          *rawFreebucksMonthlyAllowance       `json:"monthly"`
	PlanID           *string                             `json:"planId"`
	QuotaExempt      *bool                               `json:"quotaExempt"`
	Prices           map[string]float64                  `json:"prices"`
	ListPrices       map[string]float64                  `json:"listPrices"`
	PriceNotices     map[string]string                   `json:"priceNotices"`
	PriceChanges     []rawFreebucksPriceChange           `json:"priceChanges"`
	Upgrade          *rawFreebucksUpgrade                `json:"upgrade"`
	FirstTabDiscount *rawFreebucksFirstTabDiscount       `json:"firstTabDiscount"`
	OffPeak          map[string]rawFreebucksOffPeakPrice `json:"offPeak"`
	// PlanRequiredModelIDs mirrors FreebuffFreebucksInfo.planRequiredModelIds
	// (vendor c2d2958b). Optional on the wire, so a nil slice means
	// "absent" and a non-nil empty slice means "present and gating nothing
	// for this viewer" — the two differ for the static-list fallback.
	PlanRequiredModelIDs []string `json:"planRequiredModelIds"`
}

// rawFreebucksOffPeakPrice mirrors FreebuffOffPeakPrice (vendor 3420c99).
type rawFreebucksOffPeakPrice struct {
	StartHourUtc int     `json:"startHourUtc"`
	EndHourUtc   int     `json:"endHourUtc"`
	Price        float64 `json:"price"`
	RegularPrice float64 `json:"regularPrice"`
}

// rawFreebucksPriceChange mirrors FreebuffPriceChange (issue #350).
type rawFreebucksPriceChange struct {
	At      string  `json:"at"`
	ModelID string  `json:"modelId"`
	Price   float64 `json:"price"`
	Tagline string  `json:"tagline"`
}

func windowFromRaw(w rawFreebucksWindow) FreebucksWindow {
	out := FreebucksWindow{Limit: w.Limit, Spent: w.Spent, Remaining: w.Remaining, ResetTimeZone: w.ResetTimeZone}
	if t, err := parseFlexTime(w.ResetAt); err == nil {
		out.ResetAt = t
	}
	return out
}
