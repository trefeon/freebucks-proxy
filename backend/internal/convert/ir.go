package convert

import (
	"encoding/json"
	"strings"
)

// ir.go — canonical translation IR (Phase 1.1/1.2 of
// docs/UNIVERSAL-GATEWAY-PLAN.md).
//
// The request leg used to live entirely in JSON mutations with implicit
// contracts: NewToolMapper guessed names off the raw body, ToUpstream renamed
// them in place (deciding wire-name OWNERSHIP by array order),
// SubstituteCanonicalDefinitions swapped defs under the assigned names, and
// floorOnlyOMP dropped every rider for OMP-family toolsets. This file makes
// those contracts explicit as data: Decode → Own → Substitute+Floor → Emit,
// with every loss recorded as an IRDiagnostic instead of a silent drop.
//
// Wire bytes are unchanged by construction: the JSON appliers
// (toolmap_request.go, tools_normalize.go, tools_floor.go) remain the code
// that mutates payloads; the IR decides, annotates, and — via CheckWire —
// asserts agreement. Invariants carried (§2.1–2.6 of the plan): floor-only
// gate, first-wins ordered ownership, 64-char grammar, rename/reshape/
// restore contract, one mapper per request, delegation as Kind/ExecBy data.

// SurfaceFormat is the client surface a request body arrived on. Detection is
// a shape heuristic (see DetectSurface); family detection (detectFamilyBody)
// is unchanged and authoritative for translation.
type SurfaceFormat string

const (
	SurfaceUnknown   SurfaceFormat = ""
	SurfaceChat      SurfaceFormat = "chat"
	SurfaceAnthropic SurfaceFormat = "anthropic"
	SurfaceResponses SurfaceFormat = "responses"
)

// DetectSurface classifies a raw request body by envelope shape: Responses
// carries input[], Anthropic Messages carries tools[] with name/input_schema
// entries (or tool_use content blocks), everything else with messages[] is
// chat. Invalid bodies report SurfaceUnknown (never translate on doubt).
func DetectSurface(body []byte) SurfaceFormat {
	var payload struct {
		Messages []json.RawMessage `json:"messages"`
		Input    []json.RawMessage `json:"input"`
		Tools    []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return SurfaceUnknown
	}
	if len(payload.Input) > 0 {
		return SurfaceResponses
	}
	for _, t := range payload.Tools {
		if t.Name != "" || t.InputSchema != nil {
			return SurfaceAnthropic
		}
	}
	if len(payload.Messages) > 0 {
		// Anthropic content blocks can also appear inside messages; a
		// tool_use block is a stronger signal than the envelope.
		for _, raw := range payload.Messages {
			var msg struct {
				Content []struct {
					Type string `json:"type"`
				} `json:"content"`
			}
			if err := json.Unmarshal(raw, &msg); err != nil {
				continue
			}
			for _, b := range msg.Content {
				if b.Type == "tool_use" || b.Type == "server_tool_use" {
					return SurfaceAnthropic
				}
			}
		}
		return SurfaceChat
	}
	return SurfaceUnknown
}

// ToolKind classifies how a client tool reaches the wire. Client-side
// delegation (harness spawner names such as task/hub/spawn_agent) never
// enters wire claims: the IR records it as KindDelegated + ExecBy instead of
// folklore in comments.
type ToolKind string

const (
	// KindDirect is a client name that already is the wire name (official
	// passthrough or unknown custom riding verbatim).
	KindDirect ToolKind = "direct"
	// KindMapped is a client name renamed onto an official signature tool.
	KindMapped ToolKind = "mapped"
	// KindVirtual is a foreign-harness or grammar-illegal name virtualized
	// into the mcp__ namespace (or a dedupe virtualization of a duplicate
	// wire claim).
	KindVirtual ToolKind = "virtual"
	// KindDelegated is a harness spawner name (task/hub/spawn_agent) that
	// must never claim a wire name.
	KindDelegated ToolKind = "delegated"
	// KindFloor is a canonical CLI definition emitted by the floor, not
	// declared by the client.
	KindFloor ToolKind = "floor"
)

// delegatedClientTools are harness spawner names that never enter wire
// claims (docs/decisions/tool-name-translation.md:297-320). Lowercase.
var delegatedClientTools = map[string]bool{
	"task":        true,
	"hub":         true,
	"spawn_agent": true,
}

// Tool origin vocabulary for IRToolDef.Origin.
const (
	OriginClient    = "client"    // declared by the client as sent
	OriginCanonical = "canonical" // substituted canonical CLI definition
	OriginFloor     = "floor"     // emitted by the floor-only pass
	OriginPin       = "pin"       // end_turn/decide injection
)

// IRToolDef is one tool row of the canonical set: the client name, the wire
// name Own() assigned, how it got there, and which definition bytes it
// carries.
type IRToolDef struct {
	ClientName  string
	WireName    string
	Kind        ToolKind
	ExecBy      string // client name for KindDelegated, else ""
	Params      map[string]any
	Origin      string // OriginClient | OriginCanonical | OriginFloor | OriginPin
	Substituted bool   // def bytes replaced by the canonical CLI entry
	Dropped     bool   // floor-only pass removed this rider from the wire
}

// IRToolChoice is the decoded tool_choice: auto (default), none/disabled,
// or a pin on one client tool.
type IRToolChoice struct {
	Mode         string // "auto" | "none" | "pinned"
	PinnedClient string // client name as sent
	PinnedWire   string // wire name after Own()
}

// IRToolSet is the canonical tool set adapted from relaykit toolconv.Set:
// the definitions plus the choice plus the bidirectional name maps.
// Forward is client→wire, Reverse is wire→client (first-wins: the first
// claimer of a shared wire name owns the reverse slot, mirroring
// ToUpstream's ordered pass).
type IRToolSet struct {
	Defs     []IRToolDef
	Choice   IRToolChoice
	Parallel bool // false when tool_choice pins a single tool
	Forward  map[string]string
	Reverse  map[string]string
}

// Empty reports whether the set carries no rename (identity request).
func (s IRToolSet) Empty() bool { return len(s.Forward) == 0 && len(s.Reverse) == 0 }

// WireNames returns the wire names in client-declaration order.
func (s IRToolSet) WireNames() []string {
	out := make([]string, 0, len(s.Defs))
	for _, d := range s.Defs {
		if d.Dropped {
			continue
		}
		out = append(out, d.WireName)
	}
	return out
}

// RestoreName maps one upstream tool name back to the client's original
// (identity when unmapped). Same semantic as ToolMapper.RestoreName:
// map-first, then strip the mcp__ prefix ONLY when the Own pass added it
// (the stripped name forwards back to this exact upstream name).
func (s IRToolSet) RestoreName(name string) string {
	if orig, ok := s.Reverse[name]; ok {
		return orig
	}
	if strings.HasPrefix(name, "mcp__") {
		if stripped := strings.TrimPrefix(name, "mcp__"); stripped != "" {
			if up, ok := s.Forward[stripped]; ok && up == name {
				return stripped
			}
		}
	}
	return name
}

// Diagnostic severity vocabulary (relaykit ConversionSeverity, adapted).
const (
	SeverityWarning = "warning"
	SeverityError   = "error"
)

// IRDiagnostic is one machine-readable translation loss (Phase 1.2): every
// silent drop the old pipeline performed now emits one of these instead of
// vanishing. Code vocabulary is owned here; see the ir.* constants below.
type IRDiagnostic struct {
	Code     string // e.g. "ir.floor_drop"
	Path     string // payload path, e.g. "tools[3]" or "tool_choice"
	Message  string
	Severity string // SeverityWarning | SeverityError
	From     string // client-side name/shape
	To       string // wire-side name/shape ("" when dropped)
}

// Diagnostic code vocabulary.
const (
	DiagRename          = "ir.rename"            // client name renamed onto official wire name
	DiagVirtualize      = "ir.virtualize"        // foreign/grammar-illegal name virtualized to mcp__
	DiagDedupeVirtual   = "ir.dedupe_virtual"    // duplicate wire claim virtualized (first wins)
	DiagOwnIdentity     = "ir.own_identity"      // client's own name owns the wire slot (info)
	DiagSubstitute      = "ir.substitute"        // def bytes replaced by canonical CLI entry
	DiagFloorDrop       = "ir.floor_drop"        // floor-only pass dropped a rider
	DiagFloorPinStrip   = "ir.floor_pin_strip"   // tool_choice pin named a dropped wire name
	DiagChoiceRename    = "ir.choice_rename"     // tool_choice pin renamed onto the wire name
	DiagHistoryFold     = "ir.history_fold"      // resumed history call folded into text
	DiagReshapeApplied  = "ir.reshape_applied"   // CLI args reshaped to client shape
	DiagTextRender      = "ir.text_render"       // unroutable call rendered as text
	DiagTextAbsorb      = "ir.text_absorb"       // unroutable call absorbed (nothing user-facing)
	DiagDivergence      = "ir.divergence"        // IR decision disagrees with JSON applier (bug signal)
	DiagDelegatedNoWire = "ir.delegated_no_wire" // spawner name kept off the wire
)

// LossPolicy gates translation loss (relaykit ConversionLossPolicy,
// adapted): default allow is today's behavior byte-identical; safe/strict
// are available per-caller.
type LossPolicy string

const (
	LossAllow  LossPolicy = "allow"
	LossSafe   LossPolicy = "safe"
	LossStrict LossPolicy = "strict"
)

// RejectLoss enforces the loss policy over diagnostics: allow always passes,
// safe rejects on error-severity loss, strict rejects on any loss. The
// partial result is still returned alongside the error — the caller keeps
// whatever the pipeline built.
func RejectLoss(diags []IRDiagnostic, policy LossPolicy) error {
	switch policy {
	case LossSafe:
		for _, d := range diags {
			if d.Severity == SeverityError {
				return &LossRejectedError{Code: d.Code, Path: d.Path, Message: d.Message}
			}
		}
	case LossStrict:
		if len(diags) > 0 {
			d := diags[0]
			return &LossRejectedError{Code: d.Code, Path: d.Path, Message: d.Message}
		}
	}
	return nil
}

// LossRejectedError is the typed rejection RejectLoss returns.
type LossRejectedError struct {
	Code    string
	Path    string
	Message string
}

func (e *LossRejectedError) Error() string {
	if e.Path != "" {
		return "translation loss rejected (" + e.Code + " at " + e.Path + "): " + e.Message
	}
	return "translation loss rejected (" + e.Code + "): " + e.Message
}

// IRRequest is one decoded request: surface, family, client tools in
// declaration order, the decoded choice, and — after Own — the canonical
// set plus the diagnostics the passes emitted.
type IRRequest struct {
	Surface     SurfaceFormat
	Family      clientFamily // detectFamilyBody; OMP-first, unchanged
	Tools       []IRToolDef  // client declaration order, pre-Own (WireName/Kind unset)
	Choice      IRToolChoice // decoded pre-Own choice (PinnedWire unset)
	MsgCount    int
	ToolCount   int
	Set         IRToolSet // filled by Own
	Diagnostics []IRDiagnostic
	owned       bool
}

// FamilyName reports the detected client family for diagnostics and logs.
func (r *IRRequest) FamilyName() string {
	switch r.Family {
	case familyOMP:
		return "omp"
	case familyPi:
		return "pi"
	}
	return "none"
}

// DecodeRequestIR decodes a raw request body into an IRRequest: surface
// heuristic, unchanged family detection, client tools with their parameter
// schemas, and the raw tool_choice. No ownership, no mutation.
func DecodeRequestIR(body []byte) *IRRequest {
	r := &IRRequest{Family: detectFamilyBody(body), Surface: DetectSurface(body)}
	var payload struct {
		Messages []json.RawMessage `json:"messages"`
		Input    []json.RawMessage `json:"input"`
		Tools    []struct {
			Function struct {
				Name       string         `json:"name"`
				Parameters map[string]any `json:"parameters"`
			} `json:"function"`
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"input_schema"`
		} `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return r
	}
	r.MsgCount = len(payload.Messages)
	if r.MsgCount == 0 {
		r.MsgCount = len(payload.Input)
	}
	r.ToolCount = len(payload.Tools)
	for _, t := range payload.Tools {
		name := t.Function.Name
		params := t.Function.Parameters
		if name == "" {
			// Anthropic Messages shape: {name, input_schema}.
			name = t.Name
			params = t.InputSchema
		}
		if name == "" {
			continue
		}
		r.Tools = append(r.Tools, IRToolDef{
			ClientName: name,
			WireName:   name,
			Kind:       KindDirect,
			Params:     params,
			Origin:     OriginClient,
		})
	}
	r.Choice = decodeIRChoice(payload.ToolChoice)
	return r
}

// decodeIRChoice decodes the raw tool_choice: absent/"auto" → auto,
// "none" → none, {"function":{"name":…}} or {"name":…} → pinned.
func decodeIRChoice(raw json.RawMessage) IRToolChoice {
	if len(raw) == 0 {
		return IRToolChoice{Mode: "auto"}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "none":
			return IRToolChoice{Mode: "none"}
		default:
			return IRToolChoice{Mode: "auto"}
		}
	}
	var structured struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &structured); err != nil {
		return IRToolChoice{Mode: "auto"}
	}
	if structured.Function.Name != "" {
		return IRToolChoice{Mode: "pinned", PinnedClient: structured.Function.Name}
	}
	if structured.Name != "" {
		return IRToolChoice{Mode: "pinned", PinnedClient: structured.Name}
	}
	return IRToolChoice{Mode: "auto"}
}

// Own moves ToUpstream's ownership pass off JSON onto the IR: same ordered,
// first-wins algorithm (same resolveUpstreamTool, same wireVirtualName
// dedupe), but the decisions land in Set with diagnostics instead of
// mutating a payload. The JSON applier stays the byte-emitting path;
// CheckWire proves they agree.
func (r *IRRequest) Own() {
	set := IRToolSet{
		Forward: make(map[string]string, len(r.Tools)),
		Reverse: make(map[string]string, len(r.Tools)),
		Choice:  r.Choice,
	}
	set.Parallel = r.Choice.Mode != "pinned"
	used := make(map[string]bool, len(r.Tools))
	defs := make([]IRToolDef, 0, len(r.Tools))
	for i, t := range r.Tools {
		d := t
		upstreamName := resolveUpstreamTool(t.ClientName, t.Params)
		path := "tools[" + itoa(i) + "]"
		if _, isDel := delegatedClientTools[strings.ToLower(t.ClientName)]; isDel && upstreamName == t.ClientName {
			// A spawner name with no official mapping must never claim a
			// wire name, even its own: virtualize it like any foreign
			// harness tool so the gate never sees a harness name.
			upstreamName = wireVirtualName(t.ClientName)
		}
		if upstreamName == "" {
			upstreamName = t.ClientName
		}
		d.WireName = upstreamName
		switch {
		case delegatedClientTools[strings.ToLower(t.ClientName)] && upstreamName != t.ClientName &&
			clientToOfficial[strings.ToLower(t.ClientName)] == "":
			d.Kind = KindDelegated
			d.ExecBy = t.ClientName
			r.Diagnostics = append(r.Diagnostics, IRDiagnostic{
				Code: DiagDelegatedNoWire, Path: path,
				Message:  "harness spawner kept off the wire",
				Severity: SeverityWarning, From: t.ClientName, To: upstreamName,
			})
		case upstreamName != t.ClientName && clientToOfficial[strings.ToLower(t.ClientName)] != "":
			d.Kind = KindMapped
			r.Diagnostics = append(r.Diagnostics, IRDiagnostic{
				Code: DiagRename, Path: path,
				Message:  "client tool renamed onto official signature tool",
				Severity: SeverityWarning, From: t.ClientName, To: upstreamName,
			})
		case upstreamName != t.ClientName:
			d.Kind = KindVirtual
			r.Diagnostics = append(r.Diagnostics, IRDiagnostic{
				Code: DiagVirtualize, Path: path,
				Message:  "foreign/grammar-illegal name virtualized",
				Severity: SeverityWarning, From: t.ClientName, To: upstreamName,
			})
		default:
			d.Kind = KindDirect
		}
		if upstreamName != t.ClientName {
			set.Forward[t.ClientName] = upstreamName
		}
		finalName := upstreamName
		if used[finalName] {
			// Duplicate wire claim: later occurrence virtualizes (first
			// wins), mirroring ToUpstream's dedupe including the counter.
			virt := wireVirtualName(t.ClientName)
			for k := 2; used[virt]; k++ {
				virt = wireVirtualName(t.ClientName + "#" + itoa(k))
			}
			set.Forward[t.ClientName] = virt
			set.Reverse[virt] = t.ClientName
			d.WireName = virt
			d.Kind = KindVirtual
			r.Diagnostics = append(r.Diagnostics, IRDiagnostic{
				Code: DiagDedupeVirtual, Path: path,
				Message:  "duplicate wire claim virtualized (first wins)",
				Severity: SeverityWarning, From: t.ClientName, To: virt,
			})
			finalName = virt
		} else if t.ClientName == finalName {
			// The client's own name IS the wire name: this tool owns the
			// slot, clearing any provisional reverse entry (Roo-Code
			// write_file vs write_to_file precedent).
			delete(set.Reverse, finalName)
			r.Diagnostics = append(r.Diagnostics, IRDiagnostic{
				Code: DiagOwnIdentity, Path: path,
				Message:  "client name owns the wire slot",
				Severity: SeverityWarning, From: t.ClientName, To: finalName,
			})
		} else if _, taken := set.Reverse[finalName]; !taken {
			set.Reverse[finalName] = t.ClientName
		}
		used[finalName] = true
		defs = append(defs, d)
	}
	if set.Choice.Mode == "pinned" {
		if wire, ok := set.Forward[set.Choice.PinnedClient]; ok {
			set.Choice.PinnedWire = wire
			r.Diagnostics = append(r.Diagnostics, IRDiagnostic{
				Code: DiagChoiceRename, Path: "tool_choice",
				Message:  "tool_choice pin renamed onto the wire name",
				Severity: SeverityWarning, From: set.Choice.PinnedClient, To: wire,
			})
		} else {
			set.Choice.PinnedWire = set.Choice.PinnedClient
		}
	}
	set.Defs = defs
	r.Set = set
	r.owned = true
}

// itoa is strconv.Itoa without importing strconv in this file's import set.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
