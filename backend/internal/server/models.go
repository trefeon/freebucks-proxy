package server

import (
	"encoding/json"
	"fmt"
	"freebucks-proxy/backend/internal/modelcat"
	"freebucks-proxy/backend/internal/pool"
	"freebucks-proxy/backend/internal/registry"
	"freebucks-proxy/backend/internal/upstream"
	"net/http"
)

// ModelUnavailableMessage formats the rejection error message for
// unserved/disabled models (issue #189). Withdrawn-but-recognized upstream ids
// (modelcat.PausedMap, e.g. minimax/minimax-m3) get upstream's own
// withdrawn-model copy naming the replacement (freebuffWithdrawnModelMessage)
// instead of the generic supported-list dump: the client that sends that id is
// a released binary whose picker still lists it, and "not available" alone
// leaves the user staring at a row that looks fine and does not work.
func ModelUnavailableMessage(rawModel string) string {
	if modelcat.IsPaused(rawModel) {
		return modelcat.WithdrawnModelMessage(rawModel)
	}
	return fmt.Sprintf("Model '%s' is not available. Supported models: %s", rawModel, modelcat.ServedHelpText())
}

// modelRefusalMessage is the admission-gate refusal copy, shared by every
// refusal site so the chat, responses and messages surfaces cannot drift.
// Withdrawn rows keep upstream's own copy byte-identical (the released
// client that sends one is why that copy exists); a tier-gated row names the
// missing entitlement instead of the supported-list dump, which names none
// of the ids the caller asked about. model is the alias/suffix-resolved id
// the tier lookups need; rawModel is what the caller sent and what the copy
// quotes.
func (s *Server) modelRefusalMessage(rawModel, model string) string {
	if modelcat.IsPaused(rawModel) || modelcat.IsPaused(model) {
		return modelcat.WithdrawnModelMessage(rawModel)
	}
	switch modelTierReason(model, s.pool.Snapshot()) {
	case statusPlanRequired:
		return fmt.Sprintf("Model '%s' requires a paid Freebuff plan; this account has no active subscription.", rawModel)
	case statusOfferGone:
		return fmt.Sprintf("Model '%s' is a capacity-limited trial that is not being offered right now.", rawModel)
	case statusTrialUsed:
		return fmt.Sprintf("Model '%s' is a capacity-limited trial and this account has already used its allowance.", rawModel)
	}
	return ModelUnavailableMessage(rawModel)
}

// servedModels returns the registry catalog filtered to the ServedModels
// gate: the ids this gateway actually serves (blocked -max variants and any
// future non-gated registry row excluded). Used for the /v1/models
// surface-equivalent counts (/healthz, /metrics) and the "available:" model
// hints in error bodies, so nothing advertises an id that 404s.
func (s *Server) servedModels() []string {
	all := s.reg.Models()
	out := make([]string, 0, len(all))
	for _, id := range all {
		if modelcat.IsServed(id) {
			out = append(out, id)
		}
	}
	return out
}

// servedModelCount reports how many registry models pass the ServedModels
// gate. Mirrors the /v1/models row count (modulo ModelsHideUnavailable).
func (s *Server) servedModelCount() int {
	return len(s.servedModels())
}

// probeModel returns a default model for smoke-test paths: the cheapest
// served 0-Freebucks row in the registry (the model every account gets)
// when present, else the catalog default (modelcat.DefaultModelID, the
// picker lead the upstream CLI resolves a blank pick to), else the first
// SERVED row in catalog order. Never alphabetical models[0] alone: that
// would pick anthropic/claude-fable-5.1, a capacity-gated offer model that
// makes smoke tests fail on most accounts. The served gating means probes
// never target an id the gateway itself would refuse.
func probeModel(reg *registry.Registry) string {
	models := reg.Models()
	if len(models) == 0 {
		return ""
	}
	if m := modelcat.CheapestFreeIn(models, nil, false); m != "" {
		return m
	}
	for _, id := range models {
		if id == modelcat.DefaultModelID && modelcat.IsServed(id) {
			return id
		}
	}
	registered := make(map[string]struct{}, len(models))
	for _, id := range models {
		registered[id] = struct{}{}
	}
	for i := range modelcat.Catalog {
		id := modelcat.Catalog[i].ID
		if _, ok := registered[id]; !ok {
			continue
		}
		if modelcat.IsServed(id) {
			return id
		}
	}
	return ""
}

// modelAdmissible reports whether a model may be served: the hardcoded
// ServedModels gate, else the pool-wide tier entitlement. Served rows return
// without touching the pool — this runs per chat request and per /v1/models
// row — and a row no tier carries (withdrawn, god-only, -max variants) can
// never be admitted, so only a tier row pays for the snapshot.
func (s *Server) modelAdmissible(id string) bool {
	if modelcat.IsServed(id) {
		return true
	}
	if len(modelcat.Tiers(id)) == 0 {
		return false
	}
	return modelTierAdmits(id, s.pool.Snapshot())
}

// modelTierAdmits is the pure tier decision for id against one snapshot
// view: true when ANY tier the row carries is satisfied. The aggregation is
// pool-wide and optimistic, exactly like currentAccessTier's region_limited
// signal (any token satisfying a branch admits the row); per-token tier
// routing is deliberately NOT decided here.
//
//   - offer: some token is advertising this row's capacity-limited offer
//     with capacity left and an unspent personal allowance.
//   - paid: some token reports a subscription tier id (a paid plan).
//   - full: the pool's effective access tier is full or free.
//   - limited: the free limited catalog needs no pool signal (the same
//     membership isModelAllowedForTier admits on the limited tier).
func modelTierAdmits(id string, snaps []pool.TokenSnapshot) bool {
	if modelcat.HasTier(id, modelcat.TierOffer) {
		if o, ok := offerFor(id, snaps); ok && offerJoinable(o) {
			return true
		}
	}
	if modelcat.HasTier(id, modelcat.TierPaid) {
		for _, snap := range snaps {
			if snap.SubscriptionTierID != "" {
				return true
			}
		}
	}
	if modelcat.HasTier(id, modelcat.TierFull) {
		switch currentAccessTier(snaps) {
		case "full", "free":
			return true
		}
	}
	return modelcat.HasTier(id, modelcat.TierLimited)
}

// offerFor returns the capacity-limited offer a token is currently
// advertising for id, first match in snapshot order. The offer block is a
// last-seen stash, so presence in the snapshot IS the "advertised right now"
// signal: the vendor drops the whole block when the wave closes or its pool
// is spent (freebuff-session.ts:837-839), and the picker renders nothing
// without it (freebuff-model-selector.tsx:322-338).
func offerFor(id string, snaps []pool.TokenSnapshot) (upstream.LimitedModelOffer, bool) {
	for _, snap := range snaps {
		for _, o := range snap.LimitedModelOffers {
			if o.Model == id {
				return o, true
			}
		}
	}
	return upstream.LimitedModelOffer{}, false
}

// offerJoinable mirrors the vendor's join rule (freebuff-session.ts:676-680):
// a row is joinable only while both the shared pool (remaining) and the
// caller's own allowance (userRemaining) are non-zero. The picker's row gate
// reads userRemaining alone (freebuff-model-selector.tsx:543-544), so the two
// only agree while the global pool has capacity.
func offerJoinable(o upstream.LimitedModelOffer) bool {
	return o.Joinable() && o.Remaining > 0
}

// modelAllowed reports whether a model may be served: the served-or-tier
// admission decision, then MODELS_ALLOW (when non-empty) as an exact-match
// allowlist over the well-known (alias/suffix-resolved) model id.
func (s *Server) modelAllowed(model string) bool {
	if !s.modelAdmissible(model) {
		return false
	}
	return allowedByList(model, s.cfg.Load().ModelsAllow)
}

// modelListed is the /v1/models row filter: every row the catalog surface
// carries, then MODELS_ALLOW. Looser than modelAllowed on purpose — the
// listing shows a tier row the pool cannot admit right now WITH the reason
// (plan_required, offer_unavailable, trial_used), so a picker can render it
// honestly instead of showing nothing; admission stays strict.
func (s *Server) modelListed(model string) bool {
	if !modelListable(model) {
		return false
	}
	return allowedByList(model, s.cfg.Load().ModelsAllow)
}

// modelListable reports whether id belongs on the /v1/models surface: the
// served ids plus every row that carries a tier. Withdrawn rows are NOT
// listed — the wire catalog never advertises an id the gateway would refuse
// (they are refused with upstream's withdrawn copy, and the dashboard is
// where an operator sees them) — and god-only, eval, -max and other unserved
// tierless rows stay invisible too.
func modelListable(id string) bool {
	return modelcat.IsServed(id) || len(modelcat.Tiers(id)) > 0
}

// allowedByList applies the MODELS_ALLOW allowlist (empty = open).
func allowedByList(model string, allow []string) bool {
	if len(allow) == 0 {
		return true
	}
	for _, id := range allow {
		if id == model {
			return true
		}
	}
	return false
}

// Tier-reason status vocabulary shared by the /v1/models annotation and the
// refusal copy, so the picker and the error body can never disagree about
// why a row cannot run. Every member is reachable from a LISTED row (a
// withdrawn row is never listed: it is refused with upstream's withdrawn
// copy and stays visible to the operator through the dashboard). The
// served/quota/lock statuses stay inline in modelAvailability where they
// already lived.
const (
	statusPlanRequired = "plan_required"
	statusOfferGone    = "offer_unavailable"
	statusTrialUsed    = "trial_used"
)

// modelTierReason names why id cannot run right now (plan_required,
// offer_unavailable, trial_used), or "" when it is served, admitted by a
// tier, or carries no tier at all (withdrawn, god-only, eval, -max and other
// unserved tierless rows keep the advisory default; neither surface lists
// them, and a paused id is refused earlier with the withdrawn copy).
func modelTierReason(id string, snaps []pool.TokenSnapshot) string {
	if modelcat.IsServed(id) || len(modelcat.Tiers(id)) == 0 {
		return ""
	}
	if modelTierAdmits(id, snaps) {
		return ""
	}
	switch {
	case modelcat.HasTier(id, modelcat.TierPaid):
		return statusPlanRequired
	case modelcat.HasTier(id, modelcat.TierOffer):
		if o, ok := offerFor(id, snaps); ok && o.Remaining > 0 {
			// The wave is still advertising capacity; the personal
			// allowance is what ran out (userRemaining == 0).
			return statusTrialUsed
		}
		return statusOfferGone
	}
	return ""
}

// handleModels serves the OpenAI model-list shape with the registry's
// current models; created is pinned to server start so every entry matches.
// Each row carries an advisory availability annotation derived from the pool
// token snapshots (available/status) so clients can surface quota or lock
// signals without probing, plus the additive tier fields (tiers, and the
// live offer block on an offer row). A Codex client (request carrying a
// client_version query param) instead gets strict ModelInfo rows under the
// {"models": […]} envelope — see codexClientVersion/codexModelRow and
// WIRE-NOTES.md §8.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	created := s.started.Unix()
	snaps := s.pool.Snapshot()
	models := s.reg.Models()
	if len(models) == 0 {
		// T16: an empty registry is an operational anomaly (the fallback
		// table should always populate at boot) — surface it when a client
		// actually asks, not at startup.
		s.logger.Warn("model list requested with empty registry", "path", r.URL.Path, "remote", remoteHost(r), "model_count", 0)
	}
	codexReq := codexClientVersion(r)
	hideUnavailable := s.cfg.Load().ModelsHideUnavailable
	tier := currentAccessTier(snaps)
	data := make([]map[string]any, 0, len(models))
	var runnable []string
	for _, id := range models {
		available, status := modelAvailability(id, snaps)
		if hideUnavailable && !available {
			// MODELS_HIDE_UNAVAILABLE=true: prune quota/lock/tier-unavailable
			// models so picker clients never auto-select one. Off by default
			// because a stale signal could hide a working model.
			continue
		}
		if !s.modelListed(id) {
			// MODELS_ALLOW: prune ids outside the operator allowlist so
			// picker clients never auto-select a model that would 404. Uses
			// the strict list (base ids only), so PREFER_MAX_MODELS -max
			// variants stay invisible on the catalog surface.
			continue
		}
		data = append(data, modelRow(id, id, created, available, status, tier, snaps))
		if codexReq && (modelcat.IsServed(id) || modelTierAdmits(id, snaps)) {
			runnable = append(runnable, id)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if codexReq {
		// Codex deserializes strict ModelInfo rows under the {"models": […]}
		// envelope (codex-rs codex-api/src/endpoint/models.rs:70 with the
		// protocol ModelsResponse wrapper, protocol/src/openai_models.rs:745-
		// 750); the {"object":"list","data":…} OpenAI shape errors on the
		// missing `models` field and codex silently falls back to its bundled
		// catalog (reference/agents/codex/WIRE-NOTES.md §8).
		//
		// Only the admitted ids: codex renders these rows as its own picker,
		// and the strict row shape has no field to say "listed but not
		// runnable" (no tiers/status), so a tier-gated or withdrawn row would
		// be selectable and then refused at admission.
		rows := make([]codexModelInfo, 0, len(runnable))
		for _, id := range runnable {
			rows = append(rows, codexModelRow(id))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": rows})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// modelRow builds one /v1/models entry; the list and retrieve surfaces share
// it so their shapes cannot drift. id is the id the caller asked for
// (retrieve echoes its path) and resolved the catalog id the tier lookups
// use; available/status are precomputed because the list surface prunes on
// them before building the row.
func modelRow(id, resolved string, created int64, available bool, status string, tier string, snaps []pool.TokenSnapshot) map[string]any {
	row := map[string]any{
		"id":        id,
		"object":    "model",
		"created":   created,
		"owned_by":  "freebuff",
		"available": available,
		"status":    status,
	}
	if tier != "" {
		row["current_access_tier"] = tier
	}
	if tiers := modelcat.Tiers(resolved); len(tiers) > 0 {
		row["tiers"] = tiers
	}
	// Only an advertised offer carries the block: a client that sees the
	// field has a live offer to join, and one that does not must not render a
	// stale row (the vendor drops the block, not just the counters).
	if modelcat.HasTier(resolved, modelcat.TierOffer) {
		if offer, ok := offerFor(resolved, snaps); ok {
			row["offer"] = map[string]any{
				"remaining":      offer.Remaining,
				"total":          offer.Total,
				"user_remaining": offer.UserRemaining,
				"joinable":       offerJoinable(offer),
			}
		}
	}
	return row
}

// currentAccessTier resolves the effective access tier across the pool snapshots.
// Returns "full", "limited", "free", or "" if no token has reported an access tier yet.
func currentAccessTier(snaps []pool.TokenSnapshot) string {
	hasLimited := false
	for _, snap := range snaps {
		switch snap.AccessTier {
		case "full":
			return "full"
		case "free":
			return "free"
		case "limited":
			hasLimited = true
		}
	}
	if hasLimited {
		return "limited"
	}
	return ""
}

// isModelAllowedForTier reports whether id can be served under tier.
// On limited tier, only limited-tier models (mimo-v2.5) or GLM 5.2 with active referral quota
// can be admitted.
func isModelAllowedForTier(id, tier string, snaps []pool.TokenSnapshot) bool {
	if tier != "limited" {
		return true
	}
	if modelcat.IsLimitedTierAllowed(id) {
		return true
	}
	if id == modelcat.Glm52ModelID {
		for _, snap := range snaps {
			if q, ok := snap.QuotaByModel[id]; ok && q.Limit > 0 && q.RecentCount < q.Limit {
				return true
			}
		}
	}
	return false
}

// modelAvailability derives the advisory per-model annotation from the pool
// token snapshots. The snapshot does not carry the model of a live session,
// so the signal set is: the tier reason the row cannot run right now
// (missing paid plan, offer wave closed, personal trial spent), accessTier
// (limited tier marks non-mimo models as region_limited), quotaByModel
// presence (the session admitted this model), quota exhaustion
// (recent >= limit), and session-level locks.
// available defaults to true when no signal exists, so a working model is
// never hidden.
func modelAvailability(id string, snaps []pool.TokenSnapshot) (available bool, status string) {
	// Tier reasons come first: they name why the row cannot run at all,
	// while quota/lock only describe a row that would otherwise be admitted.
	if reason := modelTierReason(id, snaps); reason != "" {
		return false, reason
	}
	available = true
	status = "unknown"
	quotaHit := false
	quotaExhausted := false
	locked := false
	tier := currentAccessTier(snaps)
	for _, snap := range snaps {
		switch snap.SessionStatus {
		case "model_locked", "disabled":
			locked = true
		}
		if q, ok := snap.QuotaByModel[id]; ok {
			quotaHit = true
			if q.Limit > 0 && q.RecentCount >= q.Limit {
				quotaExhausted = true
			}
		}
	}
	switch {
	case tier == "limited" && !isModelAllowedForTier(id, tier, snaps):
		available = false
		status = "region_limited"
	case quotaExhausted:
		status = "quota_exhausted"
	case locked:
		status = "locked"
	case quotaHit:
		status = "available"
	}
	return available, status
}
