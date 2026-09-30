package convert

import (
	"encoding/json"
	"strings"
)

// Response-leg translation for floor tools OMP cannot dispatch.
//
// Floor-only OMP requests put exactly the 16 canonical CLI definitions on the
// wire (tools_floor.go): the free-tier gate rejects any foreign-schema rider,
// so the client's own tool defs never ride and the model chooses from the CLI
// set. The response leg already restores client names and reshapes CLI args
// back to OMP shape (tools_reshape.go). Four of the sixteen have no OMP
// equivalent AT ALL — verified against OMP's builtin registry
// (pi-coding-agent/src/tools/builtin-names.ts: read, bash, edit, ast_grep,
// ast_edit, ask, debug, ida, eval, github, glob, grep, find, lsp, checkpoint,
// rewind, context_notes, new_context, security_scan, task, wait, todo,
// web_search, write, memory_edit, retain, recall, reflect, learn,
// manage_skill, plus hidden yield/goal/think). A call to one of those restores
// by identity to a CLI name and the OMP dispatcher answers
// "Tool <name> not found" (pi-agent-core/src/agent-loop.ts:2828-2839).
//
// Those calls are therefore never relayed as tool calls: the response leg
// suppresses them and renders the user-facing payload as assistant text
// instead (or absorbs it when there is nothing to show). The invariant is
// "an unroutable floor call never reaches the client as a tool call", so the
// agent loop never takes an undispatchable turn.
//
// Which of the four are renderable is decided by where the payload lives: a
// tool whose ARGUMENTS are the user-facing content renders; a tool that only
// carries a request for a capability the client lacks (report_project_profile
// telemetry, gravity_index discovery) is absorbed. `ask` is deliberately NOT
// a route for suggest_followups: OMP's ask blocks the turn and auto-selects
// on timeout, which corrupts a fire-and-forget suggestion into a forced
// question.

// TextFallbackKind classifies how an unroutable floor call is handled.
type TextFallbackKind int

const (
	// TextFallbackNone means ordinary handling (name restore + arg reshape).
	TextFallbackNone TextFallbackKind = iota
	// TextFallbackRender suppresses the call and appends rendered text.
	TextFallbackRender
	// TextFallbackAbsorb suppresses the call with no replacement text.
	TextFallbackAbsorb
)

// textFloorRenderers maps an unroutable floor wire name to its renderer. A nil
// renderer absorbs the call (nothing user-facing in the payload). Only
// floor-only (OMP-family) requests consult this map: the four names are
// required on the wire by the gate, so the model can always call them and the
// response leg is the only place to degrade.
var textFloorRenderers = map[string]func(map[string]any) string{
	"suggest_followups":      renderSuggestFollowups,
	"render_ui":              renderFloatWidget,
	"gravity_index":          renderGravityIndex,
	"report_project_profile": nil, // internal telemetry: nothing to show the user
}

// renderSuggestFollowups turns the clickable followup cards the client would
// have shown into a markdown list of the same prompts.
func renderSuggestFollowups(in map[string]any) string {
	raw, _ := in["followups"].([]any)
	lines := make([]string, 0, len(raw))
	for _, item := range raw {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		prompt := strField(m, "prompt")
		if prompt == "" {
			prompt = strField(m, "label")
		}
		if prompt == "" {
			continue
		}
		lines = append(lines, "- "+prompt)
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n\n**Suggested next steps**\n" + strings.Join(lines, "\n")
}

// renderFloatWidget turns a render_ui button into a markdown link. A
// gravity_index link reference carries no URL the proxy can resolve (the
// client runtime substitutes it), so only a plain string link renders as a
// link; otherwise the label is emitted bare.
func renderFloatWidget(in map[string]any) string {
	w, _ := in["widget"].(map[string]any)
	if w == nil {
		return ""
	}
	text := strField(w, "text")
	link := ""
	switch l := w["link"].(type) {
	case string:
		link = l
	case map[string]any:
		link = strField(l, "url")
	}
	switch {
	case link != "" && text != "":
		return "\n\n[" + text + "](" + link + ")"
	case link != "":
		return "\n\n" + link
	case text != "":
		return "\n\n" + text
	}
	return ""
}

// renderGravityIndex states the discovery request the client cannot service,
// so the user still sees what the turn wanted to do. The model's next turn
// sees its own text and can pivot.
func renderGravityIndex(in map[string]any) string {
	action := strField(in, "action")
	if action == "" {
		action = "search"
	}
	note := "\n\n_Gravity Index `" + action + "` is unavailable in this client"
	if query := strField(in, "query"); query != "" {
		note += " (requested: " + query + ")"
	}
	return note + "._"
}

// TextFallback classifies one wire name + arguments for the unroutable floor
// set. kind is TextFallbackNone for every ordinary call. Rendered text is
// empty for TextFallbackAbsorb and for a render whose payload carried nothing
// renderable (callers treat an empty render as an absorb).
func (m ToolMapper) TextFallback(wireName, args string) (string, TextFallbackKind) {
	if !m.floorOnly {
		return "", TextFallbackNone
	}
	render, ok := textFloorRenderers[wireName]
	if !ok {
		return "", TextFallbackNone
	}
	if render == nil {
		return "", TextFallbackAbsorb
	}
	var in map[string]any
	if args != "" {
		if err := json.Unmarshal([]byte(args), &in); err != nil {
			return "", TextFallbackAbsorb
		}
	}
	text := render(in)
	if strings.TrimSpace(text) == "" {
		return "", TextFallbackAbsorb
	}
	return text, TextFallbackRender
}

// HasTextFallback reports whether wireName is an unroutable floor name
// (renderable or absorbable) on this mapper.
func (m ToolMapper) HasTextFallback(wireName string) bool {
	if !m.floorOnly {
		return false
	}
	_, ok := textFloorRenderers[wireName]
	return ok
}

// ApplyTextFallbacks rewrites an OpenAI-shaped completion for a floor-only
// (OMP) request: every choices[].message.tool_calls entry naming an
// unroutable floor tool is removed, the rendered text is appended to the
// message content, and finish_reason flips tool_calls->stop when no call
// remains. Returns whether the completion changed. Non-OMP requests are
// untouched.
func (m ToolMapper) ApplyTextFallbacks(completion map[string]any) bool {
	if !m.floorOnly {
		return false
	}
	changed := false
	for _, choice := range choicesOf(completion) {
		msg, _ := choice["message"].(map[string]any)
		if msg == nil {
			continue
		}
		tcs, _ := msg["tool_calls"].([]any)
		if len(tcs) == 0 {
			continue
		}
		kept := make([]any, 0, len(tcs))
		var text strings.Builder
		choiceChanged := false
		for _, raw := range tcs {
			tc, _ := raw.(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			name, _ := fn["name"].(string)
			args, _ := fn["arguments"].(string)
			rendered, kind := m.TextFallback(name, args)
			if kind == TextFallbackNone {
				kept = append(kept, raw)
				continue
			}
			choiceChanged = true
			if kind == TextFallbackRender {
				text.WriteString(rendered)
			}
		}
		if !choiceChanged {
			continue
		}
		changed = true
		if text.Len() > 0 {
			cur, _ := msg["content"].(string)
			msg["content"] = cur + text.String()
		}
		if len(kept) == 0 {
			delete(msg, "tool_calls")
			if fr, _ := choice["finish_reason"].(string); fr == "tool_calls" {
				choice["finish_reason"] = "stop"
			}
			continue
		}
		msg["tool_calls"] = kept
	}
	return changed
}
