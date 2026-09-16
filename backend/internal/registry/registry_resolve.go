package registry

import (
	"fmt"
	"strings"
)

// ResolveModel strips reasoning-effort / context suffixes (e.g. "(max)",
// "(high)", ":max") so the bare upstream id is sent on the wire.
// The proxy NEVER auto-upgrades base models to their -max extended-context
// variants: those are per-account upstream provisions (unprovisioned accounts
// are coerced upstream), so a client that holds a -max grant requests the id
// literally.
func (r *Registry) ResolveModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}

	if strings.HasSuffix(model, ")") {
		if idx := strings.LastIndex(model, "("); idx > 0 {
			tag := strings.ToLower(strings.TrimSpace(model[idx+1 : len(model)-1]))
			switch tag {
			case "max", "high", "medium", "low", "minimal", "xhigh", "ultra":
				model = strings.TrimSpace(model[:idx])
			}
		}
	} else if idx := strings.LastIndex(model, ":"); idx > 0 {
		tag := strings.ToLower(strings.TrimSpace(model[idx+1:]))
		switch tag {
		case "max", "high", "medium", "low", "minimal", "xhigh", "ultra":
			model = strings.TrimSpace(model[:idx])
		}
	}

	// Claude Code's extended-context marker (reference/agents/claude-code):
	// the CLI appends "[1m]" to models it believes support the 1M-context beta
	// and mirrors the capability in anthropic-beta. The marker is a client-side
	// context hint, not part of the upstream model id — strip it so the
	// served-model gate resolves the bare id. Mirrors 9router's
	// stripModelContextMarker (reference/routers/9router).
	if strings.HasSuffix(model, "]") {
		if idx := strings.LastIndex(model, "["); idx > 0 {
			tag := strings.ToLower(strings.TrimSpace(model[idx+1 : len(model)-1]))
			switch tag {
			case "1m", "200k":
				model = strings.TrimSpace(model[:idx])
			}
		}
	}

	return model
}

// AgentForModel returns the agent id that serves model (after suffix
// stripping), or an ErrModelNotFound-wrapped error.
func (r *Registry) AgentForModel(model string) (string, error) {
	model = r.ResolveModel(model)
	r.mu.RLock()
	defer r.mu.RUnlock()
	agent, ok := r.modelToAgent[model]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrModelNotFound, model)
	}
	return agent, nil
}
