package server

import (
	"fmt"
	"sync"
)

// Client surfaces (universal-gateway Phase 2.1): each client family has one
// surface — Init/Convert/Do/DoResponse behavior composed through the
// Phase-1 convert IR, never family-to-family mappings.
//
// The per-surface handlers, wire translation, stream relays, and error
// envelopes live in their own files (openai.go, responses.go,
// anthropic.go); this file carries the routing contract only: a Surface is
// a family's identity plus its route, and SurfaceFor resolves a request
// family to exactly one surface. Cross-family behavior composes through the
// convert IR — a surface never imports another surface's translator.

// SurfaceFamily names the client surface. It mirrors
// upstream.Family without importing it: the server must not depend on the
// upstream client for routing identity (the upstream package is the wire,
// this package is the envelope).
type SurfaceFamily string

const (
	// SurfaceChat serves POST /v1/chat/completions (+ count_tokens).
	SurfaceChat SurfaceFamily = "chat"
	// SurfaceAnthropic serves POST /v1/messages (+ count_tokens).
	SurfaceAnthropic SurfaceFamily = "anthropic"
	// SurfaceResponses serves POST /v1/responses.
	SurfaceResponses SurfaceFamily = "responses"
)

// Surface is one client family's route identity.
type Surface interface {
	// Family is the client surface this serves.
	Family() SurfaceFamily
	// Route is the primary POST route for the surface.
	Route() string
}

type staticSurface struct {
	family SurfaceFamily
	route  string
}

// Family implements Surface.
func (s staticSurface) Family() SurfaceFamily { return s.family }

// Route implements Surface.
func (s staticSurface) Route() string { return s.route }

// surfaceTable is the single enumeration of client surfaces, in canonical
// order. A new family adds one row here plus one upstream adapter
// registration — never a mapping matrix.
var surfaceTable = []Surface{
	staticSurface{family: SurfaceChat, route: "/v1/chat/completions"},
	staticSurface{family: SurfaceAnthropic, route: "/v1/messages"},
	staticSurface{family: SurfaceResponses, route: "/v1/responses"},
}

var surfacesOnce = sync.OnceValue(func() map[SurfaceFamily]Surface {
	m := make(map[SurfaceFamily]Surface, len(surfaceTable))
	for _, s := range surfaceTable {
		m[s.Family()] = s
	}
	return m
})

// SurfaceFor resolves a client family to its single surface. Unknown
// families fail: there is no generic fallback surface, so a new family is
// onboarded explicitly (decoder + renderer pair over the convert IR).
func SurfaceFor(fam SurfaceFamily) (Surface, error) {
	if s, ok := surfacesOnce()[fam]; ok {
		return s, nil
	}
	return nil, fmt.Errorf("server: unknown client surface %q", fam)
}

// Surfaces returns the registered surfaces in canonical order.
func Surfaces() []Surface {
	return append([]Surface(nil), surfaceTable...)
}
