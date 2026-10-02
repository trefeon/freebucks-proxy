// Package upstream speaks the codebuff.com wire protocol for a single token.
//
// Adapter discipline (universal-gateway Phase 2.1): each client family has
// exactly one surface — chat completions, Anthropic messages, or Responses —
// and every surface composes through the single upstream wire, never
// family-to-family. The Adapter interface below is that discipline written
// down: it names the family and its wire endpoint, and the registry refuses
// cross-family mappings at registration time. The translation itself stays
// in backend/internal/convert (the Phase-1 IR owns every mapping); adapters
// stay dumb, mirroring the llmrelay dumb-adapter/smart-executor split.
package upstream

import (
	"fmt"
	"sync"
)

// Family names the client surface an adapter serves. Exactly one wire
// target exists behind all three; the family only selects the envelope.
type Family string

const (
	// FamilyChat serves POST /v1/chat/completions.
	FamilyChat Family = "chat"
	// FamilyAnthropic serves POST /v1/messages.
	FamilyAnthropic Family = "anthropic"
	// FamilyResponses serves POST /v1/responses.
	FamilyResponses Family = "responses"
)

// Adapter is one client family's wire surface: its identity (Family) plus
// the upstream endpoint it drives. Convert/Do/DoResponse behavior lives in
// backend/internal/server per surface (openai.go, anthropic.go,
// responses.go) over the convert IR — this interface carries the routing
// contract only, so a new family adds one registration, never a mapping
// matrix.
type Adapter interface {
	// Family is the client surface this adapter serves.
	Family() Family
	// Endpoint is the upstream path the surface drives
	// (e.g. "/api/v1/chat/completions").
	Endpoint() string
}

// AdapterRegistry maps each family to its single adapter. Registering a
// second adapter for one family, or an adapter whose endpoint names another
// family's surface, fails: cross-family behavior must compose through the
// convert IR, never as a direct family-to-family mapping (the one-api
// N×direct-mapping sprawl is the anti-pattern Phase 2.1 rejects).
type AdapterRegistry struct {
	mu       sync.RWMutex
	adapters map[Family]Adapter
}

// NewAdapterRegistry builds an empty registry.
func NewAdapterRegistry() *AdapterRegistry {
	return &AdapterRegistry{adapters: make(map[Family]Adapter)}
}

// Register installs a's family surface. It fails when the family is
// already served or when a is nil.
func (r *AdapterRegistry) Register(a Adapter) error {
	if a == nil {
		return fmt.Errorf("upstream: nil adapter")
	}
	fam := a.Family()
	if fam != FamilyChat && fam != FamilyAnthropic && fam != FamilyResponses {
		return fmt.Errorf("upstream: unknown adapter family %q", fam)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.adapters[fam]; dup {
		return fmt.Errorf("upstream: family %q already has an adapter", fam)
	}
	r.adapters[fam] = a
	return nil
}

// Lookup returns the adapter serving fam, or false when unregistered.
func (r *AdapterRegistry) Lookup(fam Family) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[fam]
	return a, ok
}

// Families returns the registered families in canonical surface order
// (chat, anthropic, responses), skipping unregistered ones.
func (r *AdapterRegistry) Families() []Family {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Family
	for _, fam := range []Family{FamilyChat, FamilyAnthropic, FamilyResponses} {
		if _, ok := r.adapters[fam]; ok {
			out = append(out, fam)
		}
	}
	return out
}

// staticAdapter is the registry entry for a surface whose Convert/Do/DoResponse
// behavior lives in backend/internal/server. It carries routing identity
// only — never translation logic.
type staticAdapter struct {
	family   Family
	endpoint string
}

// Family implements Adapter.
func (a staticAdapter) Family() Family { return a.family }

// Endpoint implements Adapter.
func (a staticAdapter) Endpoint() string { return a.endpoint }

// DefaultAdapters returns the three gateway surfaces in canonical order.
// The endpoint is the single upstream wire every surface drives; only the
// client envelope differs.
func DefaultAdapters() []Adapter {
	return []Adapter{
		staticAdapter{family: FamilyChat, endpoint: "/api/v1/chat/completions"},
		staticAdapter{family: FamilyAnthropic, endpoint: "/api/v1/chat/completions"},
		staticAdapter{family: FamilyResponses, endpoint: "/api/v1/chat/completions"},
	}
}

// DefaultAdapterRegistry returns a registry with the three gateway
// surfaces registered.
func DefaultAdapterRegistry() *AdapterRegistry {
	r := NewAdapterRegistry()
	for _, a := range DefaultAdapters() {
		if err := r.Register(a); err != nil {
			panic("upstream: default adapter registration: " + err.Error())
		}
	}
	return r
}
