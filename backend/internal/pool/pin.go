package pool

import (
	"fmt"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/registry"
)

// Single-pin routing (PIN_MODEL): each pool slot may be pinned to exactly
// one model id. Slots without an entry serve any model (today's behavior,
// fully backward compatible).

// pinnedOut reports whether the slot at idx must not serve model: the slot
// carries a pin entry naming a different model (comparing both the
// requested id and its registry canonical form). A nil registry (or nil
// config) disables matching to exact strings; a nil/empty pin map never
// pins anything out.
func pinnedOut(cfg *config.Config, reg *registry.Registry, idx int, model string) bool {
	if cfg == nil || len(cfg.PinModel) == 0 {
		return false
	}
	pin, ok := cfg.PinModel[idx]
	if !ok || pin == "" {
		return false
	}
	if model == pin {
		return false
	}
	if reg != nil && reg.ResolveModel(model) == reg.ResolveModel(pin) {
		return false
	}
	return true
}

// allPinnedOut reports whether every slot is pinned away from model (each
// slot carries a pin entry and none names the model). Callers fail fast
// with pinFailFastError instead of burning quota on doomed admissions.
func allPinnedOut(toks *[]*tokenEntry, cfg *config.Config, reg *registry.Registry, model string) bool {
	if toks == nil || len(*toks) == 0 || cfg == nil || len(cfg.PinModel) == 0 {
		return false
	}
	for idx := range *toks {
		if !pinnedOut(cfg, reg, idx, model) {
			return false
		}
	}
	return true
}

// pinFailFastError is the client-facing error when no pool account is
// pinned to the requested model. It names the model and states that no
// upstream admission was attempted, so operators recognize a routing
// misconfiguration instead of an upstream outage.
func pinFailFastError(model string, slots int) error {
	return fmt.Errorf("pool: no account pinned to model %q (all %d pool slot(s) pinned to other models); no upstream admission attempted — adjust PIN_MODEL", model, slots)
}

// allLockedOut reports whether every pool slot is administratively locked.
// Callers fail fast with lockedFailFastError instead of the generic
// combined error, which would otherwise read as an upstream outage while no
// upstream admission was ever attempted.
func allLockedOut(toks *[]*tokenEntry) bool {
	if toks == nil || len(*toks) == 0 {
		return false
	}
	for _, tok := range *toks {
		if tok == nil || !tok.locked.Load() {
			return false
		}
	}
	return true
}

// lockedFailFastError is the client-facing error when no pool account can
// serve because every slot is administratively locked. It names the lock
// and states that no upstream admission was attempted, so operators
// recognize an operator lock instead of an upstream outage.
func lockedFailFastError(slots int) error {
	return fmt.Errorf("pool: all %d pool account(s) administratively locked; no upstream admission attempted — unlock a token", slots)
}
