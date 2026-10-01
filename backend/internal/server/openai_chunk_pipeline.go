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
	"sort"
	"time"
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
	// reshapeMaxIdx is the highest upstream tool-call index observed in any
	// streamed delta.tool_calls entry (ruled or not). Fan-out extras take
	// fresh indexes above it, so they can never collide with a live call —
	// upstream indexes are dense per turn, and every buffered index is one
	// of them. Recorded before the reshape-rule gate, so unruled middle
	// indexes count too.
	reshapeMaxIdx int

	// Unroutable floor emissions (text fallback): tool-call indexes naming a
	// floor tool OMP cannot dispatch, stripped from the stream like end_turn
	// and rendered into delta.content on the terminal chunk. Continuation
	// fragments carry no name, so their wire name is remembered per index.
	textFallbackIndexes map[int]bool
	textFallbackWires   map[int]string
	textFallbackArgs    map[int]*bytes.Buffer
	// nonFallbackCallSeen records any named, dispatchable tool call this
	// stream relayed. A turn whose only calls were unroutable emissions must
	// end as a finished text turn, not a tool_calls turn.
	nonFallbackCallSeen bool
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

// rewrite runs the ordered pipeline over one sanitized chunk, whose decoded
// map the caller already holds (convert.SanitizeChunkMapped) — the second
// json.Unmarshal the relay used to perform here is gone. The returned bytes
// are the original clean bytes when the pipeline mutated nothing (or the
// chunk decoded empty) so untouched frames keep their exact bytes.
func (cr *chunkRewriter) rewrite(clean []byte, chunk map[string]any) []byte {
	if len(chunk) == 0 {
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

	// 3a. Unroutable family emissions: strip tool_calls entries naming a
	// canonical tool the family cannot dispatch (and their nameless
	// continuation fragments, matched by recorded index) before any relay
	// stage sees them, buffering their argument fragments for the terminal
	// text render. Mirrors the end_turn strip: the client must never assemble
	// a call it cannot dispatch. Runs after the XML feed so an extracted call
	// is covered too.
	if cr.stripTextFallbackCalls(chunk) {
		mutated = true
	}

	// 4a. Family arg buffering: CLI-shaped argument fragments cannot reshape
	// incrementally, so withhold them per tool-call index and inject the
	// reshaped whole on the terminal chunk (stage 6a). The client SDK
	// concatenates fragments, so withheld + injected reads exactly the
	// family-shaped args. Gated on ResponseRewrite: other clients keep
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
	// 6a. Family arg flush: on the terminal chunk, inject the withheld
	// reshaped args per buffered index (client name restored inline). Runs
	// before capture so capture sees what the client gets.
	if cr.reshapeFlush(chunk) {
		mutated = true
	}
	// 6b. Unroutable floor emissions: render the stripped calls' payload as
	// assistant text on the terminal chunk and, when the stream delivered no
	// dispatchable call, flip finish_reason tool_calls->stop so the client
	// ends a text turn instead of waiting on calls it never received.
	if cr.flushTextFallbacks(chunk) {
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
	if !cr.stats.toolMap.ResponseRewrite() {
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
			if i, ok := tc["index"].(float64); ok && int(i) > cr.reshapeMaxIdx {
				cr.reshapeMaxIdx = int(i)
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
			if wire == "" || !cr.stats.toolMap.HasReshapeRule(wire) {
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
			// Name-only first fragments (no arguments yet) still record
			// the index ownership above, so later continuation fragments
			// resolve the rule; only argument bytes are withheld.
			if frag == "" {
				continue
			}
			acc.args.WriteString(frag)
			delete(fn, "arguments")
			changed = true
		}
	}
	return changed
}

// reshapeFlush injects delta.tool_calls entries for every buffered index
// onto the terminal chunk (any non-empty finish_reason): the reshaped whole
// args under the restored client name. Multi-path reads and
// multi-replacement edits fan out to one entry per call — the first keeps
// its index, extras take fresh indexes above every observed upstream index,
// so the client SDK assembles each call separately. The client SDK
// concatenates fragments, so withheld empties + these wholes assemble
// exactly the OMP-shaped calls. Unreshapable buffers (model sent invalid
// JSON) flush verbatim so no call is ever swallowed.
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
	idxs := make([]int, 0, len(cr.reshapeBuf))
	for idx := range cr.reshapeBuf {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	next := cr.reshapeMaxIdx + 1
	injected := false
	for _, idx := range idxs {
		acc := cr.reshapeBuf[idx]
		if acc.args.Len() == 0 {
			continue
		}
		bodies, ok := cr.stats.toolMap.ReshapeArgsFanout(acc.wire, acc.args.String())
		if !ok || len(bodies) == 0 {
			bodies = []string{acc.args.String()}
		}
		for k, args := range bodies {
			useIdx := idx
			if k > 0 {
				useIdx = next
				next++
			}
			entry := map[string]any{
				"index": useIdx,
				"function": map[string]any{
					"name":      cr.stats.toolMap.RestoreName(acc.wire),
					"arguments": args,
				},
			}
			tcs = append(tcs, entry)
			injected = true
		}
	}
	if !injected {
		return false
	}
	delta["tool_calls"] = tcs
	clear(cr.reshapeBuf)
	return true
}

// flushReshapeTerminal releases withheld reshaped tool arguments when the
// upstream stream ends WITHOUT ever carrying a finish_reason — a clean early
// close (openai_stream.go:151) or a mid-stream read error (openai_stream.go:139).
// reshapeBuffer withholds argument bytes for ruled wire names and re-injects
// them on the terminal chunk (reshapeFlush); a stream that never delivers a
// terminal chunk leaves the client assembling a call whose arguments were
// never sent, which surfaces as "The model did not complete a usable
// response" instead of the call running. The synthetic chunk settles the turn
// on tool_calls, shaped like emitXMLFlush's flush frame
// (openai_stream.go:76-82). complete reports whether every buffered entry's
// bytes are whole JSON: when the stream was cut mid-fragment the caller must
// still surface the transport error rather than pass a truncated turn off as
// a clean one.
func (cr *chunkRewriter) flushReshapeTerminal(streamID, model string) (map[string]any, bool, bool) {
	if len(cr.reshapeBuf) == 0 {
		return nil, false, true
	}
	complete := true
	for _, acc := range cr.reshapeBuf {
		if acc.args.Len() == 0 {
			continue
		}
		if !json.Valid(acc.args.Bytes()) {
			complete = false
			break
		}
	}
	if streamID == "" {
		streamID = "chatcmpl-flush"
	}
	chunk := map[string]any{
		"id":      streamID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "tool_calls",
		}},
	}
	if !cr.reshapeFlush(chunk) {
		return nil, false, true
	}
	return chunk, true, complete
}

// stripTextFallbackCalls removes delta.tool_calls entries naming an
// unroutable floor tool (and their nameless continuation fragments, matched
// by recorded index) from an unmarshalled chunk, buffering their argument
// fragments per index for the terminal render. Mirrors the end_turn strip:
// the model is shown the CLI floor, but OMP dispatches only its own names, so
// a call to a floor tool with no OMP equivalent must never be assembled by
// the client. Floor-only requests only; every other client keeps its calls
// byte-identical.
func (cr *chunkRewriter) stripTextFallbackCalls(chunk map[string]any) bool {
	if !cr.stats.toolMap.ResponseRewrite() {
		return false
	}
	rawChoices, ok := chunk["choices"].([]any)
	if !ok {
		return false
	}
	changed := false
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
		if len(tcs) == 0 {
			continue
		}
		kept := make([]any, 0, len(tcs))
		choiceChanged := false
		for _, rtc := range tcs {
			tc, _ := rtc.(map[string]any)
			if tc == nil {
				kept = append(kept, rtc)
				continue
			}
			idx := 0
			if f, ok := tc["index"].(float64); ok {
				idx = int(f)
			}
			fn, _ := tc["function"].(map[string]any)
			name, _ := fn["name"].(string)
			wire := name
			if wire == "" {
				wire = cr.textFallbackWires[idx]
			}
			if wire != "" && cr.stats.toolMap.HasTextFallback(wire) {
				if name != "" {
					if cr.textFallbackWires == nil {
						cr.textFallbackWires = make(map[int]string)
					}
					cr.textFallbackWires[idx] = name
				}
				if frag, _ := fn["arguments"].(string); frag != "" {
					if cr.textFallbackArgs == nil {
						cr.textFallbackArgs = make(map[int]*bytes.Buffer)
					}
					buf := cr.textFallbackArgs[idx]
					if buf == nil {
						buf = &bytes.Buffer{}
						cr.textFallbackArgs[idx] = buf
					}
					buf.WriteString(frag)
				}
				if cr.textFallbackIndexes == nil {
					cr.textFallbackIndexes = make(map[int]bool)
				}
				cr.textFallbackIndexes[idx] = true
				choiceChanged = true
				continue // drop the entry: name and arguments never relay
			}
			if name != "" {
				cr.nonFallbackCallSeen = true
			}
			kept = append(kept, rtc)
		}
		if !choiceChanged {
			continue
		}
		changed = true
		if len(kept) == 0 {
			delete(delta, "tool_calls")
		} else {
			delta["tool_calls"] = kept
		}
	}
	return changed
}

// flushTextFallbacks renders the stripped unroutable floor calls into
// delta.content on the terminal chunk: the payload the client would have
// shown (followup prompts, a UI link, a discovery request) becomes assistant
// text, and absorbed names contribute nothing. When the stream relayed no
// dispatchable call and no XML-extracted call, finish_reason flips
// tool_calls->stop — the turn delivered text, so the client must not wait on
// tool calls it never received.
func (cr *chunkRewriter) flushTextFallbacks(chunk map[string]any) bool {
	if len(cr.textFallbackIndexes) == 0 {
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
	idxs := make([]int, 0, len(cr.textFallbackIndexes))
	for idx := range cr.textFallbackIndexes {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	var text bytes.Buffer
	for _, idx := range idxs {
		args := ""
		if buf := cr.textFallbackArgs[idx]; buf != nil {
			args = buf.String()
		}
		rendered, kind := cr.stats.toolMap.TextFallback(cr.textFallbackWires[idx], args)
		if kind == convert.TextFallbackRender {
			text.WriteString(rendered)
		}
	}
	changed := false
	if text.Len() > 0 {
		if choice, _ := rawChoices[0].(map[string]any); choice != nil {
			delta, _ := choice["delta"].(map[string]any)
			if delta == nil {
				delta = map[string]any{}
				choice["delta"] = delta
			}
			cur, _ := delta["content"].(string)
			// contentParts is appended by capture (stage 7) from this same
			// delta — do not double-record here.
			delta["content"] = cur + text.String()
			changed = true
		}
	}
	if !cr.nonFallbackCallSeen && !cr.xmlCallsSeen {
		if flipFinishReason(chunk, "tool_calls", "stop") {
			changed = true
		}
	}
	clear(cr.textFallbackIndexes)
	cr.textFallbackWires = nil
	cr.textFallbackArgs = nil
	return changed
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
