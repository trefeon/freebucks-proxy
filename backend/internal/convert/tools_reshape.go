package convert

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Response-leg argument reshape for client families (tools_floor.go).
//
// The model fills CLI-shaped args per the canonical definitions it was shown
// (OMP: the floor-only wire; pi: the canonical defs every pi core tool maps
// onto); the client dispatcher validates per its own schemas, so CLI-shaped
// args must be reshaped to the client's shape alongside the name restore.
// Keyed by WIRE name and gated on the client family: no other client's relay
// ever reaches these paths.
//
// OMP rules emit the OMP vocabulary (edit {path, old_string, new_string},
// todo op-machine, …); pi rules emit pi's core shapes (read {path[, offset,
// limit]}, edit {path, edits:[{oldText,newText}]}) and only for the wire
// names pi's core toolset maps onto (piReshapeWires) — a pi extension tool
// that happens to be named like a canonical wire name keeps its own args.
//
// Two wire shapes fan out to N calls, order preserved: read_files
// {paths:[...]} becomes one read {path} per entry (OMP and pi alike), and
// str_replace {path, replacements:[...]} becomes one edit per replacement in
// the OMP vocabulary while pi's batch-native edit takes all replacements in
// ONE {path, edits:[{oldText,newText},…]} call. A single path / single
// replacement stays exactly 1:1. Every other rule yields exactly one call.
//
// All rules are total and lossy-documented: unknown shapes pass through
// verbatim rather than failing the turn. Kept scalar drops (grep flags,
// bash background extras, write instructions, web_search depth, read
// offset/limit — OMP only, pi reads keep them —, edit allowMultiple, glob
// cwd/max_results) are documented at each rule; a model-emitted `find`
// name reshapes to the live OMP find {query, grep_keywords} shape (the
// find rule). Whole tools with no CLI equivalent (eval, task, wait, learn,
// manage_skill, new_context, context_notes) never reach the wire at all —
// floorOnlyOMP drops them so the model is never shown them.

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
	if m.family == familyNone {
		return nil, false
	}
	resolved := resolveReshapeRule(wireName)
	if _, ok := reshapeRules[resolved]; !ok {
		return nil, false
	}
	// pi reshapes only the wire names its core toolset maps onto: an
	// extension tool declared under a canonical name keeps its own args.
	if m.family == familyPi && !piReshapeWires[resolved] {
		return nil, false
	}
	var in map[string]any
	dec := json.NewDecoder(strings.NewReader(args))
	dec.UseNumber()
	if err := dec.Decode(&in); err != nil || in == nil {
		return nil, false
	}
	// Concatenated tool-call JSON (live: one arguments string carrying two
	// complete objects back to back): the Decode above takes the first value
	// and silently drops the rest, so every operation after the first never
	// runs. Split into parts and reshape each through the same wire rule
	// below, so one glued call becomes N valid calls. Parts without a rule
	// shape ride verbatim — never dropped. Trailing bytes that are not
	// complete values keep the legacy path (reshape the first value).
	if rest := strings.TrimSpace(args[int(dec.InputOffset()):]); rest != "" {
		if parts, ok := SplitConcatenatedJSONValues(args); ok {
			bodies := make([]string, 0, len(parts))
			for _, part := range parts {
				if sub, ok := m.ReshapeArgsFanout(wireName, part); ok && len(sub) > 0 {
					bodies = append(bodies, sub...)
					continue
				}
				bodies = append(bodies, part)
			}
			if len(bodies) >= 2 {
				return bodies, true
			}
		}
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

// piReshapeWires are the wire names pi's core toolset maps onto
// (read→read_files, edit→str_replace, write→write_file, bash/powershell→
// run_terminal_command, grep→code_search, find→glob, ls→list_directory).
// Only these get pi-shaped args back; extension tools under other canonical
// names are left alone.
var piReshapeWires = map[string]bool{
	"read_files":           true,
	"str_replace":          true,
	"write_file":           true,
	"run_terminal_command": true,
	"code_search":          true,
	"glob":                 true,
	"list_directory":       true,
}

// fanoutArgs maps one parsed CLI-shaped arguments object to one shaped map
// per resulting client call. Nil/empty means passthrough (never a drop).
func (m ToolMapper) fanoutArgs(resolved string, in map[string]any) []map[string]any {
	if m.family == familyPi {
		switch resolved {
		case "read_files":
			return piReadArgs(in)
		case "str_replace":
			return piEditArgs(in)
		}
	}
	switch resolved {
	case "read_files":
		// OMP read {path} is singular: one call per entry, order
		// preserved. Object entries {path, offset, limit} keep the path;
		// offset/limit have no OMP equivalent and are dropped. Blank
		// paths (empty or whitespace-only, either form) are skipped: the
		// live read schema requires path, so emitting {path:""} would
		// fail dispatch. No paths key — or nothing but blanks — means
		// already OMP shape or nothing usable: nil (passthrough), never
		// blanked, never dropped.
		paths, ok := in["paths"].([]any)
		if !ok {
			return nil
		}
		var outs []map[string]any
		for _, p := range paths {
			switch v := p.(type) {
			case string:
				if strings.TrimSpace(v) == "" {
					continue
				}
				outs = append(outs, map[string]any{"path": v})
			case map[string]any:
				if strings.TrimSpace(strField(v, "path")) == "" {
					continue
				}
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
	case "write_todos":
		// OMP init carries string-only items and has no per-item status, so
		// completion rides as one trailing `done {task, phase?}` op per
		// completed entry. Order: init first, then the done ops.
		titles, done, ok := todoItems(in)
		if !ok {
			return nil // already OMP-shaped emission: passthrough
		}
		if len(titles) == 0 {
			return []map[string]any{{"op": "view"}}
		}
		items := make([]any, 0, len(titles))
		for _, title := range titles {
			items = append(items, title)
		}
		outs := []map[string]any{{
			"op":   "init",
			"list": []any{map[string]any{"phase": "Tasks", "items": items}},
		}}
		for _, title := range done {
			outs = append(outs, map[string]any{"op": "done", "task": title})
		}
		return outs
	default:
		if rule, ok := reshapeRules[resolved]; ok {
			if out := rule(in); out != nil {
				return []map[string]any{out}
			}
		}
		return nil
	}
}

// piReadArgs maps CLI read_files {paths:[...]} to pi's read calls: one call	// per entry, order preserved. pi's read is singular ({path}) but natively
// supports offset/limit, so object entries keep them — the OMP rule drops
// them because OMP's read has no equivalent. String entries pass the
// value through verbatim. No paths key means already pi shape: nil
// (passthrough), never blanked.
func piReadArgs(in map[string]any) []map[string]any {
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
			out := map[string]any{"path": strField(v, "path")}
			if n, ok := numField(v, "offset"); ok {
				out["offset"] = n
			}
			if n, ok := numField(v, "limit"); ok {
				out["limit"] = n
			}
			outs = append(outs, out)
		}
	}
	return outs
}

// piEditArgs maps CLI str_replace {path, replacements:[{oldString,
// newString}]} to pi's batch-native edit: ONE call {path, edits:[{oldText,
// newText},…]}, order preserved (OMP fans out one edit per replacement
// because OMP's edit is single-replacement per call). allowMultiple and any
// extra replacement keys have no pi equivalent and are dropped. No
// replacements key means already pi shape — native {path, edits[…]} or the
// legacy flat {path, oldText, newText} pi normalizes itself: nil
// (passthrough), never a blanked-out edits list.
func piEditArgs(in map[string]any) []map[string]any {
	reps, ok := in["replacements"].([]any)
	if !ok {
		return nil
	}
	edits := make([]any, 0, len(reps))
	for _, r := range reps {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		edits = append(edits, map[string]any{
			"oldText": strField(rm, "oldString"),
			"newText": strField(rm, "newString"),
		})
	}
	if len(edits) == 0 {
		return nil // nothing usable to reshape: keep the wire args for the error
	}
	return []map[string]any{{"path": strField(in, "path"), "edits": edits}}
}

// fanoutGlobArgs reshapes CLI glob {pattern, ...} to the OMP shape dictated
// by the request origin: on OMP-family mappers, a wire glob claimed by the
// client's `find` tool restores as the live OMP find {query,
// grep_keywords[, path]} (18.5.0, strict — query and grep_keywords are both
// required), a native glob as OMP glob {path}. Other families keep their own
// restore (pi dispatches find from its own vocabulary, so the ex-find branch
// never runs for pi). The query source is the first present of
// pattern/query: the live OMP prompt emits find with query+grep_keywords
// vocabulary while the wire never shows a find def, so either key may
// arrive; a wire carrying grep_keywords already keeps it, otherwise it
// defaults to the query itself. cwd scopes via find path, mirroring the
// grep rule; max_results has no find equivalent and is dropped. No query
// source at all means already OMP shape (native {path} emission): nil
// (passthrough), never a blanked-out path.
func (m ToolMapper) fanoutGlobArgs(in map[string]any) []map[string]any {
	src := strField(in, "pattern")
	if src == "" {
		src = strField(in, "query")
	}
	if src == "" {
		return nil
	}
	if m.family == familyOMP && m.upstreamToClient["glob"] == "find" {
		return []map[string]any{findArgs(in, src)}
	}
	return []map[string]any{{"path": src}}
}

// findArgs builds the live OMP find shape {query, grep_keywords[, path]}
// from a CLI glob arguments object and the resolved query text. Both query
// and grep_keywords are strict-required, so keywords default to the query
// itself when the wire carries none; the CLI cwd scopes via path.
func findArgs(in map[string]any, query string) map[string]any {
	out := map[string]any{"query": query}
	if kw, ok := in["grep_keywords"].([]any); ok && len(kw) > 0 {
		out["grep_keywords"] = kw
	} else {
		out["grep_keywords"] = []any{query}
	}
	if cwd := strField(in, "cwd"); cwd != "" {
		out["path"] = cwd
	}
	return out
}

// gravityQuery synthesizes the OMP web_search query from a gravity_index
// shape: the search query, a browse keyword/category, or a service slug.
// Empty means the shape carries nothing searchable — the response leg must
// not ride it as a query-less web_search (query is strict-required), so the
// OMP empty-gravity text guard renders it as assistant text instead.
func gravityQuery(in map[string]any) string {
	if query := strField(in, "query"); query != "" {
		return query
	}
	if query := strField(in, "q"); query != "" {
		return query
	}
	if cat := strField(in, "category"); cat != "" {
		return cat + " developer services"
	}
	if slug := strField(in, "slug"); slug != "" {
		return slug + " developer documentation"
	}
	return ""
}

// HasReshapeRule reports whether wireName has a reshape rule on this
// mapper (floor-only OMP requests only).
func (m ToolMapper) HasReshapeRule(wireName string) bool {
	resolved := resolveReshapeRule(wireName)
	if _, ok := reshapeRules[resolved]; !ok {
		return false
	}
	switch m.family {
	case familyNone:
		return false
	case familyPi:
		return piReshapeWires[resolved]
	}
	return true
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
	// Live 2026-10-02 (deepseek via real OMP): the model emits `mcp__task`
	// for the harness-only delegation tool — its context is saturated with
	// mcp__* names (MCP catalog in the system prompt), so it namespaces a
	// tool it knows only from prose by that convention. The name restores
	// to `task` (request-leg reverse entry), so the task rule must key on
	// it here, pre-restore; without this the stringified-tasks emission
	// streams through verbatim and the harness rejects it. pi has no task
	// rule (piReshapeWires), so pi mcp__ emissions stay untouched.
	if name == "mcp__task" {
		return "task"
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
	"gravity_index":        {"action", "search_id", "category", "slug"},
	// `task` needs no guard on its own name (resolved == wireName skips
	// it; the rule is total and self-guarding), but the namespaced
	// `mcp__task` emission resolves across names and must not be blocked:
	// any task-ish key lets the rule run, and the rule itself passes
	// valid batches and unusable shapes through untouched.
	"task": {"tasks", "task", "agent", "name"},
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
// throughout. Glued arguments (two complete JSON values back to back) split
// into one entry per value first, each then reshaped. Returns the (possibly
// longer) call list and whether anything changed; entries without a rule keep
// byte-identical args.
func (m ToolMapper) ReshapeMessageCalls(tcs []any) ([]any, bool) {
	if m.family == familyNone {
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
		// Concatenated tool-call JSON (live: one arguments string carrying
		// two complete objects back to back): split first so one glued call
		// becomes N valid calls, then run each part through the family
		// reshape below — a split part without a rule shape rides verbatim,
		// never dropped. Order preserved throughout.
		parts := []string{args}
		if split, ok := SplitConcatenatedJSONValues(args); ok {
			parts = split
		}
		bodies := make([]string, 0, len(parts))
		reshaped := false
		for _, part := range parts {
			if sub, ok := m.ReshapeArgsFanout(wire, part); ok && len(sub) > 0 {
				bodies = append(bodies, sub...)
				reshaped = true
				continue
			}
			bodies = append(bodies, part)
		}
		if len(parts) == 1 && !reshaped {
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
	if m.family == familyNone {
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

// taskCopyArgs shallow-copies an args object so batch normalization can add
// tasks[] without mutating the caller's map.
func taskCopyArgs(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
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

// writeFileContent preserves the write payload losslessly. OMP dispatches
// content as a string — file text, or the JSON args object when the write
// targets an xd:// device path — but the model may fill it as a nested
// value. A blind string cast would blank it and strand the call (the device
// receives empty args and never executes), so non-string values are
// marshaled to their JSON text. Missing content stays empty.
func writeFileContent(in map[string]any) string {
	c, ok := in["content"]
	if !ok || c == nil {
		return ""
	}
	if s, ok := c.(string); ok {
		return s
	}
	if b, err := json.Marshal(c); err == nil {
		return string(b)
	}
	return ""
}

// todoItems extracts a CLI `write_todos` dump as (titles, completedTitles),
// order preserved. ok=false means the call carried no `todos` key at all — an
// already-OMP-shaped emission the caller must pass through untouched.
func todoItems(in map[string]any) (titles, done []string, ok bool) {
	raw, present := in["todos"]
	if !present {
		return nil, nil, false
	}
	list, _ := raw.([]any)
	titles = make([]string, 0, len(list))
	for _, t := range list {
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
		titles = append(titles, title)
		if d, _ := m["completed"].(bool); d {
			done = append(done, title)
		}
	}
	return titles, done, true
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
	// OMP write {path, content} — CLI instructions dropped. content rides
	// through writeFileContent, not a blind string cast: the model fills a
	// nested value when the write targets an xd:// device (content is the
	// device JSON args), and blanking it would strand the call.
	"write_file": func(in map[string]any) map[string]any {
		return map[string]any{
			"path":    strField(in, "path"),
			"content": writeFileContent(in),
		}
	},
	// OMP read {path} — singular per call; the fan-out core (fanoutArgs)
	// expands one call per path, this single is the first. No paths key —
	// or a blank first path — means already OMP shape or nothing usable:
	// nil (passthrough) rather than a blanked-out path.
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
		if strings.TrimSpace(first) == "" {
			return nil
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
	// runs through fanoutGlobArgs, which routes ex-find origins to the live
	// OMP find {query, grep_keywords} shape instead. No pattern/query
	// source means already OMP shape (native {path} emission): nil
	// (passthrough), never a blanked-out path.
	"glob": func(in map[string]any) map[string]any {
		if p := strField(in, "pattern"); p != "" {
			return map[string]any{"path": p}
		}
		return nil
	},
	// OMP find {query, grep_keywords[, path]} (live 18.5.0, strict — both
	// query and grep_keywords are required): a model-emitted `find` call
	// carrying CLI pattern vocabulary reshapes to the registered shape; an
	// already-valid emission (query plus non-empty grep_keywords) passes
	// through untouched, and a shape with neither vocabulary passes through
	// for the harness to reject exactly as today. Keywords default to the
	// query itself when the emission carries none; cwd scopes via path like
	// the grep rule. pi has no find rule (piReshapeWires): pi dispatches
	// its own find {pattern, path?} vocabulary, so pi emissions stay
	// untouched.
	"find": func(in map[string]any) map[string]any {
		if q := strField(in, "query"); q != "" {
			if kw, ok := in["grep_keywords"].([]any); ok && len(kw) > 0 {
				return nil
			}
			return findArgs(in, q)
		}
		if p := strField(in, "pattern"); p != "" {
			return findArgs(in, p)
		}
		return nil
	},
	// OMP todo is a 9-op machine (TodoOperation, tools/todo.ts:23; schema
	// :69-81), CLI write_todos a state dump. OMP's init
	// list carries string-only items (InitListEntry.items: string[]), so a
	// per-item {task, completed} object is rejected by the dispatcher; this
	// single entry emits the init half and fanoutArgs appends one `done` op
	// per completed task. Empty dump reads back as a view. Known collapse:
	// CLI in-progress/blocked/pending distinctions do not survive — OMP
	// infers in-progress as the first pending item, so a resumed checklist
	// may restart progress display from the top. No proxy fix exists short
	// of inventing ops the 9-op machine does not define.
	"write_todos": func(in map[string]any) map[string]any {
		titles, _, ok := todoItems(in)
		if !ok {
			return nil // already OMP-shaped emission
		}
		if len(titles) == 0 {
			return map[string]any{"op": "view"}
		}
		items := make([]any, 0, len(titles))
		for _, title := range titles {
			items = append(items, title)
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
	// OMP task (delegation) batch normalization. The harness validates
	// `tasks` (non-empty array of {name?, agent?, task}), but a model that
	// never saw the tool definition — floor-only drops it from the wire so
	// the gate never sees a foreign def — emits the singular vocabulary
	// form {agent, task, context} or a bare {task}, which the harness
	// rejects with "Missing `tasks`" (deterministic: retries fail
	// identically). A valid non-empty tasks[] passes through (nil);
	// otherwise every original key is preserved and tasks[] is synthesized
	// around the singular keys — never invented from nothing (nil =
	// passthrough, the harness errors exactly as today). Synthesized agent
	// names still face the harness spawn-policy validation
	// (task/index.ts:226-266; discovery/helpers.ts:289-292): an unknown
	// agent type fails loudly in-harness, which is the honest signal (the
	// proxy cannot know the client's agent catalog). Wire-safe: this
	// runs on the response leg only, the 16+end_turn wire is untouched.
	"task": func(in map[string]any) map[string]any {
		if arr, ok := in["tasks"].([]any); ok && len(arr) > 0 {
			return nil
		}
		if s, ok := in["tasks"].(string); ok && s != "" {
			out := taskCopyArgs(in)
			var arr []any
			if err := json.Unmarshal([]byte(s), &arr); err == nil && len(arr) > 0 {
				out["tasks"] = arr
				return out
			}
			out["tasks"] = []any{map[string]any{"task": s}}
			return out
		}
		item := make(map[string]any, 3)
		for _, k := range []string{"name", "agent", "task"} {
			if s := strField(in, k); s != "" {
				item[k] = s
			}
		}
		if len(item) == 0 {
			return nil
		}
		out := taskCopyArgs(in)
		out["tasks"] = []any{item}
		return out
	},
	// gravity_index -> OMP web_search {query}: searches developer services
	// directory; query synthesizes from search query, browse keyword/category,
	// or service slug so the model gets live service info via web search. A
	// shape with no query source at all yields nil (passthrough) — the
	// OMP empty-gravity text guard (TextFallback) catches those before they
	// can ride as a query-less web_search, which strict rejects.
	"gravity_index": func(in map[string]any) map[string]any {
		if query := gravityQuery(in); query != "" {
			return map[string]any{"query": query}
		}
		return nil
	},
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
			// options is required per question by the live OMP ask schema
			// (strict): a question carrying no usable options emits an empty
			// array rather than omitting the key, so the call stays
			// dispatchable — an optionless question is answered free-text
			// (OTHER_OPTION), never dropped.
			opts, _ := qm["options"].([]any)
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
