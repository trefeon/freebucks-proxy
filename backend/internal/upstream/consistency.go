package upstream

import (
	"strings"
	"sync/atomic"
)

// consistency.go — US-consistency preset support (US_CONSISTENCY): pins the
// ads device block to US values. The server resolves country from the egress
// IP only — no client-sent signal changes that verdict — so this preset
// aligns the consistency signals the proxy does control (device timezone +
// locale) with the US zone the session calls declare, avoiding mismatched
// signals behind a US egress.
//
// The override is process-wide and live-applied: the server installs it on
// boot and refreshes it on every config reload (server.applyConfig), so all
// upstream clients — pooled and bridge — pick it up without a restart.
// Default off: requestAds consults consistencyAdsZoneOr/Locale around the
// host-derived values, and with no override installed they return the
// fallback untouched (zero behavior change when US_CONSISTENCY is unset).

var (
	consistencyZone   atomic.Value // string; "" (or never stored) = no pin
	consistencyLocale atomic.Value // string; "" (or never stored) = no pin
)

// SetConsistencyAdsOverride installs (zone, locale) as the ads device-block
// pin. An empty zone or locale clears that half; both empty disables the pin
// entirely. Values are trimmed; the zone is expected to be the effective
// session zone the caller already resolved (never trusted blindly here).
func SetConsistencyAdsOverride(zone, locale string) {
	consistencyZone.Store(strings.TrimSpace(zone))
	consistencyLocale.Store(strings.TrimSpace(locale))
}

// consistencyAdsZoneOr returns the pinned device timezone when the preset
// installed one, else the host-derived fallback.
func consistencyAdsZoneOr(fallback string) string {
	if v, ok := consistencyZone.Load().(string); ok && v != "" {
		return v
	}
	return fallback
}

// consistencyAdsLocaleOr returns the pinned device locale ("en-US" under the
// preset) when the preset installed one, else the host-derived fallback.
func consistencyAdsLocaleOr(fallback string) string {
	if v, ok := consistencyLocale.Load().(string); ok && v != "" {
		return v
	}
	return fallback
}
