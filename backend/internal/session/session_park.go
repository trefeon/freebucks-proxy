// session_park.go — short-cooldown session preservation (park-vs-drop).
//
// Vendor oracle (upstream/freebuff, read-only): common/src/types/
// freebuff-session.ts FREEBUFF_GATE_CODES decides every mid-session fate by
// endsTheSession — true drops the seat, false parks it — and cli/src/utils/
// polling-backoff.ts failedPollDelayMs paces every retry (20s base doubling
// to a 300s cap, Retry-After floor, equal jitter). This file ports both so
// short (5-15m) cooldowns hold the slot and retry with backoff instead of
// dropping instantly. Fatal set (Pacific-reset rate_limited, spend_limited
// fresh-admit, banned, unsupported, endsTheSession:true, 402-credits) always
// stays terminal regardless of tuning — callers only consult ShouldPark for
// park-set errors.
//
// Knob chain: SESSION_PARK_ENABLED (default true) + SESSION_PARK_THRESHOLD_MS
// (default 900000 = 15m: at-or-below parks, never drops) are defined by the
// config lane; the pool wires the live values through SetParkConfig (same
// sites as SetReAdmitLead/SetAdmissionProbeTTL). Contract defaults below
// preserve behavior before wiring lands.
package session

import (
	cryptoRand "crypto/rand"
	"encoding/binary"
	"errors"
	"freebuff-proxy/backend/internal/upstream"
	"time"
)

const (
	// defaultParkEnabled mirrors SESSION_PARK_ENABLED=true.
	defaultParkEnabled = true
	// defaultParkThreshold mirrors SESSION_PARK_THRESHOLD_MS=900000 (15m):
	// a park-set error whose computed cooldown is at-or-below parks.
	defaultParkThreshold = 15 * time.Minute
	// parkBackoffBase / parkBackoffMax mirror the vendor poll pacing
	// (failedPollDelayMs 20s doubling, SESSION_POLL_MAX_MS=300000 cap).
	parkBackoffBase = 20 * time.Second
	parkBackoffMax  = 5 * time.Minute
)

// SetParkConfig wires the live short-cooldown preservation tuning
// (SESSION_PARK_ENABLED + SESSION_PARK_THRESHOLD_MS, via the pool from the
// live config like the other session setters). threshold <= 0 falls back to
// the contract default (15m); safe to call at runtime.
func (m *Manager) SetParkConfig(enabled bool, threshold time.Duration) {
	if threshold <= 0 {
		threshold = defaultParkThreshold
	}
	m.mu.Lock()
	m.parkEnabled = enabled
	m.parkThreshold = threshold
	m.mu.Unlock()
}

// ShouldPark reports whether a park-set error with the given computed
// cooldown holds the slot (true) instead of dropping it: enabled plus a
// positive at-or-below-threshold cooldown. A non-positive cooldown never
// parks (mirrors pool shouldPark — the two gates are one rule shared
// across layers). The fatal set never queries — it stays terminal
// regardless. Exported so the pool's cooldown layer shares the exact gate.
func (m *Manager) ShouldPark(cooldown time.Duration) bool {
	if cooldown <= 0 {
		return false
	}
	m.mu.Lock()
	enabled, threshold := m.parkEnabled, m.parkThreshold
	m.mu.Unlock()
	if threshold <= 0 {
		threshold = defaultParkThreshold
	}
	return enabled && cooldown <= threshold
}

// EndsTheSession copies the vendor drop-vs-park oracle
// (FREEBUFF_GATE_CODES in upstream/freebuff common/src/types/
// freebuff-session.ts): true = the seat is gone, DROP the cached row (never
// retry in-request on it); false = the row is fine, PARK and retry/reroute.
// Unknown codes are terminal (drop) — a gate the proxy does not recognise
// must not be ridden. Admission-terminal refusals (banned, country_blocked,
// consent_required, purchase_*, first_tab_discount_changed) stop polling
// upstream (nextDelayMs null) and are terminal alongside the true set.
func EndsTheSession(code string) bool {
	switch code {
	case "waiting_room_queued", "session_limit_reached", "model_unavailable":
		return false
	case "waiting_room_required", "session_expired", "expired",
		"session_superseded", "superseded", "session_model_mismatch":
		return true
	default:
		return true
	}
}

// ParkDelay returns the vendor failedPollDelayMs-shaped wait before the next
// re-POST/GET after failures consecutive transient failures: base 20s
// doubling per failure capped at 300s, equal jitter over the lower half of
// the window, and never before the server's Retry-After floor (±20%
// jitter, capped 300s) — cli/src/utils/polling-backoff.ts semantics.
// Exported so the pool shares the exact pacing.
func ParkDelay(failures int, retryAfter time.Duration) time.Duration {
	if failures < 1 {
		failures = 1
	}
	d := parkBackoffBase << min(failures-1, 5)
	if d > parkBackoffMax {
		d = parkBackoffMax
	}
	d = d/2 + time.Duration(parkRand()%uint64(d/2))
	if retryAfter > 0 {
		// Floor retryAfter to avoid a uint64(0) modulo panic on an
		// absurdly small server Retry-After (mirrors the pool twin).
		if retryAfter < 5*time.Nanosecond {
			retryAfter = 5 * time.Nanosecond
		}
		ra := retryAfter - retryAfter/5 + time.Duration(parkRand()%uint64(2*retryAfter/5))
		if ra > d {
			d = ra
		}
		if d > parkBackoffMax {
			d = parkBackoffMax
		}
	}
	return d
}

// ParkRetryAfter extracts the server's Retry-After floor from a failed poll
// error (0 when the error carries none). Mirrors the pool's extractor for
// the error types the session layer sees.
func ParkRetryAfter(err error) time.Duration {
	var ue *upstream.UpstreamError
	if errors.As(err, &ue) {
		return ue.RetryAfter
	}
	var rle *upstream.RateLimitError
	if errors.As(err, &rle) {
		return rle.RetryAfter
	}
	var ice *upstream.IpCappedError
	if errors.As(err, &ice) {
		return ice.RetryAfter
	}
	var lie *upstream.LimitedIpError
	if errors.As(err, &lie) {
		return lie.RetryAfter
	}
	var wrr *upstream.WaitingRoomRequiredError
	if errors.As(err, &wrr) {
		return wrr.RetryAfter
	}
	var wr *WaitingRoomError
	if errors.As(err, &wr) {
		return wr.RetryAfter
	}
	var uwr *upstream.WaitingRoomError
	if errors.As(err, &uwr) {
		return uwr.RetryAfter
	}
	return 0
}

// parkRand draws one uint64 from crypto/rand (the jitter source, matching
// the pool twin and the upstream client pattern). A read failure falls back
// to the clock rather than panicking in a serving path.
func parkRand() uint64 {
	var b [8]byte
	if _, err := cryptoRand.Read(b[:]); err != nil {
		return uint64(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint64(b[:])
}
