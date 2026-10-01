package telemetry

import "sync/atomic"

// ModelUnavailableSkips counts session admissions short-circuited by the
// per-model model_unavailable window cache (issue #158): requests for a
// model known to be outside its upstream availability window are routed
// straight to the fallback model without the 409 admission roundtrip.
// Package-level (not per-token) like the logger/ring counters; surfaced on
// /metrics as freebuff_proxy_model_unavailable_skips_total.
var ModelUnavailableSkips atomic.Int64

// RecordModelUnavailableSkip increments the skip counter.
func RecordModelUnavailableSkip() {
	ModelUnavailableSkips.Add(1)
}
