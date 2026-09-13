package server

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Canned upstream usage block shared by every case below: prompt 321,
// completion 45, total 366, cached 12, reasoning 18.
const testUsageBlock = `"usage":{"prompt_tokens":321,"completion_tokens":45,"total_tokens":366,"prompt_tokens_details":{"cached_tokens":12},"completion_tokens_details":{"reasoning_tokens":18}}`

func testUsageChunk(id string) string {
	return testutilSSE(`{"id":"` + id + `","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` + testUsageBlock + `}`)
}

func TestUsageSplitTokens(t *testing.T) {
	chat := map[string]any{
		"prompt_tokens":     float64(321),
		"completion_tokens": float64(45),
		"total_tokens":      float64(366),
		"prompt_tokens_details": map[string]any{
			"cached_tokens": float64(12),
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": float64(18),
		},
	}
	in, out, cached, reasoning, total := usageSplitTokens(chat)
	if in != 321 || out != 45 || cached != 12 || reasoning != 18 || total != 366 {
		t.Errorf("chat split = %d/%d/%d/%d/%d, want 321/45/12/18/366", in, out, cached, reasoning, total)
	}
	// Spend-ledger total unchanged: split total must equal usageTotalTokens.
	if total != usageTotalTokens(chat) {
		t.Errorf("split total %d != ledger total %d", total, usageTotalTokens(chat))
	}

	// Responses-native key names.
	resp := map[string]any{
		"input_tokens":  float64(321),
		"output_tokens": float64(45),
		"total_tokens":  float64(366),
		"input_tokens_details": map[string]any{
			"cached_tokens": float64(12),
		},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": float64(18),
		},
	}
	if in, out, cached, reasoning, total := usageSplitTokens(resp); in != 321 || out != 45 || cached != 12 || reasoning != 18 || total != 366 {
		t.Errorf("responses split = %d/%d/%d/%d/%d, want 321/45/12/18/366", in, out, cached, reasoning, total)
	}

	// Flat Anthropic cache echo.
	flat := map[string]any{
		"prompt_tokens":           float64(321),
		"completion_tokens":       float64(45),
		"prompt_cache_hit_tokens": float64(12),
	}
	if _, _, cached, _, total := usageSplitTokens(flat); cached != 12 || total != 366 {
		t.Errorf("flat split cached/total = %d/%d, want 12/366", cached, total)
	}

	// Absent usage yields zeros; nil must not panic.
	if in, out, cached, reasoning, total := usageSplitTokens(nil); in != 0 || out != 0 || cached != 0 || reasoning != 0 || total != 0 {
		t.Errorf("nil split = %d/%d/%d/%d/%d, want zeros", in, out, cached, reasoning, total)
	}
}

// setUsage must not zero a captured split on a usage:null chunk.
func TestSetUsageKeepsSplitOnNull(t *testing.T) {
	stats := &relayStats{}
	stats.setUsage(map[string]any{
		"prompt_tokens": float64(321), "completion_tokens": float64(45), "total_tokens": float64(366),
	})
	stats.setUsage(nil)
	if stats.usageTokens != 366 || stats.usageInput != 321 || stats.usageOutput != 45 {
		t.Errorf("after nil: tokens/input/output = %d/%d/%d, want 366/321/45",
			stats.usageTokens, stats.usageInput, stats.usageOutput)
	}
}

func TestStreamUsageSplit(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()
	stats := &relayStats{}
	ss := strings.Join([]string{
		testutilSSE(`{"id":"chatcmpl-u1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`),
		testUsageChunk("chatcmpl-u1"),
		"data: [DONE]\n\n",
	}, "")
	s.relayStream(context.Background(), rec, strings.NewReader(ss), stats, time.Now())
	if stats.usageTokens != 366 {
		t.Errorf("stream ledger total = %d, want 366", stats.usageTokens)
	}
	if stats.usageInput != 321 || stats.usageOutput != 45 || stats.usageCached != 12 || stats.usageReasoning != 18 {
		t.Errorf("stream split = %d/%d/%d/%d, want 321/45/12/18",
			stats.usageInput, stats.usageOutput, stats.usageCached, stats.usageReasoning)
	}
}

func TestNonStreamUsageSplit(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()
	stats := &relayStats{}
	ss := strings.Join([]string{
		testutilSSE(`{"id":"chatcmpl-u2","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`),
		testUsageChunk("chatcmpl-u2"),
	}, "")
	s.relayJSON(context.Background(), rec, strings.NewReader(ss), stats, time.Now())
	if stats.usageTokens != 366 {
		t.Errorf("non-stream ledger total = %d, want 366", stats.usageTokens)
	}
	if stats.usageInput != 321 || stats.usageOutput != 45 || stats.usageCached != 12 || stats.usageReasoning != 18 {
		t.Errorf("non-stream split = %d/%d/%d/%d, want 321/45/12/18",
			stats.usageInput, stats.usageOutput, stats.usageCached, stats.usageReasoning)
	}
}

func TestAnthropicStreamUsageSplit(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()
	stats := &relayStats{}
	ss := strings.Join([]string{
		testutilSSE(`{"id":"chatcmpl-u3","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`),
		testUsageChunk("chatcmpl-u3"),
		"data: [DONE]\n\n",
	}, "")
	s.relayAnthropicStream(context.Background(), rec, strings.NewReader(ss), stats, time.Now(), "m", 0)
	if stats.usageTokens != 366 {
		t.Errorf("anthropic ledger total = %d, want 366", stats.usageTokens)
	}
	if stats.usageInput != 321 || stats.usageOutput != 45 || stats.usageCached != 12 || stats.usageReasoning != 18 {
		t.Errorf("anthropic split = %d/%d/%d/%d, want 321/45/12/18",
			stats.usageInput, stats.usageOutput, stats.usageCached, stats.usageReasoning)
	}
}

func TestNewUsageRecord(t *testing.T) {
	stats := &relayStats{
		servedModel:    "served-m",
		usageTokens:    366,
		usageInput:     321,
		usageOutput:    45,
		usageCached:    12,
		usageReasoning: 18,
	}
	ctx := context.WithValue(context.Background(), reqIDKey{}, "req-test-u1")
	rec := newUsageRecord(ctx, stats, "requested-m")
	if rec.Model != "served-m" {
		t.Errorf("model = %q, want served-m (servedModel wins)", rec.Model)
	}
	if rec.ReqID != "req-test-u1" {
		t.Errorf("req_id = %q, want req-test-u1", rec.ReqID)
	}
	if rec.Input != 321 || rec.Output != 45 || rec.Cached != 12 || rec.Reason != 18 || rec.Total != 366 {
		t.Errorf("record split = %d/%d/%d/%d/%d, want 321/45/12/18/366",
			rec.Input, rec.Output, rec.Cached, rec.Reason, rec.Total)
	}
	if !rec.OK {
		t.Errorf("ok = false, want true (total > 0)")
	}
	if rec.TsMS <= 0 {
		t.Errorf("ts_ms = %d, want positive", rec.TsMS)
	}

	// Lease-less fallback: requested model names the record; zero usage
	// still emits with ok=false so request counts stay honest.
	empty := newUsageRecord(context.Background(), &relayStats{}, "requested-m")
	if empty.Model != "requested-m" || empty.OK || empty.Total != 0 {
		t.Errorf("empty record = %+v, want model=requested-m ok=false total=0", empty)
	}
}

// TestAdaptUsageRecord pins the capture→store field mapping, including the
// differing Go names (TsMS/Reason) under the identical JSON-key contract.
func TestAdaptUsageRecord(t *testing.T) {
	got := adaptUsageRecord(UsageRecord{
		TsMS: 123, ReqID: "r1", Model: "m",
		Input: 321, Output: 45, Cached: 12, Reason: 18, Total: 366, OK: true,
	})
	if got.TsMs != 123 || got.ReqID != "r1" || got.Model != "m" ||
		got.Input != 321 || got.Output != 45 || got.Cached != 12 ||
		got.Reasoning != 18 || got.Total != 366 || !got.OK {
		t.Errorf("adapted record = %+v, want mapped split", got)
	}
}

// recordUsage on a dash-less server (nil dashboard) must not panic: test
// relays and dash-disabled servers record into the void.
func TestRecordUsageNilDash(t *testing.T) {
	s := testRelayServer()
	stats := &relayStats{usageTokens: 366, usageInput: 321, usageOutput: 45}
	s.recordUsage(context.Background(), stats, "m") // must not panic
}

// TestTraceChatLogsUsageSplit pins the traces-enrichment contract: the
// "chat trace" line carries integer attrs under the exact UsageRecord key
// names when a usage block was observed, and omits them otherwise.
func TestTraceChatLogsUsageSplit(t *testing.T) {
	var buf strings.Builder
	s := &Server{logger: slog.New(slog.NewTextHandler(&buf, nil))}
	st := &chatTraceState{reqID: "r-u1", usageInput: 321, usageOutput: 45, usageCached: 12, usageReasoning: 18, usageTotal: 366}
	s.traceChat(nil, "m", 7, "ok", "", map[string]int64{}, st)
	for _, want := range []string{"input=321", "output=45", "cached=12", "reasoning=18", "total=366"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("chat trace line missing %q: %s", want, buf.String())
		}
	}
	buf.Reset()
	s.traceChat(nil, "m", 7, "ok", "", nil, &chatTraceState{reqID: "r-u2"})
	for _, key := range []string{"input=", "output=", "cached=", "reasoning=", "total="} {
		if strings.Contains(buf.String(), key) {
			t.Errorf("chat trace line without usage must omit %q: %s", key, buf.String())
		}
	}
}
