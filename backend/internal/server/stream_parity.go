package server

import (
	"sort"
)

// Both translators used to relay delta bytes live with no withhold
// architecture, so family turns kept CLI-shaped args and undispatchable
// calls on these surfaces (the deferred gap). The chat relay instead
// withholds CLI-shaped fragments per tool-call index and injects reshaped
// wholes on the terminal chunk. This file ports that withhold→finalize
// discipline to the two delta translators, reusing the shared per-call
// core (convert.FinalizeStreamedCall, itself parity-tested against the
// non-streaming legs):
//
//   - while the upstream stream flows, fragments for a
//     ToolMapper.WithholdStreamArgs wire name accumulate silently (id and
//     wire recorded, no delta emitted, no stop-reason claimed);
//   - on the terminal finish_reason chunk and at upstream EOF the buffered
//     calls finalize: reshape+fan-out emits complete blocks/items, an
//     unroutable call renders as assistant text, and an absorbed call
//     vanishes (leaving the turn's stop contract to the demote logic the
//     translators already carry).
//
// Gating is per call, not per turn: unruled calls on the same rewriting
// turn (extensions, verbatim names, end_turn/decide pins) keep streaming
// live, and unmapped families never buffer at all (WithholdStreamArgs is
// false for familyNone), so generic clients stay byte-identical.

// fanoutKeyBase offsets synthetic toolCalls map keys for fan-out extras on
// the Anthropic relay. Upstream tool indexes are small dense ints (native
// and XML-synthetic alike), so base+sequence keys can never collide with a
// real index, and ascending map order keeps parents (by upstream index)
// before their extras — the emission order the terminal flush follows.
const fanoutKeyBase = int64(1) << 62

// fanoutKey returns the toolCalls map key for the k-th block (0-based
// sequence across the whole stream) emitted by fan-out expansion.
func fanoutKey(seq int) int {
	return int(fanoutKeyBase) + seq
}

// --- Anthropic ---

// noteAnthropicWithhold records the wire name on first sight and reports
// whether fragments for this call must buffer. ts accumulates args either
// way; the caller skips ALL live emission (start, deltas, sawToolCall)
// when this returns true.
func noteAnthropicWithhold(st *anthropicStreamState, ts *anthropicToolState, wire string) bool {
	if !ts.withholdChecked && wire != "" {
		ts.withholdChecked = true
		ts.wire = wire
		ts.withhold = st.toolMap.WithholdStreamArgs(wire)
	}
	return ts.withhold
}

// finalizeAnthropicWithheld emits every buffered family call in upstream
// index order: reshaped+fanned-out tool_use blocks (first keeps its block
// slot, extras take fresh indexes with suffixed ids), fallback text into
// the text block, absorbs vanished. Open blocks close first so the flushed
// blocks open sequentially. Idempotent: each call flushes once, and calls
// that never resolved to a name are skipped (the live path would also
// never have opened a block for them).
func (s *Server) finalizeAnthropicWithheld(send func(map[string]any), st *anthropicStreamState) {
	indexes := make([]int, 0, len(st.toolCalls))
	for i, ts := range st.toolCalls {
		if ts == nil || !ts.withhold || ts.flushed || ts.name == "" {
			continue
		}
		indexes = append(indexes, i)
	}
	if len(indexes) == 0 {
		return
	}
	sort.Ints(indexes)
	// Withheld blocks emit after everything streamed so far: close the
	// open blocks so the flushed starts cannot straddle them.
	st.closeThinking(send)
	st.closeText(send)
	st.closeOpenToolCalls(send)
	for _, upIdx := range indexes {
		ts := st.toolCalls[upIdx]
		ts.flushed = true
		s.emitAnthropicWithheld(send, st, ts)
	}
}

// emitAnthropicWithheld finalizes one buffered call onto the stream.
func (s *Server) emitAnthropicWithheld(send func(map[string]any), st *anthropicStreamState, ts *anthropicToolState) {
	_ = s
	calls, text, absorb := st.toolMap.FinalizeStreamedCall(ts.wire, ts.id, ts.args.String())
	if text != "" {
		st.textParts = append(st.textParts, text)
		st.ensureText(send)
		send(map[string]any{
			"type":  "content_block_delta",
			"index": st.textIndex,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})
	}
	if absorb || len(calls) == 0 {
		return
	}
	for k, call := range calls {
		if k == 0 {
			ts.id = call.ID
			ts.name = call.Name
			ts.args.Reset()
			ts.args.WriteString(call.Arguments)
			// The fragment-time slot was reserved before later blocks
			// emitted; reassign a fresh sequential index at emission so
			// block indexes stay ordered.
			ts.index = st.nextBlockIdx
			st.nextBlockIdx++
			st.registerToolID(ts.id)
			st.sawToolCall = true
			st.ensureStarted(ts, send)
			continue
		}
		ets := &anthropicToolState{index: st.nextBlockIdx, id: call.ID, name: call.Name}
		st.nextBlockIdx++
		ets.args.WriteString(call.Arguments)
		ets.started = false
		st.toolCalls[fanoutKey(st.nextFanout)] = ets
		st.nextFanout++
		st.registerToolID(ets.id)
		st.sawToolCall = true
		st.ensureStarted(ets, send)
		// Complete at emission: start + full input delta now, stop at
		// once — no later fragment can target an extra.
		send(map[string]any{"type": "content_block_stop", "index": ets.index})
		ets.blockClosed = true
	}
}

// registerToolID records an emitted tool id for the stop accounting and
// the reasoning-cache identity (mirrors the fragment-time bookkeeping).
func (st *anthropicStreamState) registerToolID(id string) {
	if id == "" {
		return
	}
	if st.toolIDsSeen == nil {
		st.toolIDsSeen = make(map[string]bool)
	}
	if !st.toolIDsSeen[id] {
		st.toolIDsSeen[id] = true
		st.toolIDs = append(st.toolIDs, id)
		if sID := sanitizeToolID(id); sID != id && !st.toolIDsSeen[sID] {
			st.toolIDsSeen[sID] = true
			st.toolIDs = append(st.toolIDs, sID)
		}
	}
}

// --- Responses ---

// finalizeResponsesWithheld emits every buffered family call in upstream
// index order: reshaped+fanned-out function_call items (added with the
// resolved identity, full args as one delta pair each), fallback text
// into the message item, absorbs vanished. Idempotent.
func (s *Server) finalizeResponsesWithheld(st *responsesStreamState, send func(string, map[string]any)) {
	_ = s
	indexes := make([]int, 0, len(st.toolByUpIdx))
	for i, item := range st.toolByUpIdx {
		if item == nil || !item.withhold || item.flushed || item.wire == "" {
			continue
		}
		indexes = append(indexes, i)
	}
	if len(indexes) == 0 {
		return
	}
	sort.Ints(indexes)
	for _, upIdx := range indexes {
		item := st.toolByUpIdx[upIdx]
		item.flushed = true
		finalizeOneResponsesWithheld(st, send, item)
	}
	// Prune absorbed calls: they render nothing dispatchable, so the
	// terminal done-event walk + completed output must not see them.
	kept := st.items[:0]
	for _, item := range st.items {
		if item.dropped {
			for upIdx, mapped := range st.toolByUpIdx {
				if mapped == item {
					delete(st.toolByUpIdx, upIdx)
				}
			}
			continue
		}
		kept = append(kept, item)
	}
	st.items = kept
}

// finalizeOneResponsesWithheld finalizes a single buffered function call.
func finalizeOneResponsesWithheld(st *responsesStreamState, send func(string, map[string]any), item *responsesItem) {
	calls, text, absorb := st.toolMap.FinalizeStreamedCall(item.wire, item.callID, item.args.String())
	if text != "" {
		msgItem := responsesMessageItem(st, send)
		msgItem.text += text
		send("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": msgItem.id, "output_index": msgItem.outputIndex, "content_index": msgItem.contentIdx, "delta": text})
	}
	if absorb || len(calls) == 0 {
		// Nothing dispatchable: exclude from the terminal done-event
		// walk + completed output (pruned by the caller).
		item.dropped = true
		return
	}
	for k, call := range calls {
		if k == 0 {
			item.callID = call.ID
			item.name = call.Name
			item.args.Reset()
			item.args.WriteString(call.Arguments)
			// The fragment-time output slot was reserved before later
			// items emitted; reassign a fresh sequential index so
			// output_index follows emission order.
			item.outputIndex = st.nextIndex
			st.nextIndex++
			emitResponsesFunctionCall(send, item)
			continue
		}
		extra := &responsesItem{id: "fc_" + randHexString(12), kind: "function_call", outputIndex: st.nextIndex, callID: call.ID, name: call.Name}
		st.nextIndex++
		extra.args.WriteString(call.Arguments)
		extra.started = true
		st.items = append(st.items, extra)
		emitResponsesFunctionCall(send, extra)
	}
}

// emitResponsesFunctionCall announces a completed function_call item and
// relays its full arguments as one delta pair: the same event set the
// live path emits per fragment (added → arguments deltas), delivered once
// the whole is known. The terminal done events + completed output come
// from endResponsesStream's item walk, unchanged.
func emitResponsesFunctionCall(send func(string, map[string]any), item *responsesItem) {
	args := item.args.String()
	if !item.announced {
		item.announced = true
		send("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": item.outputIndex, "item": map[string]any{"id": item.id, "type": "function_call", "status": "in_progress", "call_id": item.callID, "name": item.name, "arguments": ""}})
	}
	send("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "item_id": item.id, "output_index": item.outputIndex, "delta": args})
	send("response.custom_tool_call_input.delta", map[string]any{"type": "response.custom_tool_call_input.delta", "item_id": item.id, "output_index": item.outputIndex, "delta": args})
}

// responsesMessageItem returns the stream's message item, creating and
// announcing it when the turn has produced no text yet.
func responsesMessageItem(st *responsesStreamState, send func(string, map[string]any)) *responsesItem {
	for _, it := range st.items {
		if it.kind == "message" {
			if !it.started {
				it.started = true
				sendResponsesItemAdded(send, it)
			}
			return it
		}
	}
	item := &responsesItem{id: "msg_" + randHexString(12), kind: "message", outputIndex: st.nextIndex}
	st.nextIndex++
	st.items = append(st.items, item)
	item.started = true
	sendResponsesItemAdded(send, item)
	return item
}
