// acquire_buckets.go - failover error-bucket helpers: typed error
// extractors (as*) and the shortest-window picker (bestRateLimit).
// Pure move from pool.go; no behavior change.
package pool

import (
	"errors"
	"sort"

	"freebuff-proxy/backend/internal/upstream"
)

// asRateLimit extracts a RateLimitError from err (nil when absent).
func asRateLimit(err error) *upstream.RateLimitError {
	var rle *upstream.RateLimitError
	if errors.As(err, &rle) {
		return rle
	}
	return nil
}

// asIpCapped extracts an IpCappedError from err (nil when absent).
func asIpCapped(err error) *upstream.IpCappedError {
	var ice *upstream.IpCappedError
	if errors.As(err, &ice) {
		return ice
	}
	return nil
}

// asBan extracts a BanError from err (nil when absent).
func asBan(err error) *upstream.BanError {
	var be *upstream.BanError
	if errors.As(err, &be) {
		return be
	}
	return nil
}

// asCountryBlocked extracts a CountryBlockedError from err (nil when
// absent).
func asCountryBlocked(err error) *upstream.CountryBlockedError {
	var cbe *upstream.CountryBlockedError
	if errors.As(err, &cbe) {
		return cbe
	}
	return nil
}

// asLimitedIp extracts a LimitedIpError from err (nil when absent).
func asLimitedIp(err error) *upstream.LimitedIpError {
	var lie *upstream.LimitedIpError
	if errors.As(err, &lie) {
		return lie
	}
	return nil
}

// bestRateLimit picks the rate-limit error with the shortest retry
// window (the token that unblocks earliest bounds the wait).
func bestRateLimit(entries []*upstream.RateLimitError) *upstream.RateLimitError {
	best := entries[0]
	for _, e := range entries[1:] {
		if e.RetryAfter < best.RetryAfter {
			best = e
		}
	}
	return best
}

// rateLimitEntry pairs one rate-limit refusal with the 0-based pool token
// that produced it (idx is -1 when the token is unknown). The index lets
// the chat trace attribute an acquire-time 429 to the binding token
// without changing acquire semantics: selection still runs on the bare
// errors with the bestRateLimit rule.
type rateLimitEntry struct {
	err *upstream.RateLimitError
	idx int
}

// AcquireRateLimitedError wraps the surfaced 429 when no lease could be
// acquired: Err is the binding refusal (shortest retry window, the token
// that unblocks earliest), Token its 0-based pool index, LimitedTokens the
// deduped ascending set of every rate-limited token for the model (empty
// when enumeration was unavailable — callers fall back to Token alone).
// Unwrap exposes Err so errors.Is(err, ErrRateLimited) and errors.As for
// *RateLimitError keep working through the wrapper: status mapping,
// cooldown classification and the spend ledger are untouched.
type AcquireRateLimitedError struct {
	Err           *upstream.RateLimitError
	Token         int
	LimitedTokens []int
}

// Error mirrors the binding refusal so logs and dedupe keys read unchanged.
func (e *AcquireRateLimitedError) Error() string {
	if e == nil || e.Err == nil {
		return "upstream rate limited"
	}
	return e.Err.Error()
}

// Unwrap exposes the binding refusal for errors.Is/As traversal.
func (e *AcquireRateLimitedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// appendRateLimitEntry records one token's refusal. Unlike the old
// error-string dedup, every token is kept: two tokens cooling on the same
// window must both land in the rate_tokens set. Selection is unaffected —
// duplicates never beat the minimum.
func appendRateLimitEntry(dst []rateLimitEntry, rle *upstream.RateLimitError, idx int) []rateLimitEntry {
	return append(dst, rateLimitEntry{err: rle, idx: idx})
}

// bestRateLimitEntry picks the entry with the shortest retry window (the
// bestRateLimit rule: first wins ties) and wraps it with token
// attribution. Entries must be non-empty; callers guard on len.
func bestRateLimitEntry(entries []rateLimitEntry) *AcquireRateLimitedError {
	best := entries[0]
	for _, e := range entries[1:] {
		if e.err.RetryAfter < best.err.RetryAfter {
			best = e
		}
	}
	seen := make(map[int]bool, len(entries))
	var set []int
	for _, e := range entries {
		if e.idx < 0 || seen[e.idx] {
			continue
		}
		seen[e.idx] = true
		set = append(set, e.idx)
	}
	sort.Ints(set)
	return &AcquireRateLimitedError{Err: best.err, Token: best.idx, LimitedTokens: set}
}
