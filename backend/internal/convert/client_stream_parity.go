package convert

import (
	"encoding/json"
	"strconv"
)

// Client stream parity: terminal reshape/fan-out/fallback for streaming
// Anthropic + Responses relays (Phase 3, UNIVERSAL-GATEWAY-PLAN §6).
//
// The chat relay already withholds CLI-shaped argument fragments per
// tool-call index and injects reshaped wholes on the terminal chunk
// (server/openai_chunk_pipeline.go reshapeBuffer/reshapeFlush). The
// Anthropic and Responses streaming translators relayed delta bytes live
// with no withhold architecture, so OMP/pi turns on those surfaces kept
// CLI-shaped args and undispatchable calls. This file is the shared
// per-call core those translators finalize through: exactly one upstream
// tool call (wire name + complete args JSON) in, client calls + optional
// fallback text out.
//
// The order mirrors the non-streaming legs (ApplyTextFallbacks, then
// ReshapeCompletionCalls, then FromUpstreamChunk restore): fallback first,
// then reshape, then name restore. The ruled set and the fallback set are
// disjoint per family, so the order is defensive, not load-bearing.
//
// Invalid or partial args JSON never drops the call: it falls through to
// the verbatim passthrough (chat-parity: unreshapable buffers flush
// verbatim, no call is ever swallowed).

// StreamedCall is one client-dispatchable call finalized from a buffered
// upstream tool call: the restored client name, the dispatch id, and the
// final args JSON (reshaped or verbatim).
type StreamedCall struct {
	// ID is the dispatch id: the upstream id for the first call, and
	// <id>-fanout-<k> for fan-out extras (mirroring ReshapeMessageCalls),
	// so every dispatched call keeps a unique identity the client echoes
	// results against. Empty when upstream sent no id.
	ID string
	// Name is the restored CLIENT dispatch name.
	Name string
	// Arguments is the final args JSON string.
	Arguments string
}

// WithholdStreamArgs reports whether streaming argument fragments for wire
// must be buffered for terminal finalize instead of relayed live: exactly
// the calls the response leg transforms (reshape-ruled or fallback
// Stripped), gated on ResponseRewrite so unmapped families stay
// byte-identical live streams. Decidable from the wire name alone, before
// any args byte arrives.
func (m ToolMapper) WithholdStreamArgs(wire string) bool {
	if m.family == familyNone {
		return false
	}
	return m.HasReshapeRule(wire) || m.HasTextFallback(wire)
}

// FinalizeStreamedCall resolves one COMPLETE upstream tool call into the
// client calls to emit plus optional fallback text. calls carries the
// dispatchable calls in order (1:1 except fan-out, which yields N);
// text is rendered assistant text replacing a stripped unroutable call
// (empty unless the call rendered); absorb reports a stripped call with
// nothing to show (internal telemetry). A non-absorbed call always yields
// either ≥1 call or non-empty text — never silence.
func (m ToolMapper) FinalizeStreamedCall(wire, id, args string) (calls []StreamedCall, text string, absorb bool) {
	name := m.RestoreName(wire)
	single := func() []StreamedCall {
		return []StreamedCall{{ID: id, Name: name, Arguments: args}}
	}
	// Unroutable names strip first (same order as the non-streaming legs
	// and the chat stream strip): TextFallback absorbs unparseable args,
	// matching ApplyTextFallbacks on the same input — an undispatchable
	// call has no verbatim form worth preserving.
	if rendered, kind := m.TextFallback(wire, args); kind != TextFallbackNone {
		if kind == TextFallbackRender {
			return nil, rendered, false
		}
		return nil, "", true
	}
	// Partial/truncated args never reshape: relay verbatim like the live
	// path would have (chat-parity: unreshapable buffers flush verbatim,
	// no dispatchable call is ever swallowed).
	if !validStreamArgs(args) {
		return single(), "", false
	}
	if bodies, ok := m.ReshapeArgsFanout(wire, args); ok && len(bodies) > 0 {
		calls = make([]StreamedCall, 0, len(bodies))
		for k, body := range bodies {
			callID := id
			if k > 0 && id != "" {
				callID = id + "-fanout-" + strconv.Itoa(k)
			}
			calls = append(calls, StreamedCall{ID: callID, Name: name, Arguments: body})
		}
		return calls, "", false
	}
	return single(), "", false
}

// validStreamArgs reports whether args is complete JSON. Empty is invalid
// here (an empty-args call keeps no reshaped form); the caller falls back
// to verbatim passthrough. Uses encoding/json validity only — no schema
// judgment.
func validStreamArgs(args string) bool {
	if args == "" {
		return false
	}
	return json.Valid([]byte(args))
}
