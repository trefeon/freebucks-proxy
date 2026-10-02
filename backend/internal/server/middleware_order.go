// middleware_order.go - middleware-chain audit + transport guards
// (universal-gateway Phase 2.8).
//
// Canonical chain (server_routes.go Handler), outermost first:
//
//	gzip → access (+per-IP rate limit) → cors → mux
//
// Rationale, audited by middleware_order_test.go:
//   - gzip outermost: single compression point; streaming responses are
//     exempted inside by Content-Type gate, HEAD and no-body statuses skip.
//   - access (+rate limit) next: the correlation id is minted here, exactly
//     once, and every downstream log line shares it. The per-IP limiter
//     sits here so every /v1/* surface is covered (chat, Responses,
//     Anthropic, count_tokens, /v1/models) — a client cannot burn upstream
//     work through an unthrottled surface. Exempt: /admin/*, /healthz,
//     /metrics, OPTIONS preflights, non-/v1/ paths.
//   - cors inside the limiter: preflights answer 204 with the allow headers
//     on every /v1/* response; admin routes are untouched (cookie-authed,
//     SameSite=Strict).
//   - mux innermost: route table only.
//
// Per-/v1/ request-plane order (auth → model → cost → limit → parse →
// stream): auth middleware (middleware_auth.go) → model/agent resolution
// (Acquire) → spend ceilings (server-enforced, ledger only records) →
// rate-limit/cooldown gates (pool walkGates) → body parse/normalize
// (convert, single decode) → stream relay. User headers never win over
// proxy defaults on the upstream leg because client headers are never
// forwarded: the upstream request is built by the proxy (upstream/chat.go,
// client_chat.go) and the inbound X-Request-Id is logged as
// client_request_id only, never adopted as the correlation id.
//
// Terminal SSE events (openziti enforcement precedent: a missing
// Done/Err is a truncation error): chat terminates on data:[DONE],
// Responses on response.completed, Anthropic on message_stop. The
// per-surface relays enforce this (conformance_*_test.go); TerminalEventFor
// below is the shared vocabulary so new surfaces cannot invent a fourth
// terminator.
//
// Zero-copy fast path: deliberately absent. Every /v1 body normalizes
// through convert so the floor gate sees it; a passthrough that skips the
// gate would re-open the foreign-rider hole invariant §2.1 closes.
package server

import (
	"fmt"
	"strings"
)

// MiddlewareLayer names one documented chain position, outermost first.
type MiddlewareLayer string

const (
	// LayerGzip compresses everything except streams/HEAD/empty.
	LayerGzip MiddlewareLayer = "gzip"
	// LayerAccess mints the correlation id and applies per-IP limiting.
	LayerAccess MiddlewareLayer = "access+ratelimit"
	// LayerCORS answers /v1/* preflights and stamps allow headers.
	LayerCORS MiddlewareLayer = "cors"
	// LayerMux is the route table.
	LayerMux MiddlewareLayer = "mux"
)

// DocumentedMiddlewareOrder is the canonical chain, outermost first. The
// audit test pins the observable behavior of each layer; a reorder must
// update this table and the test together.
func DocumentedMiddlewareOrder() []MiddlewareLayer {
	return []MiddlewareLayer{LayerGzip, LayerAccess, LayerCORS, LayerMux}
}

// Terminal events per client surface: the last frame a healthy stream
// must carry. A stream ending without its terminal event is truncated.
var surfaceTerminalEvents = map[SurfaceFamily]string{
	SurfaceChat:      "[DONE]",
	SurfaceResponses: "response.completed",
	SurfaceAnthropic: "message_stop",
}

// TerminalEventFor returns the terminal event for a client surface, or an
// error for unknown families (no generic terminator — a new surface
// declares its own).
func TerminalEventFor(fam SurfaceFamily) (string, error) {
	if term, ok := surfaceTerminalEvents[fam]; ok {
		return term, nil
	}
	return "", fmt.Errorf("server: unknown client surface %q (no terminal event)", fam)
}

// MissingTerminalError reports a stream that ended without its terminal
// event: the turn is truncated, never complete. want is the TerminalEventFor
// value for the surface; tail names the last event seen ("<eof>" when the
// body closed with no events at all).
func MissingTerminalError(surface, want, tail string) error {
	if tail == "" {
		tail = "<eof>"
	}
	return fmt.Errorf("server: %s stream ended without terminal %s (last: %s)", surface, want, tail)
}

// AssertTerminalEvent checks that events ends with the surface terminal:
// the last event must equal want exactly (a terminal followed by trailer
// frames is a relay bug, not a clean close).
func AssertTerminalEvent(surface string, events []string, want string) error {
	if len(events) == 0 {
		return MissingTerminalError(surface, want, "")
	}
	if last := events[len(events)-1]; last != want {
		return MissingTerminalError(surface, want, last)
	}
	return nil
}

// IsTerminalEventLine reports whether one raw SSE data line carries the
// surface terminal: "[DONE]" matches verbatim; event-typed surfaces match
// `"type":"<terminal>"` in the JSON payload.
func IsTerminalEventLine(fam SurfaceFamily, line string) bool {
	term, err := TerminalEventFor(fam)
	if err != nil {
		return false
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "" {
		return false
	}
	if term == "[DONE]" {
		return data == "[DONE]"
	}
	return strings.Contains(data, `"type":"`+term+`"`)
}
