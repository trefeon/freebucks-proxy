package server

import (
	"errors"
	"freebucks-proxy/backend/internal/config"
	"strings"
)

// tokenMarkerKey is the settings-table presence marker for the AUTH_TOKENS
// pool ("true" while pooled): a cheap presence signal for readers that must
// not parse the pool. The raw pool itself is ALSO mirrored in the
// config:AUTH_TOKENS overlay row (the dashboard DB holds secrets at mode
// 0600) — tokenMarkerDelta converges both, so the overlay always carries
// the latest pool.
const tokenMarkerKey = "auth/tokens_configured"

// overlayWrite persists settings-table rows AND derives the new live
// snapshot from mem (unified-store sync applier): no .env write, no
// loadConfig re-read, no reload fan-out. Files are boot seed only.
//
//   - set maps full settings row keys (config.OverlayRowKey(...) or the
//     auth/ marker namespace) to their new values; del lists row keys to
//     drop. Both enqueue to the WAL spill behind (a nil store means
//     live-only: mem-only swap, lost on restart).
//   - verify may reject the derived config (divergence guard); the
//     rejection returns unwrapped and nothing was enqueued or swapped.
//
// Derivation and verification precede any enqueue, so a failure has nothing
// to roll back. Callers must hold adminSaveMu, then swap the returned
// config via applyReloadedConfig (which also flips mem synchronously for
// the next read); overlayWrite never swaps itself.
func (a *adminHandlers) overlayWrite(set map[string]string, del []string, verify func(config.Config) error) (config.Config, error) {
	oldCfg := a.cfgLoad()

	// Derive from mem (no disk): translate row keys to the overlay delta.
	// Non-config rows (the auth/ marker namespace) never drive Config.
	overlaySet := make(map[string]string, len(set))
	for k, v := range set {
		if rest, ok := strings.CutPrefix(k, config.OverlayRowPrefix); ok {
			overlaySet[rest] = v
		}
	}
	var overlayDel []string
	for _, k := range del {
		if rest, ok := strings.CutPrefix(k, config.OverlayRowPrefix); ok {
			overlayDel = append(overlayDel, rest)
		}
	}
	newCfg, err := config.ApplyOverlay(*oldCfg, overlaySet, overlayDel)
	if err != nil {
		return config.Config{}, err
	}
	if verify != nil {
		if err := verify(newCfg); err != nil {
			return config.Config{}, err
		}
	}
	a.enqueueSettingsSpill(set, del)
	return newCfg, nil
}

// tokenMarkerDelta maps a post-mutation AUTH_TOKENS list to its settings
// persist: marker set while pooled, and the config:AUTH_TOKENS overlay row
// converged to the same list. Every token path (add/remove/swap) funnels
// through here, so the overlay always carries the latest pool.
func tokenMarkerDelta(tokens []string) (set map[string]string, del []string) {
	set = map[string]string{
		config.OverlayRowKey("AUTH_TOKENS"): strings.Join(tokens, ","),
	}
	if len(tokens) > 0 {
		set[tokenMarkerKey] = "true"
		return set, nil
	}
	return set, []string{tokenMarkerKey}
}

// errAdminTokenOverridden is the divergence rejection when a password change
// cannot move the effective ADMIN_TOKEN (process env wins). The handler
// renders the full conflict text; the sentinel only threads the branch
// through overlayWrite's verify.
var errAdminTokenOverridden = errors.New("admin token overridden")

// errRequireLoginShadowed is the divergence rejection when a require-login
// toggle cannot move the effective DASHBOARD_REQUIRE_LOGIN. The handler
// renders the conflict; the sentinel only threads the branch.
var errRequireLoginShadowed = errors.New("require login shadowed")
