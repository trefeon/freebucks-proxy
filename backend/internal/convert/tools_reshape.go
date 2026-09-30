package convert

import (
	"encoding/json"
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
	rule, ok := reshapeRules[wireName]
	if !ok {
		return "", false
	}
	var in map[string]any
	dec := json.NewDecoder(strings.NewReader(args))
	dec.UseNumber()
	if err := dec.Decode(&in); err != nil || in == nil {
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
	_, ok := reshapeRules[wireName]
	return ok
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
	// OMP read {path} — singular; the first path wins.
	"read_files": func(in map[string]any) map[string]any {
		paths, _ := in["paths"].([]any)
		first := ""
		if len(paths) > 0 {
			switch p := paths[0].(type) {
			case string:
				first = p
			case map[string]any:
				first = strField(p, "path")
			}
		}
		return map[string]any{"path": first}
	},
	// OMP edit {path, old_string, new_string} — flat pair; first
	// replacement wins (multi-replacement turns need one call each).
	"str_replace": func(in map[string]any) map[string]any {
		out := map[string]any{"path": strField(in, "path")}
		if reps, _ := in["replacements"].([]any); len(reps) > 0 {
			if r, ok := reps[0].(map[string]any); ok {
				out["old_string"] = strField(r, "oldString")
				out["new_string"] = strField(r, "newString")
				return out
			}
		}
		out["old_string"] = ""
		out["new_string"] = ""
		return out
	},
	// OMP write {path, content} — CLI instructions dropped.
	"write_file": func(in map[string]any) map[string]any {
		return map[string]any{
			"path":    strField(in, "path"),
			"content": strField(in, "content"),
		}
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
	// the open titles (lossy: per-item done states collapse — the loop
	// stays functional, planning display refreshes). All-done dumps read
	// back as a view (init rejects empty items).
	"write_todos": func(in map[string]any) map[string]any {
		todos, _ := in["todos"].([]any)
		var open []string
		for _, t := range todos {
			m, ok := t.(map[string]any)
			if !ok {
				continue
			}
			done, _ := m["completed"].(bool)
			if done {
				continue
			}
			title := strField(m, "task")
			if title == "" {
				title = strField(m, "content")
			}
			if title != "" {
				open = append(open, title)
			}
		}
		if len(open) == 0 {
			return map[string]any{"op": "view"}
		}
		items := make([]any, 0, len(open))
		for _, t := range open {
			items = append(items, t)
		}
		return map[string]any{"op": "init", "items": items}
	},
	// OMP web_search {query, ...} — CLI depth dropped.
	"web_search": func(in map[string]any) map[string]any {
		return map[string]any{"query": strField(in, "query")}
	},
}
