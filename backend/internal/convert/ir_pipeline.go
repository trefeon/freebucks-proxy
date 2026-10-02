package convert

import (
	"encoding/json"
)

// ir_pipeline.go — Decode → Own → Substitute+Floor → Emit restructure
// (Phase 1.3 of docs/UNIVERSAL-GATEWAY-PLAN.md).
//
// NormalizeRequestMappedIR is the primary request-leg pipeline: it builds
// the IR (Decode), decides ownership on the IR (Own), runs the existing
// JSON appliers unchanged (the Emit path — byte-identical by construction),
// then annotates the IR to match (Substitute+Floor) and asserts the IR and
// the payload agree (CheckWire). NormalizeRequestMappedOpts delegates to it
// and discards the IR, so every existing caller keeps its signature and its
// bytes.
//
// Family detection is unchanged (detectFamilyBody, OMP-first); provenance
// (Phase 0.5) is recorded per def via Origin.

// SubstituteAndFloor annotates the owned set with the substitute + floor
// decisions: defs on substituted wire names carry canonical bytes
// (OriginCanonical), the OMP floor drops every rider (Dropped + diagnostic)
// and emits the 16 canonical defs (KindFloor/OriginFloor), end_turn/decide
// pins keep OriginPin. A tool_choice pin naming a dropped wire name is
// recorded as an error-severity diagnostic (the applier removes it).
func (r *IRRequest) SubstituteAndFloor() {
	if !r.owned {
		r.Own()
	}
	floor := r.Family == familyOMP
	for i, d := range r.Set.Defs {
		if d.ClientName == "end_turn" || d.ClientName == "decide" {
			r.Set.Defs[i].Origin = OriginPin
			continue
		}
		if substitutedWireNames[d.WireName] {
			r.Set.Defs[i].Substituted = true
			if r.Set.Defs[i].Origin == OriginClient {
				r.Set.Defs[i].Origin = OriginCanonical
			}
			r.Diagnostics = append(r.Diagnostics, IRDiagnostic{
				Code: DiagSubstitute, Path: "tools[" + itoa(i) + "]",
				Message:  "def bytes replaced by canonical CLI entry",
				Severity: SeverityWarning, From: d.ClientName, To: d.WireName,
			})
		}
		if floor && !substitutedWireNames[d.WireName] &&
			d.WireName != "end_turn" && d.WireName != "decide" {
			// The floor-only pass drops this rider: the gate sees foreign
			// schemas riding alongside the floor as a 503.
			r.Set.Defs[i].Dropped = true
			r.Diagnostics = append(r.Diagnostics, IRDiagnostic{
				Code: DiagFloorDrop, Path: "tools[" + itoa(i) + "]",
				Message:  "floor-only pass dropped non-floor rider",
				Severity: SeverityWarning, From: d.ClientName, To: d.WireName,
			})
		}
	}
	if floor {
		// The floor emits the canonical defs the client never declared.
		defs, err := canonicalToolDefs()
		if err == nil {
			for _, raw := range defs {
				m, _ := raw.(map[string]any)
				if m == nil {
					continue
				}
				fn, _ := m["function"].(map[string]any)
				if fn == nil {
					continue
				}
				name, _ := fn["name"].(string)
				if name == "" {
					continue
				}
				r.Set.Defs = append(r.Set.Defs, IRToolDef{
					ClientName: name, WireName: name,
					Kind: KindFloor, Origin: OriginFloor,
				})
			}
		}
		if r.Set.Choice.Mode == "pinned" {
			kept := false
			for _, d := range r.Set.Defs {
				if !d.Dropped && d.WireName == r.Set.Choice.PinnedWire {
					kept = true
					break
				}
			}
			if !kept {
				r.Diagnostics = append(r.Diagnostics, IRDiagnostic{
					Code: DiagFloorPinStrip, Path: "tool_choice",
					Message:  "tool_choice pin named a dropped wire name",
					Severity: SeverityError, From: r.Set.Choice.PinnedClient, To: r.Set.Choice.PinnedWire,
				})
			}
		}
	}
}

// wireNameSet returns the wire names the IR predicts on the wire.
func (r *IRRequest) wireNameSet() map[string]bool {
	out := make(map[string]bool, len(r.Set.Defs))
	for _, d := range r.Set.Defs {
		if d.Dropped {
			continue
		}
		out[d.WireName] = true
	}
	return out
}

// payloadWireNames reads the function names out of payload["tools"].
func payloadWireNames(payload map[string]any) []string {
	tools, ok := payload["tools"].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, t := range tools {
		m, _ := t.(map[string]any)
		if m == nil {
			continue
		}
		fn, _ := m["function"].(map[string]any)
		if fn == nil {
			continue
		}
		if name, _ := fn["name"].(string); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// CheckWire asserts the IR decisions agree with the emitted payload: same
// wire-name set, and the threaded ToolMapper's maps agree with the IR set.
// Disagreements are error diagnostics (bug signals), never request failures —
// the payload on the wire is authoritative.
func (r *IRRequest) CheckWire(payload map[string]any, mapper ToolMapper) {
	want := r.wireNameSet()
	for _, name := range payloadWireNames(payload) {
		if !want[name] && name != "end_turn" && name != "decide" {
			r.Diagnostics = append(r.Diagnostics, IRDiagnostic{
				Code: DiagDivergence, Path: "tools",
				Message:  "payload wire name the IR did not predict",
				Severity: SeverityError, From: "", To: name,
			})
		}
		delete(want, name)
	}
	for name := range want {
		r.Diagnostics = append(r.Diagnostics, IRDiagnostic{
			Code: DiagDivergence, Path: "tools",
			Message:  "IR-predicted wire name missing from payload",
			Severity: SeverityError, From: "", To: name,
		})
	}
	for client, wire := range r.Set.Forward {
		if got, ok := mapper.clientToUpstream[client]; ok && got != wire {
			r.Diagnostics = append(r.Diagnostics, IRDiagnostic{
				Code: DiagDivergence, Path: "tools",
				Message:  "mapper forward map disagrees with IR ownership",
				Severity: SeverityError, From: client, To: got,
			})
		}
	}
	for wire, client := range r.Set.Reverse {
		if got, ok := mapper.upstreamToClient[wire]; ok && got != client {
			r.Diagnostics = append(r.Diagnostics, IRDiagnostic{
				Code: DiagDivergence, Path: "tools",
				Message:  "mapper reverse map disagrees with IR ownership",
				Severity: SeverityError, From: wire, To: got,
			})
		}
	}
}

// NormalizeRequestMappedIR is the IR-structured request pipeline: Decode the
// body, Own the wire names on the IR, run the established Emit appliers in
// their documented order, annotate Substitute+Floor on the IR, and CheckWire
// agreement. Wire bytes are emitted ONLY by the pre-existing appliers, in
// their pre-existing order.
func NormalizeRequestMappedIR(body []byte, modelOverride string, opts Options) ([]byte, ToolMapper, *IRRequest, error) {
	ir := DecodeRequestIR(body)
	ir.Own()
	mapper := NewToolMapper(body)
	family := detectFamilyBody(body)
	out, err := NormalizeRequestOpts(body, modelOverride, opts)
	if err != nil {
		return nil, ToolMapper{}, ir, err
	}
	// Apply renames on top of the normalized body.
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		return out, ToolMapper{}, ir, nil //nolint:NormalizeRequest already validated; unreachable in practice
	}
	mapper.ToUpstream(payload)
	// Foreign-definition substitution: entries renamed onto CLI wire names
	// carry the canonical CLI description + parameters, so the gate sees
	// CLI definitions, never foreign schemas under renamed names.
	SubstituteCanonicalDefinitions(payload)
	// Family response translation (tools_floor.go). OMP-family toolsets go
	// floor-only: no foreign-schema rider may reach the gate (live
	// 2026-09-30). pi-family toolsets keep their substituted wire (every pi
	// core tool already maps to an official name) and need only the
	// response-leg pi arg reshape + unroutable text render. Non-family
	// clients keep the riding top-up.
	mapper.family = family
	if family == familyOMP {
		floorOnlyOMP(payload)
		// Undeclared floor tools route to the OMP equivalent so model
		// calls to ask_user/read_url/list_directory/skill restore +
		// reshape instead of failing client-side with "not found".
		mapper.RegisterFloorFallbacks()
		// Replay tool calls in historical assistant messages must match
		// the canonical floor tools on the wire.
		mapper.RenameMessagesToolCalls(payload)
		// Resumed history may call harness-only tools the floor dropped
		// (task/eval/learn/...): fold those calls (and their echoes) into
		// assistant text so the wire carries no call to an undeclared
		// tool, which upstream refuses.
		foldResumedHistoryCalls(payload)
	}
	mapper.RenameRequestToolChoice(payload)
	ir.SubstituteAndFloor()
	ir.CheckWire(payload, mapper)
	renamed, merr := json.Marshal(payload)
	if merr != nil {
		return out, ToolMapper{}, ir, nil // fall back to unrenamed rather than fail the request
	}
	return renamed, mapper, ir, nil
}
