package server

// relayStream's per-chunk rewrite gauntlet, restructured as ONE
// parse-mutate-marshal pipeline (issue #249): a chunk is unmarshalled once,
// chained through map-level rewrites in fixed order, and marshalled once at
// the end. Chunks the pipeline leaves untouched keep their exact bytes
// (the byte-preserving fast path). All per-stream state lives in one
// rewriter struct so a fourth relay cannot copy a half of it.

import (
	"bytes"
	"encoding/json"
	"freebuff-proxy/backend/internal/convert"
)

// chunkRewriter owns the per-stream chunk state and the ordered rewrite
// pipeline shared by relayStream's chunk loop.
type chunkRewriter struct {
	stats *relayStats

	// XML tool-call extraction state.
	xmlExtractor *convert.XMLToolCallExtractor
	xmlCallIndex int
	xmlCallsSeen bool

	// end_turn pseudo-tool state.
	endTurnCallIndexes map[int]bool
	seenRealToolCalls  bool

	// Stream identity / capture state (also consumed at terminal time).
	streamModel      string
	xmlStreamID      string
	roleSent         bool
	lastFinishReason string

	toolIDsMap      map[string]bool
	toolIDs         []string
	streamToolCalls map[int]*streamToolAcc
	reasoningParts  []string
	contentParts    []string

	// Floor-only (OMP) arg-reshape streaming state: per-index withheld
	// CLI-shaped argument fragments, flushed reshaped on the terminal
	// chunk. Nil-map safe: only allocated when the first ruled fragment
	// arrives.
	reshapeBuf map[int]*reshapeAcc
}

// reshapeAcc buffers one tool call's withheld argument fragments.
type reshapeAcc struct {
	id   string
	wire string
	args bytes.Buffer
}

func newChunkRewriter(stats *relayStats) *chunkRewriter {
	return &chunkRewriter{
		stats:              stats,
		xmlExtractor:       &convert.XMLToolCallExtractor{},
		endTurnCallIndexes: make(map[int]bool),
		streamModel:        stats.servedModel,
		toolIDsMap:         make(map[string]bool),
		streamToolCalls:    make(map[int]*streamToolAcc),
	}
}

// rewrite runs the ordered pipeline over one sanitized chunk. The returned
// bytes are the original when the pipeline mutated nothing (or the chunk is
// not a JSON object) so untouched frames keep their exact bytes.
func (cr *chunkRewriter) rewrite(clean []byte) []byte {
	// Cheap coarse probe: none of the stages can act on a chunk lacking
	// every relevant token; keep the exact bytes without unmarshalling.
	if !bytes.Contains(clean, []byte(`"choices"`)) &&
		!bytes.Contains(clean, []byte(`"model":`)) &&
		!bytes.Contains(clean, []byte(`"id":`)) {
		return clean
	}
	var chunk map[string]any
	if json.Unmarshal(clean, &chunk) != nil {
		return clean
	}
	mutated := false

	// 1-3. The end_turn pipeline core (issue #246): track tool-call indexes
	// BEFORE any strip (StripEndTurnToolCalls deletes end_turn entries, so
	// tracking after the strip would never see the name — the recorded
	// indexes feed the continuation-fragment drop below, and any real named
	// call flips seenRealToolCalls so the terminal finish_reason rewrite
	// never downgrades a genuine tool-call turn), strip Codebuff's
	// end_turn pseudo-tool-calls (issue #140: injected into every upstream
	// request to pass foreign_toolset validation, never relayed to clients
	// that did not declare it), and drop continuation fragments for the
	// stripped indexes. StripEndTurnToolCalls itself flips finish_reason
	// tool_calls -> stop when nothing remains; the relay gates its own flip
	// on seenRealToolCalls below.
	_, _, _, endTurnMutated := processEndTurnCalls(chunk, cr.endTurnCallIndexes, &cr.seenRealToolCalls, false)
	mutated = endTurnMutated || mutated

	// 3. Extract XML-embedded tool calls from delta.content (streaming
	// parity with the accumulator's Finish): feed each content fragment
	// through the extractor, withhold text inside a candidate block, and
	// relay completed calls as native tool_calls fragments appended after
	// any native ones.
	mutatedFeed, appended := feedXMLToolCalls(cr.xmlExtractor, chunk, &cr.xmlCallIndex)
	mutated = mutatedFeed || mutated
	if appended {
		cr.xmlCallsSeen = true
	}

	// 4a. Floor-only (OMP) arg buffering: CLI-shaped argument fragments
	// cannot reshape incrementally, so withhold them per tool-call index
	// and inject the reshaped whole on the terminal chunk (stage 6a).
	// The client SDK concatenates fragments, so withheld + injected reads
	// exactly the OMP-shaped args. Gated on FloorOnly: other clients keep
	// byte-identical arg streaming.
	if cr.reshapeBuffer(chunk) {
		mutated = true
	}

	// 4. Restore client tool names (#140): the request renamed mapped
	// client tools to official signature names, so fragments carrying those
	// names must read the CLIENT's name on the wire.
	if cr.stats.toolMap.Len() > 0 && cr.stats.toolMap.FromUpstreamChunk(chunk) {
		mutated = true
	}

	// 5. Rewrite finish_reason for the terminal chunk when ALL tool calls in
	// this stream were end_turn. The terminal chunk carries no "end_turn"
	// string (only finish_reason: "tool_calls"), so it must gate on the
	// recorded indexes. Without this, finish_reason: "tool_calls" leaks to
	// clients that never declared end_turn.
	if !cr.seenRealToolCalls && len(cr.endTurnCallIndexes) > 0 {
		mutated = flipFinishReason(chunk, "tool_calls", "stop") || mutated
	}

	// 6. finish_reason parity for extracted XML calls: upstream models that
	// emit XML tool calls in content terminate with finish_reason: "stop"
	// (they never emit native tool_calls). Flip it so clients see a
	// complete tool-call turn. Runs after the end_turn rewrite above, so an
	// extracted call wins over the end_turn-only flip.
	if cr.xmlCallsSeen {
		mutated = flipFinishReason(chunk, "stop", "tool_calls") || mutated
	}
	// 6a. Floor-only (OMP) arg flush: on the terminal chunk, inject the
	// withheld reshaped args per buffered index (client name restored
	// inline). Runs before capture so capture sees what the client gets.
	if cr.reshapeFlush(chunk) {
		mutated = true
	}

	// 7. Capture-only stages (never mutate the chunk): version-neutral
	// reads for the identity, cache and ledger state.
	cr.capture(chunk)
	// 8. Stamp the served model and ensure the first chunk carries role.
	mutated = rewriteChatChunkModelInChunk(chunk, cr.stats.servedModel) || mutated
	mutated = ensureChatChunkRoleInChunk(chunk, &cr.roleSent) || mutated

	if !mutated {
		return clean
	}
	if b, err := json.Marshal(chunk); err == nil {
		return b
	}
	return clean
}

// flipFinishReason rewrites every choice's finish_reason from one value to
// another; returns whether anything changed.
func flipFinishReason(chunk map[string]any, from, to string) bool {
	changed := false
	if rawChoices, ok := chunk["choices"].([]any); ok {
		for _, raw := range rawChoices {
			choice, _ := raw.(map[string]any)
			if choice == nil {
				continue
			}
			if fr, ok := choice["finish_reason"].(string); ok && fr == from {
				choice["finish_reason"] = to
				changed = true
			}
		}
	}
	return changed
}

// reshapeBuffer withholds CLI-shaped argument fragments for ruled wire
// names (floor-only OMP requests): incremental fragments cannot reshape,
// so they buffer per tool-call index and relay without arguments; the
// terminal chunk carries the reshaped whole (reshapeFlush). Entries the
// client must still see (id on first sight, restored name via stage 4)
// keep flowing — only argument bytes are withheld.
func (cr *chunkRewriter) reshapeBuffer(chunk map[string]any) bool {
	if !cr.stats.toolMap.FloorOnly() {
		return false
	}
	changed := false
	rawChoices, ok := chunk["choices"].([]any)
	if !ok {
		return false
	}
	for _, raw := range rawChoices {
		choice, _ := raw.(map[string]any)
		if choice == nil {
			continue
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		tcs, _ := delta["tool_calls"].([]any)
		for _, rtc := range tcs {
			tc, _ := rtc.(map[string]any)
			if tc == nil {
				continue
			}
			fn, _ := tc["function"].(map[string]any)
			if fn == nil {
				continue
			}
			wire, _ := fn["name"].(string)
			frag, _ := fn["arguments"].(string)
			idx := 0
			if i, ok := tc["index"].(float64); ok {
				idx = int(i)
			}
			if wire == "" {
				// Continuation fragments carry no name: the first
				// fragment of this index named it (pre-restore, so the
				// recorded name is the wire name — unlike the capture
				// table, which only ever sees restored client names).
				if acc := cr.reshapeBuf[idx]; acc != nil {
					wire = acc.wire
				}
			}
			if wire == "" || frag == "" || !cr.stats.toolMap.HasReshapeRule(wire) {
				continue
			}
			if cr.reshapeBuf == nil {
				cr.reshapeBuf = make(map[int]*reshapeAcc)
			}
			acc := cr.reshapeBuf[idx]
			if acc == nil {
				acc = &reshapeAcc{wire: wire}
				cr.reshapeBuf[idx] = acc
			}
			if id, _ := tc["id"].(string); id != "" && acc.id == "" {
				acc.id = id
			}
			acc.args.WriteString(frag)
			delete(fn, "arguments")
			changed = true
		}
	}
	return changed
}

// reshapeFlush injects one delta.tool_calls entry per buffered index onto
// the terminal chunk (any non-empty finish_reason): the reshaped whole
// args under the restored client name. The client SDK concatenates
// fragments, so withheld empties + this whole assemble exactly the
// OMP-shaped call. Unreshapable buffers (model sent invalid JSON) flush
// verbatim so no call is ever swallowed.
func (cr *chunkRewriter) reshapeFlush(chunk map[string]any) bool {
	if len(cr.reshapeBuf) == 0 {
		return false
	}
	rawChoices, ok := chunk["choices"].([]any)
	if !ok || len(rawChoices) == 0 {
		return false
	}
	terminal := false
	for _, raw := range rawChoices {
		if choice, _ := raw.(map[string]any); choice != nil {
			if fr, _ := choice["finish_reason"].(string); fr != "" {
				terminal = true
				break
			}
		}
	}
	if !terminal {
		return false
	}
	choice, _ := rawChoices[0].(map[string]any)
	if choice == nil {
		return false
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		delta = map[string]any{}
		choice["delta"] = delta
	}
	tcs, _ := delta["tool_calls"].([]any)
	for idx, acc := range cr.reshapeBuf {
		args := acc.args.String()
		if out, ok := cr.stats.toolMap.ReshapeArgsFor(acc.wire, args); ok {
			args = out
		}
		entry := map[string]any{
			"index": idx,
			"function": map[string]any{
				"name":      cr.stats.toolMap.RestoreName(acc.wire),
				"arguments": args,
			},
		}
		if acc.id != "" {
			entry["id"] = acc.id
		}
		tcs = append(tcs, entry)
	}
	delta["tool_calls"] = tcs
	clear(cr.reshapeBuf)
	return true
}

// capture reads stream identity, reasoning/content parts, usage and
// tool-call identity out of one chunk. It never mutates.
func (cr *chunkRewriter) capture(chunk map[string]any) {
	// Usage: the final chunk carries the usage block (or a usage-only chunk
	// with stream_options.include_usage); capture its total for the spend
	// ledger (#122) plus the split for the usage log. Only adopt a real
	// usage block: "usage":null or a chunk merely mentioning the key must
	// not zero the ledger.
	if u, ok := chunk["usage"]; ok && u != nil {
		cr.stats.setUsage(u)
	}
	if chunkModel, _ := chunk["model"].(string); chunkModel != "" && cr.streamModel == "" {
		cr.streamModel = chunkModel
	}
	if chunkID, _ := chunk["id"].(string); chunkID != "" {
		cr.xmlStreamID = chunkID
	}
	// Record the finish_reason the client actually sees (after the two flips
	// above) for the XML-flush terminal repair.
	cr.captureFinishReason(chunk)
	cr.captureDelta(chunk)
}

func (cr *chunkRewriter) captureFinishReason(chunk map[string]any) {
	if rawChoices, ok := chunk["choices"].([]any); ok {
		for _, raw := range rawChoices {
			if choice, ok := raw.(map[string]any); ok {
				if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
					cr.lastFinishReason = fr
				}
			}
		}
	}
}

func (cr *chunkRewriter) captureDelta(chunk map[string]any) {
	// Only choices[0], mirroring the structured capture the relay used
	// before the pipeline: chat streams carry one choice.
	rawChoices, _ := chunk["choices"].([]any)
	if len(rawChoices) == 0 {
		return
	}
	choice, _ := rawChoices[0].(map[string]any)
	if choice == nil {
		return
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		return
	}
	if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
		cr.reasoningParts = append(cr.reasoningParts, rc)
	} else if rc, ok := delta["reasoning"].(string); ok && rc != "" {
		cr.reasoningParts = append(cr.reasoningParts, rc)
	} else if rc, ok := delta["thinking"].(string); ok && rc != "" {
		cr.reasoningParts = append(cr.reasoningParts, rc)
	}
	if c, ok := delta["content"].(string); ok && c != "" {
		cr.contentParts = append(cr.contentParts, c)
	}
	if tcs, ok := delta["tool_calls"].([]any); ok {
		for _, raw := range tcs {
			tc, _ := raw.(map[string]any)
			if tc == nil {
				continue
			}
			id, _ := tc["id"].(string)
			if id != "" && !cr.toolIDsMap[id] {
				cr.toolIDsMap[id] = true
				cr.toolIDs = append(cr.toolIDs, id)
			}
			idx := 0
			if i, ok := tc["index"].(float64); ok {
				idx = int(i)
			}
			acc := cr.streamToolCalls[idx]
			if acc == nil {
				acc = &streamToolAcc{}
				cr.streamToolCalls[idx] = acc
			}
			if id != "" && acc.id == "" {
				acc.id = id
			}
			fn, _ := tc["function"].(map[string]any)
			if name, _ := fn["name"].(string); name != "" && acc.name == "" {
				acc.name = name
			}
			if args, _ := fn["arguments"].(string); args != "" {
				acc.args.WriteString(args)
			}
		}
	}
}

// rewriteChatChunkModelInChunk is the map-level core of
// rewriteChatChunkModel.
func rewriteChatChunkModelInChunk(chunk map[string]any, served string) bool {
	if served == "" {
		return false
	}
	if cur, _ := chunk["model"].(string); cur == served {
		return false
	}
	if _, has := chunk["model"]; !has {
		return false // never inject into arbitrary frames
	}
	chunk["model"] = served
	return true
}

// ensureChatChunkRoleInChunk is the map-level core of ensureChatChunkRole:
// inject "role":"assistant" into the first relayed chunk's delta (choice 0)
// when the upstream omitted it. Idempotent per stream via roleSent.
func ensureChatChunkRoleInChunk(chunk map[string]any, roleSent *bool) bool {
	if *roleSent {
		return false
	}
	rawChoices, _ := chunk["choices"].([]any)
	if len(rawChoices) == 0 {
		return false
	}
	choice, _ := rawChoices[0].(map[string]any)
	if choice == nil {
		return false
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		return false
	}
	if _, has := delta["role"]; !has {
		delta["role"] = "assistant"
		*roleSent = true
		return true
	}
	*roleSent = true
	return false
}
