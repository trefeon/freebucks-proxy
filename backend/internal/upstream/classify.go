// Upstream error classification: maps a status + body to the recovery
// matrix (classifyError) and the body parsers that build the typed errors
// (parseRateLimit/parseBan/parseIpCapped/parseCountryBlock/parseRetryAfter/
// parseFlexTime), plus the bounded-cooldown constants and the class-name
// helper. The quota-ledger policy (rate-limit event counting, window
// derivation, log lines) lives in ratelimit.go; the sentinels and typed
// error values live in errors.go.
package upstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// classifyError maps an upstream error response to the recovery matrix. It
// matches on the WireCode vocabulary defined in wirecodes.go.
//
// Free-tier only: every call classified here is a freebuff free-tier call
// (the proxy manages free sessions per token and has no BYOK/passthrough
// lane). A future non-free lane must bypass this mapping the way the CLI
// skips all free-mode handling on BYOK runs (send-message.ts isByokRun
// gates): no free-mode envelope markers, no provider-usage/out-of-credits
// rewrite, no gate recovery.
func classifyError(status int, body string, hdr http.Header) error {
	lower := strings.ToLower(body)
	retryAfter := parseRetryAfter(hdr)

	switch {
	case status == http.StatusForbidden && strings.Contains(lower, `"status":"banned"`):
		// The canonical ban body is {"status":"banned"} (the free-session
		// status wire shape, upstream/freebuff freebuff-session.ts). Match
		// the marker exactly: any 403 whose body merely mentions the word
		// "banned" (e.g. {"error":"model temporarily banned..."}) must stay
		// a generic 403, not trigger the ban cooldown.
		return parseBan(body)
	case status == http.StatusForbidden && strings.Contains(lower, `"error":"`+string(WireCodeAccountSuspended)+`"`):
		// Hard-ban shape: 403 {"error":"account_suspended","message":"...
		// suspended due to billing issues."} (upstream/freebuff
		// sdk run-cancellation.test.ts:314-359, api/_post.ts:298-307).
		// Same ban class as "status":"banned": the body carries no
		// resumes_at, so BanError.ResumesAt stays zero and CooldownBan
		// treats it as a PERMANENT hard ban (no timed retry — re-contacting
		// a suspended account only generates more 403 signal). Exact marker
		// only: 'account-suspended' or a message merely containing the word
		// must stay a generic 403.
		return parseBan(body)
	case strings.Contains(lower, string(WireCodeDeploymentOutsideHours)):
		// Free tier is outside its operating hours: temporarily unavailable
		// but worth a later retry. Checked before the status-driven 503/429
		// cases because upstream can attach it to any status (reference:
		// freebuff-reverse adapter.go classifies it Retryable by body first).
		return &UpstreamError{Status: status, Body: truncate(body, 500), RetryAfter: retryAfter, Retryable: true}
	case containsAny(lower, string(WireCodeFreeModeRunFanout)):
		// free_mode_run_fanout: the free tier refused the request because the
		// account's concurrent-run counter looked like proxy fanout. It is a
		// CONCURRENCY refusal, not quota: distinct Status, no ResetAt, no
		// Period (no Pacific-midnight lock). The Retry-After header rides to
		// the caller verbatim — no fabricated bounded backoff.
		return &RateLimitError{
			Status:     string(WireCodeFreeModeRunFanout),
			RetryAfter: retryAfter,
			Body:       truncate(body, 200),
		}
	case containsAny(lower, string(WireCodeFreeModeCapacityDeferred)):
		// Free-tier transient capacity queue: upstream says "your request
		// will be retried automatically" and a same-session retry recovers
		// immediately. Retryable transport-level condition handled under the
		// TRANSIENT_RETRIES budget in ChatCompletions against the SAME
		// lease/session — never a token cooldown, never a session
		// invalidation (reference/freebucks-proxy-hengxin proxy.js:652-668).
		return &CapacityDeferredError{Status: status, Body: truncate(body, 500), RetryAfter: retryAfter}
	case containsAny(lower, string(WireCodeTurnSpendLimit)):
		// turn_spend_limit is a distinctive loop-protection literal and it is
		// terminal on WHATEVER status carries it: upstream attaches it to
		// 429 canonically, but retrying the same turn re-trips the breaker
		// instantly (live 20+ minutes of 60s re-trips), so it must never
		// become a RateLimitError cooldown drumbeat and never the generic
		// 502. The incoming status is preserved for telemetry; the server
		// still surfaces 429 turn_spend_limited with no Retry-After.
		return &TurnSpendLimitError{Status: status, Body: truncate(body, 200)}
	case (status == http.StatusPaymentRequired || status == http.StatusUnauthorized) && reProviderUsageBill.MatchString(lower):
		// Provider-billing failure behind Freebuff (observed as 401 and
		// 402): the shared provider account needs a refill — an operator
		// problem, never the caller's credits. Must precede the blanket
		// 402 arm so it can never become CreditsError/out_of_credits
		// (upstream/freebuff error-handling.ts isFreebuffProviderUsageError
		// + FREEBUFF_PROVIDER_USAGE_ERROR_PATTERN, checked before the
		// credit arms in send-message.ts).
		return parseProviderUsage(status, body)
	case status == http.StatusUnauthorized:
		return fmt.Errorf("%w: %d %s", ErrAuthRejected, status, truncate(body, 200))
	case status == http.StatusServiceUnavailable:
		return &WaitingRoomError{RetryAfter: retryAfter, Detail: truncate(body, 200)}
	case status == http.StatusPaymentRequired:
		return &CreditsError{Status: status, Body: truncate(body, 200)}
	case status == http.StatusNotFound && strings.Contains(lower, "no endpoints"):
		// Issue #630: upstream routing has no serving endpoint for the
		// (model, request-shape) combination — OpenRouter phrasing ("No
		// endpoints found for ...", same family as the max_price fence's
		// failed_routing_step precedent). Distinct from the generic 502
		// so clients see the shape to change instead of an opaque
		// upstream_unavailable.
		return parseNoEndpoints(status, body)
	case status == http.StatusConflict && containsAny(lower, string(WireCodeSessionLimitReached)):
		// 409 session_limit_reached: the ACCOUNT is over its concurrent-tab
		// budget, but this session's row is fine (endsTheSession:false).
		// Distinct non-invalid error: the server surfaces 409 and never
		// refreshes/recreates the session
		// (upstream/freebuff freebuff-session.ts FREEBUFF_GATE_CODES).
		return &SessionLimitError{Status: status, Body: truncate(body, 200)}
	case status == http.StatusConflict && strings.Contains(lower, `"status":"`+string(WireCodeConsentRequired)+`"`):
		// 409 consent_required: the wallet balance moved since the spend
		// limit was confirmed. Terminal for the request (the CLI drops to
		// the picker with retry:null) — never a cooldown, never a retry,
		// never the generic 502. Exact marker only, like the banned arm.
		return parseConsentRequired(status, body)
	case status == http.StatusConflict && strings.Contains(lower, `"status":"`+string(WireCodeFirstTabDiscountChanged)+`"`):
		// 409 first_tab_discount_changed: the first-tab offer moved
		// mid-admission, so the quote is stale and nothing was charged.
		// Terminal with the re-pick copy, never a cooldown or retry.
		return parseFirstTabChanged(status, body)
	case status == http.StatusForbidden && strings.Contains(lower, string(WireCodeFreeModeCLIRequired)):
		return fmt.Errorf("%w: %d %s", ErrFreeModeCLIRequired, status, truncate(body, 200))
	case status == http.StatusForbidden && strings.Contains(lower, string(WireCodeFreeModeUnavailable)):
		// 403 free_mode_unavailable: the free tier refused the request at
		// the region/egress gate (country_not_allowed or anonymous_network
		// egress, with the country_blocked field shape). Terminal like
		// cli_required — a config/egress refusal, never a cooldown, never
		// the generic 502. The 403 gate stays tight: the same marker on
		// any other status falls to default.
		return parseFreeModeUnavailable(status, body)
	case status == http.StatusForbidden && strings.Contains(lower, string(WireCodeFreeModeInvalidAgentHierarchy)):
		// free_mode_invalid_agent_hierarchy: the subagent id is not in its
		// root's allowlist (vendor free-agents.ts hierarchy gate). Dedicated
		// 403 sentinel mirroring free_mode_cli_required: a config refusal,
		// never a cooldown, never the generic 502. The 403 gate stays
		// tight — the same marker on any other status falls to default.
		return fmt.Errorf("%w: %d %s", ErrFreeModeInvalidAgentHierarchy, status, truncate(body, 200))
	case status == http.StatusForbidden && strings.Contains(lower, string(WireCodeCountryBlocked)):
		return parseCountryBlock(body)
	case containsAny(lower, string(WireCodeIpCapped)):
		// 429 ip_capped: too many DISTINCT users on the egress IP.
		// Admission-only — existing sessions keep running, so unlike
		// rate_limited this is NOT tied to a quota reset. Cooldown is
		// bounded by the proxy to retryAfterMs + jitter, with a per-token
		// daily re-admission cap (3rd hit in a rolling window locks until
		// Pacific midnight — #118) (upstream/freebuff freebuff-session.ts).
		return parseIpCapped(body, retryAfter)
	case containsAny(lower, string(WireCodeWaitingRoomQueued)):
		// 429 waiting_room_queued: transient admission race — the session
		// row was caught mid-admit (endsTheSession:false). NOT session
		// invalid: the row is fine, so the cached session must not be
		// invalidated or refreshed. Surfaced as 503 waiting_room_queued +
		// Retry-After via the shared WaitingRoomError
		// (upstream/freebuff freebuff-session.ts FREEBUFF_GATE_CODES).
		return &WaitingRoomError{RetryAfter: retryAfter, Detail: truncate(body, 200)}
	case containsAny(lower, string(WireCodeWaitingRoomRequired)):
		// 428 waiting_room_required (issue #94): the account must walk the
		// reference pre-session ad-chain + streak flow before the next
		// session create. Own retryable signal (Retry-After honored, no
		// cooldown) — deliberately NOT ErrSessionInvalid: the session row is
		// fine, so nothing must be invalidated (reference
		// freebuff2api-optimized codebuff.py:1048-1074). The body marker is
		// the discriminator (upstream can attach it to 428/429 alike); the
		// Client.classify wrapper records the flag so the pool can fire the
		// gated WAITING_ROOM_CHAIN before the next create.
		return &WaitingRoomRequiredError{RetryAfter: retryAfter, Detail: truncate(body, 200)}
	case containsAny(lower, string(WireCodeSessionModelMismatch)) && containsAny(lower, "limited"):
		// The egress IP cannot serve the requested model (e.g. "Limited free
		// access is only available with DeepSeek V4 Flash or MiMo 2.5." or
		// "model <id> is limited on this IP"). The session row is fine — it
		// stays bound to its admitted model — so this is NOT session-invalid:
		// invalidating would re-admit and burn a daily session slot. The server
		// marks the refusal and the pool registry cools the (egress, model)
		// pairing instead.
		return &LimitedIpError{RetryAfter: retryAfter, Body: truncate(body, 200)}
	case containsAny(lower, string(WireCodeFreeModeInvalidAgentModel)):
		// free_mode_invalid_agent_model: the (agent, model) pair is not in
		// upstream's allowlist — a CONFIG/mismatch refusal, not quota: no
		// ResetAt/Period (no midnight lock), distinct Status so the server
		// can surface it with an operator hint. The Retry-After header
		// rides verbatim (usually absent).
		return &RateLimitError{
			Status:     string(WireCodeFreeModeInvalidAgentModel),
			RetryAfter: retryAfter,
			Body:       truncate(body, 200),
		}
	case containsAny(lower, string(WireCodeSessionSuperseded)):
		// #119: 409 session_superseded is a TERMINAL gate rejection
		// (endsTheSession:true — another instance took over the account;
		// upstream/freebuff freebuff-session.ts FREEBUFF_GATE_CODES).
		// Deliberately NOT ErrSessionInvalid: the server must never
		// auto-reacquire in-request (auto-takeover risks ping-pong) — it
		// surfaces 409 session_superseded and lets the NEXT request re-join
		// fresh (send-message.ts handleFreebuffGateError marks the session
		// superseded and stops polling; use-freebuff-session.ts
		// nextDelayMs returns null).
		return &SessionSupersededError{Status: status, Body: truncate(body, 200)}
	case containsAny(lower,
		string(WireCodeFreebuffUpdateRequired), string(WireCodeSessionExpired),
		string(WireCodeSessionModelMismatch), string(WireCodeModelLocked),
		string(WireCodeFreeModeLegacyLunaAgent), string(WireCodeFreeModeLegacyLuna)):
		return fmt.Errorf("%w: %s%s", ErrSessionInvalid, truncate(body, 200), retryDetail(retryAfter))
	case status == http.StatusBadRequest && containsAny(lower, string(WireCodeRunIDNotFound), string(WireCodeRunIDNotRunning)):
		return fmt.Errorf("%w: %s", ErrRunInvalid, truncate(body, 200))
	case status == http.StatusTooManyRequests && containsAny(lower, string(WireCodeInsufficientQuota), string(WireCodeLimitBurstRate)):
		// #133: upstream load saturation. No Retry-After in the body —
		// the refusal surfaces with the header value verbatim, never a
		// fabricated window.
		return &RateLimitError{
			Status:     string(WireCodeLoadShedding),
			RetryAfter: retryAfter,
			Body:       truncate(body, 200),
		}
	case status == http.StatusTooManyRequests && containsAny(lower, string(WireCodePeakHours), string(WireCodePeakHoursStatus)):
		// #133: "Usage is temporarily limited during peak hours, when
		// upstream model prices double…". Both the space body form ("peak
		// hours") and the underscore form ("peak_hours") share this arm;
		// the RateLimitError.Status stays the underscore constant. The peak
		// end is unknowable from the body: the refusal surfaces with the
		// header value verbatim.
		return &RateLimitError{
			Status:     string(WireCodePeakHoursStatus),
			RetryAfter: retryAfter,
			Body:       truncate(body, 200),
		}
	case status == http.StatusTooManyRequests || containsAny(lower, string(WireCodeRateLimited), string(WireCodeSpendLimited)):
		return parseRateLimit(body, parseRetryAfter(hdr))
	default:
		// Vendor-UI copy only, never a classify marker: purchase / consent /
		// terms / purchasesPaused strings (availability labels, Desktop
		// updateRequired/purchasesPaused session flags, wallet-consent prose)
		// ride UI copy or session-parse flags, never a chat/body status
		// literal — and the wiregen guard fails loud on any new snapshot
		// literal. Until vendor ships one, bodies carrying only those
		// strings stay this default 502 upstream_unavailable. Do NOT invent
		// arms for them here.
		return &UpstreamError{Status: status, Body: truncate(body, 500), RetryAfter: retryAfter}
	}
}

// errClassName names the classified error type for the `upstream response`
// debug line. Wrapped sentinel errors (auth/session/run refusals built
// with fmt.Errorf) fall back to the generic upstream error class.
func errClassName(err error) string {
	switch err.(type) {
	case *RateLimitError:
		return "RateLimitError"
	case *IpCappedError:
		return "IpCappedError"
	case *BanError:
		return "BanError"
	case *CountryBlockedError:
		return "CountryBlockedError"
	case *CreditsError:
		return "CreditsError"
	case *SessionLimitError:
		return "SessionLimitError"
	case *LimitedIpError:
		return "LimitedIpError"
	case *SessionSupersededError:
		return "SessionSupersededError"
	case *CapacityDeferredError:
		return "CapacityDeferredError"
	case *WaitingRoomError:
		return "WaitingRoomError"
	case *WaitingRoomRequiredError:
		return "WaitingRoomRequiredError"
	case *NoEndpointsError:
		return "NoEndpointsError"
	case *UpstreamError:
		return "UpstreamError"
	}
	if err == nil {
		return ""
	}
	return "UpstreamError"
}

// SetCooldownTuning is the retired bounded-cooldown push point
// (pool.SetConfig pushed the live values on boot and every reload). It is
// kept with its signature because pool/cooldown_tuning.go still calls it;
// every argument is ignored now that the bounded windows are gone (that
// caller is removed with them).
func SetCooldownTuning(fanout, invalidModel, opaque, loadShed, peakHours, ceiling time.Duration) {
}

// TuningSnapshot is the retired bounded-cooldown snapshot. Kept so the
// test helpers that snapshot around pool.New/SetConfig still compile;
// removed windows read back zero.
type TuningSnapshot struct {
	Fanout, InvalidModel, Opaque, LoadShed, PeakHours, Ceiling time.Duration
}

// SnapshotTuning captures the (now zero) bounded-cooldown values.
func SnapshotTuning() TuningSnapshot {
	return TuningSnapshot{}
}

// Restore is a no-op: there are no bounded windows left to re-apply.
func (s TuningSnapshot) Restore() {
}

// CooldownFromMillis converts an upstream retryAfterMs value to a duration
// with no policy ceiling: the value rides to the caller verbatim. The
// overflow guard runs BEFORE the multiply (time.Duration(ms)*time.Millisecond
// wraps for ms >= ~9.2e12), saturating at the largest representable
// duration instead of wrapping. Non-positive values return 0 so callers'
// <=0 fallback logic is unaffected.
func CooldownFromMillis(ms float64) time.Duration {
	if ms <= 0 {
		return 0
	}
	if ms > float64(math.MaxInt64)/float64(time.Millisecond) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(ms) * time.Millisecond
}

// cooldownFromSeconds converts an upstream retryAfter (seconds) value to a
// duration with no policy ceiling, guarding the float64→duration conversion
// the same way CooldownFromMillis guards the multiply.
func cooldownFromSeconds(sec float64) time.Duration {
	if sec <= 0 {
		return 0
	}
	if sec > float64(math.MaxInt64)/float64(time.Second) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(sec * float64(time.Second))
}

// untilResetAt converts a future reset timestamp to a duration with no
// policy ceiling. time.Until (time.Time.Sub) is undefined when the
// difference exceeds the int64-nanosecond range (~292 years), so a
// centuries-out timestamp is detected on the unix-seconds difference and
// saturates instead of wrapping.
func untilResetAt(t, now time.Time) time.Duration {
	secs := t.Unix() - now.Unix()
	if secs <= 0 {
		return 0
	}
	if secs > math.MaxInt64/int64(time.Second) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(secs) * time.Second
}

// reRetryAfterNs matches "retry after Ns" or "retry after N s" (N = digits).
var reRetryAfterNs = regexp.MustCompile(`retry\s+after\s+(\d+)\s*s`)

// reMinutesLimit matches "N minutes limit" (the free_mode_rate_limited body
// for 30-minute windows).
var reMinutesLimit = regexp.MustCompile(`(\d+)\s+minutes?\s+limit`)

// reHours matches "N hours" for a broader duration fallback.
var reHours = regexp.MustCompile(`(\d+)\s+hours?`)

// reResetAt matches "reset(s) at <ISO-8601>" in the body text.
// Lowercase [tT]/[zZ] because the body is lowercased before matching.
var reResetAt = regexp.MustCompile(`resets?\s+at\s+(\d{4}-\d{2}-\d{2}[tT][\d:.]+[zZ]?)`)

// parseRetryAfterFromText extracts a retry-after duration from plain-text
// body content. Three patterns are tried in order:
//  1. "retry after Ns" — explicit delay from the upstream message.
//  2. "N minutes limit" — the free_mode_rate_limited 30-minute window.
//  3. "N hours" — broader duration fallback.
//
// Returns 0 when none match so callers' <= 0 fallback logic is unaffected.
func parseRetryAfterFromText(text string) time.Duration {
	if m := reRetryAfterNs.FindStringSubmatch(text); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	if m := reMinutesLimit.FindStringSubmatch(text); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	if m := reHours.FindStringSubmatch(text); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return time.Duration(n) * time.Hour
		}
	}
	return 0
}

// parseResetAtFromText extracts a reset timestamp from plain-text body
// content matching "reset(s) at <ISO-8601>". Returns the zero time when no
// match is found so callers' IsZero() checks are unaffected.
func parseResetAtFromText(text string) time.Time {
	if m := reResetAt.FindStringSubmatch(text); m != nil {
		// The body is already lowercased; RFC3339 requires uppercase T/Z.
		iso := strings.ToUpper(m[1])
		if t, err := time.Parse(time.RFC3339, iso); err == nil {
			return t
		}
	}
	return time.Time{}
}

// parseRateLimit builds a RateLimitError from a 429 body, extracting
// retryAfterMs/resetAt/limit/recentCount/period best-effort across multiple
// JSON schemas. Falls back to the Retry-After header, then to body-text
// signals. A body with no timestamp and no header delay keeps RetryAfter 0
// and surfaces as-is — no bounded fallback is fabricated.
// ADR-0027: no Pacific-midnight lock is ever fabricated from the quota
// period/counters — upstream enforces its own pools server-side, explicit
// resetAt/Retry-After signals are honored, and unmirrored refusals surface
// as honest upstream errors.
func parseRateLimit(body string, headerRetryAfter time.Duration) error {
	rle := &RateLimitError{Body: truncate(body, 200), RetryAfter: headerRetryAfter}
	lower := strings.ToLower(body)

	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err == nil {
		target := raw
		if errObj, ok := raw["error"].(map[string]any); ok {
			target = errObj
		}

		if ms, ok := getNumber(target, "retryAfterMs", "retry_after_ms"); ok && ms > 0 {
			rle.RetryAfter = CooldownFromMillis(ms)
		} else if sec, ok := getNumber(target, "retryAfter", "retry_after"); ok && sec > 0 {
			rle.RetryAfter = cooldownFromSeconds(sec)
		}

		if t, ok := getTime(target, "resetAt", "reset_at", "resets_at", "resumes_at", "reset"); ok && !t.IsZero() {
			rle.ResetAt = t
		}

		// Window evidence for the freebucks ceiling (prod 2026-09-16). Both
		// fields are informational for the cooldown math (none of it reads
		// them); they only let the payloads and the ledger name the refusal.
		// windowHours is clamped like every other upstream-controlled number
		// so an absurd value can never truncate into a bogus int.
		if wh, ok := getNumber(target, "windowHours", "window_hours"); ok && wh > 0 && wh <= math.MaxInt32 {
			rle.WindowHours = int(wh)
		}
		if hasFreebucksShortfall(target) {
			rle.FreebucksShortfall = true
		}

		if lim, ok := getNumber(target, "limit"); ok {
			rle.Limit = lim
		}
		if cnt, ok := getNumber(target, "recentCount", "recent_count"); ok {
			rle.RecentCount = cnt
		}
		if mod, ok := target["model"].(string); ok {
			rle.Model = mod
		}
		if st, ok := target["status"].(string); ok {
			rle.Status = st
		}
		// Per-turn spend ceiling: upstream kills runaway turns with
		// {"error":"turn_spend_limit",...} — loop protection, NOT
		// session-quota exhaustion. TERMINAL (ErrTurnSpendLimited, no
		// RetryAfter): the breaker's own retryAfterMs does not clear it —
		// live 2026-09-05 every 60s client retry re-tripped instantly for
		// 20+ minutes — so returning a RateLimitError here would hand the
		// client a futile Retry-After drumbeat. Distinct type also keeps
		// it out of the pool cooldown + quota-fallback budget.
		if es, ok := raw["error"].(string); ok && es == string(WireCodeTurnSpendLimit) {
			// parseRateLimit is only entered on 429 (or rate_limited/
			// spend_limited markers), and the breaker only rides 429 —
			// canonicalize to 429 for telemetry consistency.
			return &TurnSpendLimitError{Status: http.StatusTooManyRequests, Body: truncate(body, 200)}
		}
		if period, ok := target["period"].(string); ok {
			rle.Period = period
		}
	}

	// Text-based fallback: extract retry-after duration from the body
	// text when JSON fields didn't provide one (e.g. "30 minutes limit"
	// in free_mode_rate_limited bodies).
	if rle.RetryAfter <= 0 {
		rle.RetryAfter = parseRetryAfterFromText(lower)
	}

	// Text-based fallback: extract reset timestamp from the body text
	// when JSON fields didn't provide one (e.g. "reset at 2026-08-22T07:00:00Z").
	if rle.ResetAt.IsZero() {
		rle.ResetAt = parseResetAtFromText(lower)
	}

	if !rle.ResetAt.IsZero() && rle.ResetAt.After(time.Now()) {
		if rle.RetryAfter <= 0 {
			rle.RetryAfter = untilResetAt(rle.ResetAt, time.Now())
		}
	}
	// No bounded fallback, no floor, no ceiling: a refusal carrying no
	// upstream retry signal keeps RetryAfter 0 and surfaces as-is.
	// Ledger window, computed after ResetAt/RetryAfter are finalized
	// (explicit-ResetAt 429s carry "reset"; timestamp-less ones carry just
	// RetryAfter → "retry-after").
	rle.Window = rateLimitWindow(body, rle)
	return rle
}

// hasFreebucksShortfall reports whether a rate-limit body carries the vendor's
// freebucks shortfall marker (freebucksShortfall / freebucks_shortfall). An
// absent key, an explicit JSON null, or an explicit false leaves the refusal
// plain, so a body that merely mentions the word can never flip the kind.
func hasFreebucksShortfall(target map[string]any) bool {
	for _, key := range []string{"freebucksShortfall", "freebucks_shortfall"} {
		if freebucksShortfallPresent(target[key]) {
			return true
		}
	}
	return false
}

// freebucksShortfallPresent reports whether a decoded freebucksShortfall value
// is a real marker: anything but JSON null and an explicit false counts (the
// vendor sends an object with the shortfall amounts).
func freebucksShortfallPresent(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	default:
		return true
	}
}

// banFromBody builds a BanError from a banned body, extracting the
// resumes_at timestamp best-effort. resumes_at may be RFC3339, unix seconds,
// or unix milliseconds (parseFlexTime). It is the single constructor for both
// the classification matrix (parseBan) and ProbeAccount's status conversion,
// so both sites produce identical typed errors (issue #306).
func banFromBody(body string) *BanError {
	be := &BanError{Body: truncate(body, 200)}
	var parsed struct {
		ResumesAt any    `json:"resumes_at"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err == nil {
		if t, perr := parseFlexTime(parsed.ResumesAt); perr == nil {
			be.ResumesAt = t
		}
	}
	return be
}

// parseBan builds a BanError from a 403 banned body (delegates to banFromBody).
func parseBan(body string) error {
	return banFromBody(body)
}

// countryBlockFromBody builds a CountryBlockedError from a country_blocked
// body, extracting countryCode/countryBlockReason/ipPrivacySignals
// best-effort (absent fields are tolerated). It is the single constructor for
// both the classification matrix (parseCountryBlock) and ProbeAccount's status
// conversion (issue #306).
func countryBlockFromBody(body string) *CountryBlockedError {
	cbe := &CountryBlockedError{}
	var parsed struct {
		CountryCode        string   `json:"countryCode"`
		CountryBlockReason string   `json:"countryBlockReason"`
		IpPrivacySignals   []string `json:"ipPrivacySignals"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err == nil {
		cbe.CountryCode = parsed.CountryCode
		cbe.CountryBlockReason = parsed.CountryBlockReason
		cbe.IpPrivacySignals = parsed.IpPrivacySignals
	}
	return cbe
}

// parseCountryBlock builds a CountryBlockedError from a 403 country_blocked
// body (delegates to countryBlockFromBody).
func parseCountryBlock(body string) error {
	return countryBlockFromBody(body)
}

// freeModeUnavailableFromBody builds a FreeModeUnavailableError from a
// free_mode_unavailable body, extracting message/countryCode/
// countryBlockReason/ipPrivacySignals best-effort (absent fields are
// tolerated; non-string signal entries are dropped like the CLI's parser).
func freeModeUnavailableFromBody(status int, body string) *FreeModeUnavailableError {
	fue := &FreeModeUnavailableError{Status: status, Body: truncate(body, 200)}
	var parsed struct {
		Message            string `json:"message"`
		CountryCode        string `json:"countryCode"`
		CountryBlockReason string `json:"countryBlockReason"`
		IpPrivacySignals   []any  `json:"ipPrivacySignals"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return fue
	}
	fue.Message = parsed.Message
	fue.CountryCode = parsed.CountryCode
	fue.CountryBlockReason = parsed.CountryBlockReason
	for _, sig := range parsed.IpPrivacySignals {
		if s, ok := sig.(string); ok && s != "" {
			fue.IpPrivacySignals = append(fue.IpPrivacySignals, s)
		}
	}
	return fue
}

// parseFreeModeUnavailable builds a FreeModeUnavailableError from a 403
// free_mode_unavailable body. Terminal: no Retry-After is read (retrying
// the same egress re-trips the gate) and no cooldown is ever scheduled.
func parseFreeModeUnavailable(status int, body string) error {
	return freeModeUnavailableFromBody(status, body)
}

// parseProviderUsage builds a ProviderUsageError from a 401/402 body
// carrying provider-billing wording. Never a cooldown: the token is
// healthy, the shared provider account needs a refill.
func parseProviderUsage(status int, body string) error {
	return &ProviderUsageError{Status: status, Body: truncate(body, 200)}
}

// parseConsentRequired builds a ConsentRequiredError from a 409
// consent_required body, extracting the walletSpend re-confirm amount
// best-effort.
func parseConsentRequired(status int, body string) error {
	cre := &ConsentRequiredError{Status: status, Body: truncate(body, 200)}
	var parsed struct {
		WalletConsent struct {
			WalletSpend float64 `json:"walletSpend"`
		} `json:"walletConsent"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err == nil {
		cre.WalletSpend = parsed.WalletConsent.WalletSpend
	}
	return cre
}

// parseFirstTabChanged builds a FirstTabChangedError from a 409
// first_tab_discount_changed body.
func parseFirstTabChanged(status int, body string) error {
	return &FirstTabChangedError{Status: status, Body: truncate(body, 200)}
}

// parseIpCapped builds an IpCappedError from a 429 ip_capped body,
// extracting retryAfterMs/activeUsersForIp/limit best-effort (absent fields
// are tolerated). The error carries the body's retryAfterMs verbatim (1m
// default when absent) — ip_capped is admission-only and not a quota reset
// upstream, so the parse never fabricates a Pacific-midnight window. The
// refusal surfaces directly; no cooldown is written.
func parseIpCapped(body string, headerRetryAfter time.Duration) error {
	ice := &IpCappedError{Body: truncate(body, 200), RetryAfter: headerRetryAfter}

	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err == nil {
		target := raw
		if errObj, ok := raw["error"].(map[string]any); ok {
			target = errObj
		}

		if ms, ok := getNumber(target, "retryAfterMs", "retry_after_ms"); ok && ms > 0 {
			ice.RetryAfter = CooldownFromMillis(ms)
		} else if sec, ok := getNumber(target, "retryAfter", "retry_after"); ok && sec > 0 {
			ice.RetryAfter = cooldownFromSeconds(sec)
		}

		if n, ok := getNumber(target, "activeUsersForIp", "active_users_for_ip"); ok {
			ice.ActiveUsersForIP = int(n)
		}
		if lim, ok := getNumber(target, "limit"); ok {
			ice.Limit = lim
		}
	}

	if ice.RetryAfter <= 0 {
		ice.RetryAfter = time.Minute
	}
	return ice
}

// reNoEndpointsModel extracts the model id from a "no endpoints found
// for <model>" body. Trailing sentence punctuation is trimmed at parse
// time; the match is best-effort and empty when the marker shape differs.
var reNoEndpointsModel = regexp.MustCompile(`no endpoints found for\s+([^\s"']+)`)

// reProviderUsageBill ports FREEBUFF_PROVIDER_USAGE_ERROR_PATTERN
// (common/src/constants/freebuff-errors.ts): provider-billing wording that
// survives into a 401/402 refusal body. Matched against the lowercased
// body, like every other marker in this matrix.
var reProviderUsageBill = regexp.MustCompile(`(?i)\b(?:(?:not enough|insufficient|out of)\s+credits?|(?:add|refill|top up)\s+(?:more\s+)?credits?)\b`)

// parseNoEndpoints builds a NoEndpointsError from a 404 no-endpoints body
// (issue #630). Never a cooldown, never a session invalidation: the
// refusal is model-scoped routing, not account or session state.
func parseNoEndpoints(status int, body string) error {
	model := ""
	if m := reNoEndpointsModel.FindStringSubmatch(strings.ToLower(body)); m != nil {
		model = strings.TrimRight(m[1], ".,;:")
	}
	return &NoEndpointsError{Status: status, Model: model, Body: truncate(body, 500)}
}

// isCapacityDeferred reports whether err is a free_mode_capacity_deferred
// response (the free tier's transient capacity queue).
func isCapacityDeferred(err error) bool {
	var cde *CapacityDeferredError
	return errors.As(err, &cde)
}

// isWaitingRoom reports whether err is an upstream waiting-room refusal: any
// 503 (the model has no serving slot right now) or the 429
// waiting_room_queued admission race. Both are transient queue conditions
// the chat path waits out same-session under the TRANSIENT_RETRIES budget.
func isWaitingRoom(err error) bool {
	var wr *WaitingRoomError
	return errors.As(err, &wr)
}

// queueRetryAfter extracts the honor-this-window delay from a transient
// queue error: the parsed Retry-After when upstream sent one, else 0 (the
// caller applies the 10s AI-SDK default).
func queueRetryAfter(err error) time.Duration {
	var cde *CapacityDeferredError
	if errors.As(err, &cde) && cde.RetryAfter > 0 {
		return cde.RetryAfter
	}
	var wr *WaitingRoomError
	if errors.As(err, &wr) && wr.RetryAfter > 0 {
		return wr.RetryAfter
	}
	return 0
}

// parseRetryAfter reads the Retry-After header (seconds or HTTP date).
func parseRetryAfter(hdr http.Header) time.Duration {
	raw := hdr.Get("Retry-After")
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
		return cooldownFromSeconds(float64(seconds))
	}
	if t, err := http.ParseTime(raw); err == nil {
		return untilResetAt(t, time.Now())
	}
	return 0
}

// parseFlexTime accepts RFC3339, unix seconds, or unix milliseconds.
func parseFlexTime(v any) (time.Time, error) {
	switch t := v.(type) {
	case nil:
		return time.Time{}, errors.New("nil time")
	case string:
		if t == "" {
			return time.Time{}, errors.New("empty time")
		}
		if parsed, err := time.Parse(time.RFC3339Nano, t); err == nil {
			return parsed, nil
		}
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			return parsed, nil
		}
		if secs, err := strconv.ParseInt(t, 10, 64); err == nil {
			return unixFrom(secs), nil
		}
		return time.Time{}, fmt.Errorf("unparseable time %q", t)
	case float64:
		return unixFrom(int64(t)), nil
	default:
		return time.Time{}, fmt.Errorf("unexpected time type %T", v)
	}
}
