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
// All rules are total and lossy-documented: unknown shapes pass through
// verbatim rather than failing the turn.

// ReshapeArgsFor rewrites CLI-shaped arguments JSON for wireName into the
// OMP client shape. Returns the rewritten JSON and true, or ("", false) when
// no rule applies (caller keeps the args verbatim).
func (m ToolMapper) ReshapeArgsFor(wireName, args string) (string, bool) {
	if !m.floorOnly {
		return "", false
	}
	resolved := resolveReshapeRule(wireName)
	rule, ok := reshapeRules[resolved]
	if !ok {
		return "", false
	}
	var in map[string]any
	dec := json.NewDecoder(strings.NewReader(args))
	dec.UseNumber()
	if err := dec.Decode(&in); err != nil || in == nil {
		return "", false
	}
	// OMP-vocabulary emission (live 2026-09-30: a turn called OMP-native
	// "read" with OMP-native {path}): the name resolves to a wire rule,
	// but args already in OMP shape must pass through — reshaping them
	// would corrupt valid calls (e.g. read {path} into {path:""}). A
	// wire-name emission always carries wire-schema args (the model fills
	// what it was shown). So an OMP-name call reshapes only when it
	// carries CLI-distinctive keys.
	if resolved != wireName && !hasAnyKey(in, reshapeCliKeys[resolved]) {
		return "", false
	}
	out := rule(in)
	if out == nil {
		return "", false
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", false
	}
	return string(b), true
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

// ReshapeMessageCalls rewrites CLI-shaped arguments to OMP shape in place
// for non-streaming message.tool_calls entries, keyed by the WIRE name
// (call before FromUpstreamChunk restores names). Returns whether anything
// changed. Entries without a rule keep byte-identical args.
func (m ToolMapper) ReshapeMessageCalls(tcs []any) bool {
	if !m.floorOnly {
		return false
	}
	changed := false
	for _, raw := range tcs {
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
		if out, ok := m.ReshapeArgsFor(wire, args); ok {
			fn["arguments"] = out
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
	// OMP read {path} — singular; the first path wins. No paths key
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
	// OMP edit {path, old_string, new_string} — flat pair; first
	// replacement wins (multi-replacement turns need one call each). No
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
	// OMP glob {path, ...} is path-listing, not pattern search: the CLI
	// pattern rides as path (best-effort; magic chars pass through to the
	// OMP glob implementation).
	"glob": func(in map[string]any) map[string]any {
		out := map[string]any{}
		if p := strField(in, "pattern"); p != "" {
			out["path"] = p
		}
		return out
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
