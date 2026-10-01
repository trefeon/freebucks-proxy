package convert

import "strings"

// registry.go — the single source-of-truth *API* for the tool-translation
// tables (Phase 1 of docs/UNIVERSAL-TOOLS-PLAN.md; audit:
// docs/UNIVERSAL-TOOLS-AUDIT.md).
//
// The four tables the request and response legs read live beside the code
// that owns them, and every access goes through the accessors here so a new
// family or a new mapping is added in exactly one place:
//
//   - Name table   — clientToOfficial / officialTools (toolmap_request.go):
//     NameFor / IsOfficialWireName.
//   - Arg table    — reshapeRules / reshapeCliKeys / ompClientToWire /
//     piReshapeWires (tools_reshape.go): ReshapeWireNames.
//   - Floor table  — floorFallbacks (tools_floor.go): FloorRoutes.
//   - Detection    — detectFamilyBody (tools_floor.go): FamilyOf.
//
// The response leg still selects a rule by client family at the call site
// (fanoutArgs); these accessors are the stable surface future phases (new
// families, surface adapters) grow against without reaching into the raw
// maps.

// NameFor returns the official wire name a client tool maps to, or "" when
// the client name has no explicit mapping (such names virtualize/passthrough
// in resolveUpstreamTool instead).
func NameFor(client string) string {
	return clientToOfficial[strings.ToLower(client)]
}

// IsOfficialWireName reports whether name is one of the official signature
// tool names a mapping may target.
func IsOfficialWireName(name string) bool { return officialTools[name] }

// ReshapeWireNames returns the wire names that carry an argument-reshape
// rule. Used by tests and by surface adapters that need to know whether a
// wire name participates in reshaping without holding a ToolMapper.
func ReshapeWireNames() []string {
	out := make([]string, 0, len(reshapeRules))
	for name := range reshapeRules {
		out = append(out, name)
	}
	return out
}

// FloorRoutes returns the wire→client restore routes a flooring family
// registers. OMP owns the floor today; every other family returns nil.
func FloorRoutes(f clientFamily) map[string]string {
	if f != familyOMP {
		return nil
	}
	routes := make(map[string]string, len(floorFallbacks))
	for wire, client := range floorFallbacks {
		routes[wire] = client
	}
	return routes
}

// FamilyOf is the documented family-detection entry point: it classifies the
// raw client tools in a request body. Invalid bodies report familyNone
// (never translate a family on doubt).
func FamilyOf(body []byte) clientFamily { return detectFamilyBody(body) }
