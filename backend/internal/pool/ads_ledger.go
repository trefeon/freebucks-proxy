package pool

// ads_ledger.go — ad-leg firing ledger: server-side proof that the proxy
// walks ad legs like the real CLI (auction + impression, streak).
//
// Lane A (chat loop) and the waiting-room chain record one event per fired
// leg via RecordAdLeg; the dashboard serves retained-window aggregates plus
// the recent event list (GET /admin/api/ads/summary, /admin/api/ads/legs).
//
// Privacy rule: the struct carries NO URL fields (impUrl/clickUrl never
// reach the ledger, the dashboard API, or the frontend — titles/brands
// only). Aggregation is in-memory and resets on restart.

import (
	"sync"
	"time"

	"freebucks-proxy/backend/internal/upstream"
)

// Ad surfaces and legs, matching the wire surfaces the proxy mirrors.
const (
	AdSurfaceWaitingRoom = "waiting_room"
	AdSurfaceChat        = "cli_chat"

	AdLegAuction    = "auction"
	AdLegImpression = "impression"
	AdLegStreak     = "streak"
)

// maxAdLegEvents caps the retained event window (ring). Any over-cap limit
// is inherently bounded because the ring never retains more.
const maxAdLegEvents = 200

// DefaultAdLegsLimit is the legs-page size when ?limit is absent/invalid.
// The dashboard and the OpenAPI description share it.
const DefaultAdLegsLimit = 50

// AdLegEvent is one fired ad leg. Title/Brand are display strings only;
// Credits carries the leg's granted amount (0 when the leg grants none);
// Error is set (and Credits left 0) when the leg failed. There are
// deliberately no URL fields on this struct.
type AdLegEvent struct {
	TS       string  `json:"ts"`
	Surface  string  `json:"surface"`
	Provider string  `json:"provider"`
	Leg      string  `json:"leg"`
	Title    string  `json:"title,omitempty"`
	Brand    string  `json:"brand,omitempty"`
	Credits  float64 `json:"credits,omitempty"`
	Error    string  `json:"error,omitempty"`
}

// AdLegTotals counts retained events per leg.
type AdLegTotals struct {
	Auction    int `json:"auction"`
	Impression int `json:"impression"`
	Streak     int `json:"streak"`
}

// AdSummary is the GET /admin/api/ads/summary answer: retained-window
// aggregates. Field order matches the fixed API contract.
type AdSummary struct {
	Totals         AdLegTotals    `json:"totals"`
	CreditsGranted float64        `json:"creditsGranted"`
	ByProvider     map[string]int `json:"byProvider"`
	BySurface      map[string]int `json:"bySurface"`
	LastEventAt    string         `json:"lastEventAt"`
	Errors         int            `json:"errors"`
}

var (
	adLegMu         sync.Mutex
	adLegEvents     []AdLegEvent
	adLegTotals     AdLegTotals
	adLegCredits    float64
	adLegByProvider = map[string]int{}
	adLegBySurface  = map[string]int{}
	adLegErrors     int
	adLegLastAt     string
)

// WireChatAdLegs connects the upstream chat ad loop to this ledger. It
// assigns upstream.RecordChatAdLeg so chat-surface legs land here; the
// assignment lives pool-side because pool already imports upstream and the
// reverse import would cycle. Called from New; unwired legs are dropped by
// the emitter until then. Safe to call repeatedly (idempotent overwrite).
func WireChatAdLegs() {
	upstream.RecordChatAdLeg = func(l upstream.ChatAdLeg) {
		var credits float64
		if l.Credits != nil {
			credits = *l.Credits
		}
		RecordAdLeg(AdLegEvent{
			TS:       l.TS.UTC().Format(time.RFC3339),
			Surface:  l.Surface,
			Provider: l.Provider,
			Leg:      l.Leg,
			Title:    l.Title,
			Brand:    l.Brand,
			Credits:  credits,
			Error:    l.Error,
		})
	}
}

// RecordAdLeg appends one fired leg to the retained window, evicting the
// oldest event past maxAdLegEvents (aggregates track the retained window,
// so totals always agree with the legs list). An empty TS is stamped with
// the current UTC instant (RFC3339). Best-effort: never blocks, never
// errors — ad tracking must not fail admission.
func RecordAdLeg(ev AdLegEvent) {
	if ev.TS == "" {
		ev.TS = time.Now().UTC().Format(time.RFC3339)
	}
	adLegMu.Lock()
	defer adLegMu.Unlock()
	adLegEvents = append(adLegEvents, ev)
	adLegAccumulate(ev, +1)
	adLegLastAt = ev.TS
	for len(adLegEvents) > maxAdLegEvents {
		adLegAccumulate(adLegEvents[0], -1)
		adLegEvents = append(adLegEvents[:0], adLegEvents[1:]...)
	}
}

// adLegAccumulate applies (+1) or removes (-1) one event's aggregate
// contribution. Caller holds adLegMu.
func adLegAccumulate(ev AdLegEvent, sign int) {
	switch ev.Leg {
	case AdLegAuction:
		adLegTotals.Auction += sign
	case AdLegImpression:
		adLegTotals.Impression += sign
	case AdLegStreak:
		adLegTotals.Streak += sign
	}
	adLegCredits += float64(sign) * ev.Credits
	adLegByProvider[ev.Provider] += sign
	if adLegByProvider[ev.Provider] == 0 {
		delete(adLegByProvider, ev.Provider)
	}
	adLegBySurface[ev.Surface] += sign
	if adLegBySurface[ev.Surface] == 0 {
		delete(adLegBySurface, ev.Surface)
	}
	if ev.Error != "" {
		adLegErrors += sign
	}
}

// AdLegs returns retained events newest-first. A non-positive limit returns
// the whole retained window; any larger limit is bounded by the ring.
func AdLegs(limit int) []AdLegEvent {
	adLegMu.Lock()
	defer adLegMu.Unlock()
	n := len(adLegEvents)
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]AdLegEvent, 0, n)
	for i := len(adLegEvents) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, adLegEvents[i])
	}
	return out
}

// AdSummarySnapshot snapshots the retained-window aggregates. Maps are
// copies; empty ledgers report empty (non-nil) maps and zero values.
func AdSummarySnapshot() AdSummary {
	adLegMu.Lock()
	defer adLegMu.Unlock()
	byProvider := make(map[string]int, len(adLegByProvider))
	for k, v := range adLegByProvider {
		byProvider[k] = v
	}
	bySurface := make(map[string]int, len(adLegBySurface))
	for k, v := range adLegBySurface {
		bySurface[k] = v
	}
	return AdSummary{
		Totals:         adLegTotals,
		CreditsGranted: adLegCredits,
		ByProvider:     byProvider,
		BySurface:      bySurface,
		LastEventAt:    adLegLastAt,
		Errors:         adLegErrors,
	}
}

// ResetAdLedger drops every retained event and aggregate. Tests and
// operator-driven rotation only; the firing paths never call it.
func ResetAdLedger() {
	adLegMu.Lock()
	defer adLegMu.Unlock()
	adLegEvents = nil
	adLegTotals = AdLegTotals{}
	adLegCredits = 0
	adLegByProvider = map[string]int{}
	adLegBySurface = map[string]int{}
	adLegErrors = 0
	adLegLastAt = ""
}
