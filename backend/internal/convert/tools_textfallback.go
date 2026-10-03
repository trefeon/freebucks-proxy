package convert

import (
	"encoding/json"
	"strings"
)

// Response-leg translation for canonical wire tools the client cannot
// dispatch.
//
// Both families see the canonical CLI definitions on the wire: OMP via the
// floor-only replacement (tools_floor.go), pi via the canonical substitution
// every core pi tool maps onto plus the upstream top-up. The response leg
// already restores client names and reshapes CLI args back to client shape
// (tools_reshape.go), but a call to a canonical tool the client has no
// equivalent for would restore by identity to a CLI name and the dispatcher
// answers "Tool <name> not found" (pi-agent-core/src/agent-loop.ts:2828-2839
// — OMP and pi share the agent loop).
//
// OMP: four of the sixteen have no OMP equivalent AT ALL — verified against
// OMP's live 18.5.0 builtin registry (BUILTIN_TOOL_NAMES: read, bash, edit,
// ast_grep, ast_edit, ask, debug, ida, eval, github, glob, grep, find, lsp,
// checkpoint, rewind, context_notes, new_context, security_scan, task,
// wait, todo, web_search, write, memory_edit, retain, recall, reflect,
// learn, manage_skill, plus hidden yield/goal/think; `find` is a registered
// semantic-search tool since 18.5.0, and hub/browser/computer/inspect_image
// are GONE — a call relayed under any of those answers "Tool <name> not
// found"). A call to one of those restores
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
//
// pi: the dispatch surface is its eight built-in tools (read, bash,
// powershell, edit, write, grep, find, ls), so every other canonical name is
// unroutable unless the client itself declared it — a pi extension tool whose
// own name IS the wire name restores by identity and stays a real call
// (piRoutable). Unroutable pi calls render the payload the model wanted to
// act on (todo checklist, search/read/skill request, blocking question) as
// assistant text and absorb telemetry, the same invariant as OMP: an
// unroutable call must never reach the client as a tool call.

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
// floor-only (OMP-family) requests consult this map: the three names are
// required on the wire by the gate, so the model can always call them and the
// response leg is the only place to degrade.
var textFloorRenderers = map[string]func(map[string]any) string{
	"suggest_followups":      renderSuggestFollowups,
	"render_ui":              renderFloatWidget,
	"report_project_profile": nil, // internal telemetry: nothing to show the user
}

// piFloorRenderers maps a canonical wire name pi cannot dispatch to its
// renderer (nil absorbs). Consulted only when the name does NOT restore to a
// pi-declared tool (piRoutable), so an extension that declares, say, its own
// web_search keeps receiving real calls.
var piFloorRenderers = map[string]func(map[string]any) string{
	"suggest_followups":      renderSuggestFollowups,
	"render_ui":              renderFloatWidget,
	"gravity_index":          renderGravityIndex,
	"report_project_profile": nil, // internal telemetry: nothing to show the user
	"write_todos":            renderTodoChecklist,
	"web_search":             renderMissingWebSearch,
	"read_url":               renderMissingReadURL,
	"ask_user":               renderQuestionList,
	"skill":                  renderMissingSkill,
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

// renderTodoChecklist turns the CLI write_todos state dump into a markdown
// checklist for pi, which has no todo tool: the user still sees the plan the
// turn recorded, and the model's next turn sees the same text.
func renderTodoChecklist(in map[string]any) string {
	todos, _ := in["todos"].([]any)
	lines := make([]string, 0, len(todos))
	for _, t := range todos {
		m, _ := t.(map[string]any)
		if m == nil {
			continue
		}
		title := strField(m, "task")
		if title == "" {
			title = strField(m, "content")
		}
		if title == "" {
			continue
		}
		box := "[ ]"
		if done, _ := m["completed"].(bool); done {
			box = "[x]"
		}
		lines = append(lines, "- "+box+" "+title)
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n\n**Tasks**\n" + strings.Join(lines, "\n")
}

// renderMissingWebSearch states the search request pi cannot service, so the
// user still sees what the turn wanted to do.
func renderMissingWebSearch(in map[string]any) string {
	note := "\n\n_Web search is unavailable in this client"
	if query := strField(in, "query"); query != "" {
		note += " (requested: " + query + ")"
	}
	return note + "._"
}

// renderMissingReadURL states the remote-read request pi cannot service
// (pi's read tool is filesystem-only).
func renderMissingReadURL(in map[string]any) string {
	note := "\n\n_Reading remote URLs is unavailable in this client"
	if url := strField(in, "url"); url != "" {
		note += " (requested: " + url + ")"
	}
	return note + "._"
}

// renderMissingSkill states the skill request pi cannot dispatch as a tool.
func renderMissingSkill(in map[string]any) string {
	note := "\n\n_Skills are unavailable in this client"
	if name := strField(in, "name"); name != "" {
		note += " (requested: " + name + ")"
	}
	return note + "._"
}

// renderQuestionList renders an ask_user question as assistant text: pi has
// no ask tool, so a blocking question can only be asked in the reply and
// answered on the user's next turn.
func renderQuestionList(in map[string]any) string {
	qs, _ := in["questions"].([]any)
	lines := make([]string, 0, len(qs))
	for _, q := range qs {
		qm, _ := q.(map[string]any)
		if qm == nil {
			continue
		}
		text := strField(qm, "question")
		if text == "" {
			continue
		}
		lines = append(lines, "- "+text)
		opts, _ := qm["options"].([]any)
		for _, o := range opts {
			om, _ := o.(map[string]any)
			if om == nil {
				continue
			}
			label := strField(om, "label")
			if label == "" {
				continue
			}
			line := "  - " + label
			if desc := strField(om, "description"); desc != "" {
				line += " — " + desc
			}
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n\n**Question for you**\n" + strings.Join(lines, "\n")
}

// TextFallback classifies one wire name + arguments for the client family's
// unroutable set. kind is TextFallbackNone for every ordinary call. Rendered
// text is empty for TextFallbackAbsorb and for a render whose payload carried
// nothing renderable (callers treat an empty render as an absorb).
//
// OMP empty-gravity guard: a gravity_index shape with no query source cannot
// ride as web_search (query is strict-required), so it renders the discovery
// note here instead. Query-ful shapes fall through to the web_search reshape
// — the guard is conditional on the args, which is why gravity_index has no
// static entry in textFloorRenderers below (the name-based strip cannot see
// it; the chat-streaming flush consults this same classifier on the whole
// args before injecting).
func (m ToolMapper) TextFallback(wireName, args string) (string, TextFallbackKind) {
	if m.family == familyOMP && wireName == "gravity_index" {
		var in map[string]any
		if args != "" {
			_ = json.Unmarshal([]byte(args), &in)
		}
		if gravityQuery(in) == "" {
			return renderGravityIndex(in), TextFallbackRender
		}
		return "", TextFallbackNone
	}
	render, ok := m.unroutableRenderer(wireName)
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

// unroutableRenderer resolves the family renderer for a canonical wire name,
// or reports that the call is ordinary. OMP uses the static floor table (its
// floor wire never carries a name the client declared). pi checks
// routability first, then the pi table: a name that restores to a declared pi
// tool is a real dispatch, not a render.
func (m ToolMapper) unroutableRenderer(wireName string) (func(map[string]any) string, bool) {
	switch m.family {
	case familyOMP:
		render, ok := textFloorRenderers[wireName]
		return render, ok
	case familyPi:
		if m.piRoutable(wireName) {
			return nil, false
		}
		render, ok := piFloorRenderers[wireName]
		return render, ok
	}
	return nil, false
}

// piRoutable reports whether a wire call dispatches on this pi client: the
// request leg mapped the wire name onto a client tool (upstreamToClient) or
// the client declared the wire name itself. RestoreName cannot answer the
// second case — ToUpstream stores no reverse entry when the client's own name
// IS the wire name — hence the raw clientTools table.
func (m ToolMapper) piRoutable(wireName string) bool {
	if m.upstreamToClient[wireName] != "" {
		return true
	}
	return m.clientTools[wireName]
}

// HasTextFallback reports whether wireName is an unroutable canonical name
// (renderable or absorbable) on this mapper.
func (m ToolMapper) HasTextFallback(wireName string) bool {
	_, ok := m.unroutableRenderer(wireName)
	return ok
}

// ApplyTextFallbacks rewrites an OpenAI-shaped completion for a family
// request: every choices[].message.tool_calls entry naming an unroutable
// canonical tool is removed, the rendered text is appended to the message
// content, and finish_reason flips tool_calls->stop when no call remains.
// Returns whether the completion changed. Unmapped requests are untouched.
func (m ToolMapper) ApplyTextFallbacks(completion map[string]any) bool {
	if m.family == familyNone {
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
