package server

import (
	"context"
	"time"

	"freebuff-proxy/backend/internal/dashboard"
)

// UsageRecord is one completed chat's token split for the usage overview
// log (9Router-style). Cost is derived server-side by the store/API slice
// from the wire prices map where available; this slice only captures counts.
//
// Contract shared with the store/API slice (BackendUsageStoreApi) and the
// dashboard slice (FrontendUsageOverview): JSON keys ts_ms, req_id, model,
// input, output, cached, reasoning, total, ok.
type UsageRecord struct {
	TsMS   int64  `json:"ts_ms"`
	ReqID  string `json:"req_id"`
	Model  string `json:"model"`
	Input  int64  `json:"input"`
	Output int64  `json:"output"`
	Cached int64  `json:"cached"`
	Reason int64  `json:"reasoning"`
	Total  int64  `json:"total"`
	OK     bool   `json:"ok"`
}

// adaptUsageRecord maps the capture-side record onto the dashboard store
// type (dashboard cannot import server, so the store defines its own field
// names — notably TsMs/Reasoning — under the identical JSON-key contract).
func adaptUsageRecord(r UsageRecord) dashboard.UsageRecord {
	return dashboard.UsageRecord{
		TsMs:      r.TsMS,
		ReqID:     r.ReqID,
		Model:     r.Model,
		Input:     r.Input,
		Output:    r.Output,
		Cached:    r.Cached,
		Reasoning: r.Reason,
		Total:     r.Total,
		OK:        r.OK,
	}
}

// recordUsage persists one completed chat's UsageRecord into the dashboard
// usage ring. It runs unconditionally on completion — zero-usage completions
// still count as requests (OK=false) — and is safe on servers without a
// dashboard (RecordUsage is nil-receiver safe; test relays pass nil dash).
func (s *Server) recordUsage(ctx context.Context, stats *relayStats, model string) {
	s.dash.RecordUsage(adaptUsageRecord(newUsageRecord(ctx, stats, model)))
}

// usageSplitTokens extracts the per-completion token split from an upstream
// usage object. It accepts every shape the relays observe:
//
//   - chat completion: prompt_tokens / completion_tokens / total_tokens,
//     prompt_tokens_details.cached_tokens,
//     completion_tokens_details.reasoning_tokens
//   - Responses-native: input_tokens / output_tokens,
//     input_tokens_details.cached_tokens,
//     output_tokens_details.reasoning_tokens
//   - translated/legacy: prompt_cache_hit_tokens (flat cache-read count,
//     same source openAIUsageToAnthropic reads)
//
// Later shapes win when several are present, mirroring responsesUsage.
// total falls back to input+output, matching usageTotalTokens semantics.
// Returns zeros when usage is absent or malformed.
func usageSplitTokens(usage any) (input, output, cached, reasoning, total int64) {
	u, ok := usage.(map[string]any)
	if !ok || u == nil {
		return 0, 0, 0, 0, 0
	}
	if v, ok := intOf(u["prompt_tokens"]); ok {
		input = v
	}
	if v, ok := intOf(u["input_tokens"]); ok {
		input = v
	}
	if v, ok := intOf(u["completion_tokens"]); ok {
		output = v
	}
	if v, ok := intOf(u["output_tokens"]); ok {
		output = v
	}
	// Cached input tokens: detail maps first, flat echo last (any source
	// counts — they never co-occur on one usage object in practice).
	for _, key := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if d, ok := u[key].(map[string]any); ok {
			if v, ok := intOf(d["cached_tokens"]); ok && v > 0 {
				cached = v
			}
		}
	}
	if v, ok := intOf(u["prompt_cache_hit_tokens"]); ok && v > 0 {
		cached = v
	}
	if v, ok := intOf(u["cache_read_input_tokens"]); ok && v > 0 {
		cached = v
	}
	for _, key := range []string{"completion_tokens_details", "output_tokens_details"} {
		if d, ok := u[key].(map[string]any); ok {
			if v, ok := intOf(d["reasoning_tokens"]); ok && v > 0 {
				reasoning = v
			}
		}
	}
	if v, ok := intOf(u["total_tokens"]); ok && v > 0 {
		total = v
	} else {
		total = input + output
	}
	return input, output, cached, reasoning, total
}

// setUsage records the upstream usage block on the relay stats: the ledger
// total via usageTotalTokens (unchanged #122 behavior) plus the split fields
// for the usage log. Only adopts a real usage block — "usage":null must not
// zero a previously captured split. Callers already nil-guard; the guard
// here keeps direct calls safe too.
func (s *relayStats) setUsage(usage any) {
	if usage == nil {
		return
	}
	if _, ok := usage.(map[string]any); !ok {
		return
	}
	s.usageTokens = usageTotalTokens(usage)
	s.usageInput, s.usageOutput, s.usageCached, s.usageReasoning, _ = usageSplitTokens(usage)
}

// newUsageRecord builds the per-completion log record from the relay stats.
// model is the chatCore fallback; stats.servedModel (lease-bound, fallbacks
// included) wins when set. ok is true when the upstream carried a real usage
// block (total > 0) — zero-usage completions still emit a record so request
// counts stay honest.
func newUsageRecord(ctx context.Context, stats *relayStats, model string) UsageRecord {
	m := stats.servedModel
	if m == "" {
		m = model
	}
	total := stats.usageTokens
	return UsageRecord{
		TsMS:   time.Now().UnixMilli(),
		ReqID:  reqIDFrom(ctx),
		Model:  m,
		Input:  stats.usageInput,
		Output: stats.usageOutput,
		Cached: stats.usageCached,
		Reason: stats.usageReasoning,
		Total:  total,
		OK:     total > 0,
	}
}
