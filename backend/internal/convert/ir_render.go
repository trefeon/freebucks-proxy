package convert

import (
	"encoding/json"
	"strconv"
)

// ir_render.go — family renderers over []IRCall (Phase 1.4 of
// docs/UNIVERSAL-GATEWAY-PLAN.md).
//
// Response leg: Assemble → IRCalls → Render → Restore → Encode.
// FromUpstreamChunk parses wire calls into IRCalls; the family renderers map
// one []IRCall to client dispatches (1:1, 1:N fan-out, text fallback); the
// Reverse map restores names; the surface encodes.
//
// Every rule delegates to the established family paths
// (ReshapeArgsFanout/TextFallback/RestoreName), so OMP fan-out
// (read_files{paths}→N×read, str_replace{replacements}→N×edit), pi batch
// shapes (ONE edit{edits[]}, per-path read keeping offset/limit), and the
// unroutable text fallbacks behave byte-identically — proven by the render
// parity test (ir_wireproof_test.go).

// IRCall is one canonical wire call: the wire name, the client name it
// restores to, the raw JSON arguments, and the call id.
type IRCall struct {
	WireName   string
	ClientName string
	Args       string
	ID         string
}

// AssembleIRCalls parses OpenAI-shaped tool_calls entries (message or delta
// shape) into IRCalls, resolving client names through the set's Reverse map.
// Entries without a usable name ride through with ClientName == WireName.
func AssembleIRCalls(set IRToolSet, toolCalls []any) []IRCall {
	var out []IRCall
	for _, raw := range toolCalls {
		tc, _ := raw.(map[string]any)
		if tc == nil {
			continue
		}
		fn, _ := tc["function"].(map[string]any)
		if fn == nil {
			continue
		}
		wire, _ := fn["name"].(string)
		if wire == "" {
			continue
		}
		args, _ := fn["arguments"].(string)
		id, _ := tc["id"].(string)
		out = append(out, IRCall{
			WireName: wire, ClientName: set.RestoreName(wire),
			Args: args, ID: id,
		})
	}
	return out
}

// AssembleAnthropicCalls parses Anthropic tool_use content blocks into
// IRCalls (input marshals back to the JSON args the reshape rules read).
func AssembleAnthropicCalls(set IRToolSet, blocks []any) []IRCall {
	var out []IRCall
	for _, raw := range blocks {
		b, _ := raw.(map[string]any)
		if b == nil {
			continue
		}
		if typ, _ := b["type"].(string); typ != "tool_use" && typ != "server_tool_use" {
			continue
		}
		wire, _ := b["name"].(string)
		if wire == "" {
			continue
		}
		id, _ := b["id"].(string)
		args := ""
		if input, ok := b["input"]; ok && input != nil {
			if s, ok := input.(string); ok {
				args = s
			} else if body, err := json.Marshal(input); err == nil {
				args = string(body)
			}
		}
		out = append(out, IRCall{
			WireName: wire, ClientName: set.RestoreName(wire),
			Args: args, ID: id,
		})
	}
	return out
}

// RenderedCall is one client dispatch: the restored client name, the
// reshaped JSON args, and — when the call is unroutable on this client —
// the suppression with its rendered text. Quality + Diagnostics record the
// loss per call.
type RenderedCall struct {
	ClientName  string
	Args        string
	ID          string
	Suppressed  bool   // unroutable: never relay as a tool call
	Text        string // rendered payload ("" when absorbed)
	Quality     ConversionQuality
	Diagnostics []IRDiagnostic
}

// RenderIRCalls maps wire calls to client dispatches for one turn:
// restore the name, apply the unroutable text fallback, else reshape args
// (fanning out read_files/str_replace into N calls with suffixed ids,
// mirroring ReshapeMessageCalls). Order preserved; unknown shapes pass
// through verbatim — reshaping never drops a call.
func RenderIRCalls(m ToolMapper, calls []IRCall) []RenderedCall {
	var out []RenderedCall
	for _, c := range calls {
		out = append(out, renderIRCall(m, c)...)
	}
	return out
}

func renderIRCall(m ToolMapper, c IRCall) []RenderedCall {
	client := m.RestoreName(c.WireName)
	if text, kind := m.TextFallback(c.WireName, c.Args); kind != TextFallbackNone {
		diag := IRDiagnostic{
			Code: DiagTextRender, Path: "tool_calls",
			Message:  "unroutable floor call rendered as text",
			Severity: SeverityWarning, From: c.WireName, To: client,
		}
		quality := QualityLossyDocumented
		if text == "" || kind == TextFallbackAbsorb {
			diag.Code = DiagTextAbsorb
			diag.Message = "unroutable floor call absorbed"
			quality = QualityDegraded
		}
		return []RenderedCall{{
			ClientName: client, Args: c.Args, ID: c.ID,
			Suppressed: true, Text: text,
			Quality: quality, Diagnostics: []IRDiagnostic{diag},
		}}
	}
	bodies, ok := m.ReshapeArgsFanout(c.WireName, c.Args)
	if !ok || len(bodies) == 0 {
		return []RenderedCall{{
			ClientName: client, Args: c.Args, ID: c.ID,
			Quality: QualityLossless,
		}}
	}
	diag := IRDiagnostic{
		Code: DiagReshapeApplied, Path: "tool_calls",
		Message:  "CLI args reshaped to client shape",
		Severity: SeverityWarning, From: c.WireName, To: client,
	}
	rendered := make([]RenderedCall, 0, len(bodies))
	for k, body := range bodies {
		id := c.ID
		if k > 0 && id != "" {
			id = id + "-fanout-" + strconv.Itoa(k)
		}
		// NOTE: ReshapeMessageCalls suffixes extras as -fanout-<k+1>
		// (k indexes bodies[1:]); here k indexes bodies, so the first
		// extra (k=1) is -fanout-1 in both. Identical ids, proven by
		// the fan-out parity test.
		rendered = append(rendered, RenderedCall{
			ClientName: client, Args: body, ID: id,
			Quality:     QualityLossyDocumented,
			Diagnostics: []IRDiagnostic{diag},
		})
	}
	return rendered
}
