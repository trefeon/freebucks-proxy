package convert

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Response-leg argument reshape for floor-only (OMP-family) requests.
//
// The model fills CLI-shaped args per the canonical floor definitions it was
// shown; the OMP dispatcher validates per its own schemas, so CLI-shaped
// args must be reshaped to OMP shape alongside the name restore. Keyed by
// WIRE name (floor-official, unambiguous) and gated on FloorOnly: no other
// client's relay ever reaches these paths.
//
// Two wire shapes fan out to N OMP calls, order preserved: read_files
// {paths:[...]} becomes one read {path} per entry, and str_replace
// {path, replacements:[...]} becomes one edit {path, old_string,
// new_string} per replacement. A single path / single replacement stays
// exactly 1:1. Every other rule yields exactly one call.
//
// All rules are total and lossy-documented: unknown shapes pass through
// verbatim rather than failing the turn. Kept scalar drops (grep flags,
// bash background extras, write instructions, web_search depth, read
// offset/limit, edit allowMultiple, glob cwd/max_results, OMP-find
// query/grep_keywords variants) are documented at each rule; whole tools
// with no CLI equivalent (eval, task, wait, learn, manage_skill,
// new_context, context_notes) never reach the wire at all — floorOnlyOMP
// drops them so the model is never shown them.

// ReshapeArgsFor rewrites CLI-shaped arguments JSON for wireName into the
// OMP client shape. Returns the rewritten JSON and true, or ("", false) when
// no rule applies (caller keeps the args verbatim). Single-first: when the
// wire shape fans out (read_files, str_replace) this returns the FIRST call;
// ReshapeArgsFanout delivers all N.
func (m ToolMapper) ReshapeArgsFor(wireName, args string) (string, bool) {
	outs, ok := m.ReshapeArgsFanout(wireName, args)
	if !ok || len(outs) == 0 {
		return "", false
	}
	return outs[0], true
}

// ReshapeArgsFanout rewrites CLI-shaped arguments JSON for wireName into one
// OMP-shaped arguments JSON per resulting client call, order preserved:
// read_files with N paths yields N read args, str_replace with N
// replacements yields N edit args; every other rule yields exactly one.
// Returns (nil, false) passthrough when no rule applies or the shape carries
// nothing to reshape — the caller keeps the args verbatim, so no call is
// ever dropped by reshaping.
func (m ToolMapper) ReshapeArgsFanout(wireName, args string) ([]string, bool) {
	if !m.floorOnly {
		return nil, false
	}
	resolved := resolveReshapeRule(wireName)
	if _, ok := reshapeRules[resolved]; !ok {
		return nil, false
	}
	var in map[string]any
	dec := json.NewDecoder(strings.NewReader(args))
	dec.UseNumber()
	if err := dec.Decode(&in); err != nil || in == nil {
		return nil, false
	}
	// OMP-vocabulary emission (live 2026-09-30: a turn called OMP-native
	// "read" with OMP-native {path}): the name resolves to a wire rule,
	// but args already in OMP shape must pass through — reshaping them
	// would corrupt valid calls (e.g. read {path} into {path:""}). A
	// wire-name emission always carries wire-schema args (the model fills
	// what it was shown). So an OMP-name call reshapes only when it
	// carries CLI-distinctive keys.
	if resolved != wireName && !hasAnyKey(in, reshapeCliKeys[resolved]) {
		return nil, false
	}
	outs := m.fanoutArgs(resolved, in)
	if len(outs) == 0 {
		return nil, false
	}
	bodies := make([]string, 0, len(outs))
	for _, out := range outs {
		b, err := json.Marshal(out)
		if err != nil {
			return nil, false
		}
		bodies = append(bodies, string(b))
	}
	return bodies, true
}

// fanoutArgs maps one parsed CLI-shaped arguments object to one OMP-shaped
// map per resulting client call. Nil/empty means passthrough (never a drop).
func (m ToolMapper) fanoutArgs(resolved string, in map[string]any) []map[string]any {
	switch resolved {
	case "read_files":
		// OMP read {path} is singular: one call per entry, order
		// preserved. Object entries {path, offset, limit} keep the path;
		// offset/limit have no OMP equivalent and are dropped. No paths
		// key means already OMP shape: nil (passthrough), never blanked.
		paths, ok := in["paths"].([]any)
		if !ok {
			return nil
		}
		var outs []map[string]any
		for _, p := range paths {
			switch v := p.(type) {
			case string:
				outs = append(outs, map[string]any{"path": v})
			case map[string]any:
				outs = append(outs, map[string]any{"path": strField(v, "path")})
			}
		}
		return outs
	case "str_replace":
		// OMP edit {path, old_string, new_string} is one replacement per
		// call: one call per array entry, order preserved. allowMultiple
		// has no OMP equivalent and is dropped. No replacements key means
		// already OMP shape: nil (passthrough).
		reps, ok := in["replacements"].([]any)
		if !ok {
			return nil
		}
		path := strField(in, "path")
		var outs []map[string]any
		for _, r := range reps {
			rm, ok := r.(map[string]any)
			if !ok {
				continue
			}
			outs = append(outs, map[string]any{
				"path":       path,
				"old_string": strField(rm, "oldString"),
				"new_string": strField(rm, "newString"),
			})
		}
		return outs
	case "glob":
		return m.fanoutGlobArgs(in)
	default:
		if rule, ok := reshapeRules[resolved]; ok {
			if out := rule(in); out != nil {
				return []map[string]any{out}
			}
		}
		return nil
	}
}

// fanoutGlobArgs reshapes CLI glob {pattern, ...} to the OMP shape dictated
// by the request origin: a wire glob claimed by the client's `find` tool
// restores as OMP find {pattern}, a native glob as OMP glob {path}.
// The pattern source is the first present of pattern/query: the live OMP
// prompt emits find with query+grep_keywords vocabulary while the wire never
// shows a find def, so either key may arrive; cwd/max_results/grep_keywords
// have no OMP-find equivalent and are dropped. No pattern source at all
// means already OMP shape (native {path} emission): nil (passthrough),
// never a blanked-out path.
func (m ToolMapper) fanoutGlobArgs(in map[string]any) []map[string]any {
	src := strField(in, "pattern")
	if src == "" {
		src = strField(in, "query")
	}
	if src == "" {
		return nil
	}
	if m.upstreamToClient["glob"] == "find" {
		return []map[string]any{{"pattern": src}}
	}
	return []map[string]any{{"path": src}}
}

// HasReshapeRule reports whether wireName has a reshape rule on this
// mapper (floor-only OMP requests only).
func (m ToolMapper) HasReshapeRule(wireName string) bool {
	if !m.floorOnly {
		return false
	}
	_, ok := reshapeRules[resolveReshapeRule(wireName)]
	return ok
}

// ompClientToWire covers the mapped OMP names the model emits from prompt
// vocabulary instead of the wire schema (live 2026-09-30: a turn called
// "read" with CLI-shaped {paths} though the wire only showed read_files).
// Their calls restore by identity, but the args still need the wire rule.
var ompClientToWire = map[string]string{
	"bash":  "run_terminal_command",
	"read":  "read_files",
	"edit":  "str_replace",
	"write": "write_file",
	"grep":  "code_search",
	"todo":  "write_todos",
}

// wire name first, then the OMP client name the model may emit instead.
func resolveReshapeRule(name string) string {
	if _, ok := reshapeRules[name]; ok {
		return name
	}
	if wire, ok := ompClientToWire[name]; ok {
		return wire
	}
	return name
}

// reshapeCliKeys lists per-wire-rule argument keys that only exist in the
// CLI schema, never in the OMP shape. An OMP-vocabulary call carrying none
// of these is already OMP-shaped and passes through untouched.
var reshapeCliKeys = map[string][]string{
	"run_terminal_command": {"timeout_seconds", "process_type"},
	"read_files":           {"paths"},
	"str_replace":          {"replacements"},
	"write_file":           {"instructions"},
	"code_search":          {"flags", "maxResults"},
	"glob":                 {"pattern"},
	"write_todos":          {"todos"},
	"web_search":           {"depth"},
}

func hasAnyKey(m map[string]any, keys []string) bool {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

// ReshapeMessageCalls rewrites CLI-shaped arguments to OMP shape for
// non-streaming message.tool_calls entries, keyed by the WIRE name (call
// before FromUpstreamChunk restores names). Single-path and
// single-replacement calls rewrite 1:1 in place; multi-path read_files fans
// out to one entry per path and multi-replacement str_replace to one entry
// per replacement, extras inserted immediately after their parent with
// suffixed ids (<id>-fanout-<k>) so every dispatched call keeps a unique
// identity the client can echo results against. Order is preserved
// throughout. Returns the (possibly longer) call list and whether anything
// changed; entries without a rule keep byte-identical args.
func (m ToolMapper) ReshapeMessageCalls(tcs []any) ([]any, bool) {
	if !m.floorOnly {
		return tcs, false
	}
	changed := false
	out := make([]any, 0, len(tcs))
	for _, raw := range tcs {
		out = append(out, raw)
		tc, _ := raw.(map[string]any)
		if tc == nil {
			continue
		}
		fn, _ := tc["function"].(map[string]any)
		if fn == nil {
			continue
		}
		wire, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		if wire == "" || args == "" {
			continue
		}
		bodies, ok := m.ReshapeArgsFanout(wire, args)
		if !ok || len(bodies) == 0 {
			continue
		}
		fn["arguments"] = bodies[0]
		changed = true
		for k, body := range bodies[1:] {
			extra := make(map[string]any, len(tc)+1)
			for key, val := range tc {
				extra[key] = val
			}
			extraFn := make(map[string]any, len(fn)+1)
			for key, val := range fn {
				extraFn[key] = val
			}
			extraFn["arguments"] = body
			extra["function"] = extraFn
			if id, _ := tc["id"].(string); id != "" {
				extra["id"] = id + "-fanout-" + strconv.Itoa(k+1)
			}
			out = append(out, extra)
		}
	}
	return out, changed
}

// ReshapeCompletionCalls fans out + reshapes CLI-shaped args across every
// choice's message.tool_calls in a completion object, in place. Non-streaming
// legs (chat, Anthropic, Responses) call this BEFORE FromUpstreamChunk
// restores names — the rules key on the wire name. Returns whether anything
// changed.
func (m ToolMapper) ReshapeCompletionCalls(completion map[string]any) bool {
	if !m.floorOnly {
		return false
	}
	rawChoices, ok := completion["choices"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, raw := range rawChoices {
		choice, _ := raw.(map[string]any)
		if choice == nil {
			continue
		}
		msg, _ := choice["message"].(map[string]any)
		if msg == nil {
			continue
		}
		tcs, ok := msg["tool_calls"].([]any)
		if !ok {
			continue
		}
		if expanded, ok := m.ReshapeMessageCalls(tcs); ok {
			msg["tool_calls"] = expanded
			changed = true
		}
	}
	return changed
}

func strField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func numField(m map[string]any, key string) (json.Number, bool) {
	switch v := m[key].(type) {
	case json.Number:
		return v, true
	case float64:
		return json.Number(strings.TrimRight(strings.TrimRight(jsonFloat(v), "0"), ".")), true
	}
	return "", false
}

func jsonFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

var reshapeRules = map[string]func(map[string]any) map[string]any{
	// OMP bash {command, cwd?, timeout?, ...} — extras have no CLI
	// equivalent and are dropped; the `i` intent is omitted (the OMP
	// dispatcher tolerates missing `i`, PI_NO_INTENT precedent).
	"run_terminal_command": func(in map[string]any) map[string]any {
		out := map[string]any{"command": strField(in, "command")}
		if cwd := strField(in, "cwd"); cwd != "" {
			out["cwd"] = cwd
		}
		if t, ok := numField(in, "timeout_seconds"); ok {
			out["timeout"] = t
		}
		return out
	},
	// OMP write {path, content} — CLI instructions dropped.
	"write_file": func(in map[string]any) map[string]any {
		return map[string]any{
			"path":    strField(in, "path"),
			"content": strField(in, "content"),
		}
	},
	// OMP read {path} — singular per call; the fan-out core (fanoutArgs)
	// expands one call per path, this single is the first. No paths key
	// means already OMP shape: nil (passthrough) rather than a
	// blanked-out path.
	"read_files": func(in map[string]any) map[string]any {
		paths, _ := in["paths"].([]any)
		if len(paths) == 0 {
			return nil
		}
		first := ""
		switch p := paths[0].(type) {
		case string:
			first = p
		case map[string]any:
			first = strField(p, "path")
		}
		return map[string]any{"path": first}
	},
	// OMP edit {path, old_string, new_string} — flat pair, one call per
	// replacement via the fan-out core, this single is the first. No
	// replacements key means already OMP shape: nil, never blanked-out
	// strings.
	"str_replace": func(in map[string]any) map[string]any {
		reps, _ := in["replacements"].([]any)
		if len(reps) == 0 {
			return nil
		}
		out := map[string]any{"path": strField(in, "path")}
		if r, ok := reps[0].(map[string]any); ok {
			out["old_string"] = strField(r, "oldString")
			out["new_string"] = strField(r, "newString")
			return out
		}
		return nil
	},
	// OMP grep {pattern, path?} — CLI cwd becomes path; flags/maxResults
	// dropped.
	"code_search": func(in map[string]any) map[string]any {
		out := map[string]any{"pattern": strField(in, "pattern")}
		if cwd := strField(in, "cwd"); cwd != "" {
			out["path"] = cwd
		}
		return out
	},
	// Native OMP glob {path, ...} is path-listing, not pattern search: the
	// CLI pattern rides as path (best-effort; magic chars pass through to
	// the OMP glob implementation). Membership in this map keeps
	// HasReshapeRule true for the streaming buffer gate; the live reshape
	// runs through fanoutGlobArgs, which routes ex-find origins to OMP
	// find {pattern} instead. No pattern/query source means already OMP
	// shape (native {path} emission): nil (passthrough), never a
	// blanked-out path.
	"glob": func(in map[string]any) map[string]any {
		if p := strField(in, "pattern"); p != "" {
			return map[string]any{"path": p}
		}
		return nil
	},
	// OMP todo is an op-machine, CLI write_todos a state dump: re-init with
	// completed}]}]} (live-probed 2026-09-30: bare init errors "Missing
	// list for init operation"; flat items rely on the repair path).
	// Completed flags ride through per item. All-done dumps read back as
	// a view (init rejects empty items).
	"write_todos": func(in map[string]any) map[string]any {
		todos, _ := in["todos"].([]any)
		items := make([]any, 0, len(todos))
		for _, t := range todos {
			m, ok := t.(map[string]any)
			if !ok {
				continue
			}
			title := strField(m, "task")
			if title == "" {
				title = strField(m, "content")
			}
			if title == "" {
				continue
			}
			done, _ := m["completed"].(bool)
			items = append(items, map[string]any{"task": title, "completed": done})
		}
		if len(items) == 0 {
			return map[string]any{"op": "view"}
		}
		return map[string]any{
			"op":   "init",
			"list": []any{map[string]any{"phase": "Tasks", "items": items}},
		}
	},
	// OMP web_search {query, ...} — CLI depth dropped.
	"web_search": func(in map[string]any) map[string]any {
		return map[string]any{"query": strField(in, "query")}
	},
	// ask_user -> OMP ask {questions:[{id, question, options}]}: ids
	// synthesized per index (OMP requires them), option labels and
	// descriptions carried verbatim.
	"ask_user": func(in map[string]any) map[string]any {
		qs, _ := in["questions"].([]any)
		if len(qs) == 0 {
			return nil
		}
		// Already OMP shape (every item carries an id): passthrough.
		native := true
		for _, q := range qs {
			qm, _ := q.(map[string]any)
			if qm == nil || strField(qm, "id") == "" {
				native = false
				break
			}
		}
		if native {
			return nil
		}
		out := make([]any, 0, len(qs))
		for i, q := range qs {
			qm, _ := q.(map[string]any)
			if qm == nil {
				continue
			}
			entry := map[string]any{
				"id":       "q" + strconv.Itoa(i),
				"question": strField(qm, "question"),
			}
			if opts, ok := qm["options"].([]any); ok {
				clean := make([]any, 0, len(opts))
				for _, o := range opts {
					om, _ := o.(map[string]any)
					if om == nil {
						continue
					}
					item := map[string]any{"label": strField(om, "label")}
					if d := strField(om, "description"); d != "" {
						item["description"] = d
					}
					clean = append(clean, item)
				}
				entry["options"] = clean
			}
			out = append(out, entry)
		}
		return map[string]any{"questions": out}
	},
	// read_url -> OMP read {path}: OMP reads http(s) URLs through read;
	// max_chars dropped. No url means already OMP shape: nil.
	"read_url": func(in map[string]any) map[string]any {
		if strField(in, "url") == "" {
			return nil
		}
		return map[string]any{"path": strField(in, "url")}
	},
	// skill -> OMP read {path}: skill content resolves through the
	// skill:// internal URI the read tool serves. Empty name: nil.
	"skill": func(in map[string]any) map[string]any {
		if strField(in, "name") == "" {
			return nil
		}
		return map[string]any{"path": "skill://" + strField(in, "name")}
	},
}
