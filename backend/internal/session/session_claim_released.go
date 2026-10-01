// session_claim_released.go — honest handling for a released purchase.
//
// A 409 purchase_claim_released whose post-rotation retry ALSO 409s (the
// token-1 deepseek wedge, live 2026-10-01: rotation fired per request —
// "purchase claim rotated reason=purchase_claim_released" — yet no session
// was ever adopted) means the purchase itself is gone upstream
// (released/refunded), not the claim: no fresh cli: id can admit, so the
// rotation budget is spent correctly and the honest 409 surfaces. This is
// account-side — operator action (a fresh purchase/login on that account).
//
// What this file adds (no rotation-contract change — claim-rotate-closed.md
// still owns that): the manager remembers the refusal per model for a brief
// TTL, so a hot loop of requests does not burn one rotation + two admission
// POSTs per attempt while the purchase is gone. Inside the TTL admissions
// for that model fail fast with the same honest 409 (pool fails over
// exactly as before, minus the upstream churn); the TTL expiry re-probes,
// so an operator repair is picked up with no restart. Releases are
// per-model: token-1 kept serving luna/mimo quota rows while only its
// deepseek purchase was refused.
package session

import (
	"errors"
	"freebuff-proxy/backend/internal/upstream"
	"net/http"
	"sort"
	"strings"
	"time"
)

// claimReleasedMarker matches the upstream refusal when the purchase behind
// a claim is released (status-form {"status":"purchase_claim_released"} via
// the refresh status branch, or error-form {"error":"purchase_claim_released"}
// which classifies to UpstreamError like admission_attempt_closed does).
const claimReleasedMarker = "purchase_claim_released"

// claimReleasedTTL bounds the per-model released-purchase memory: long
// enough to stop a hot request loop churning rotations + admission POSTs
// against a gone purchase, short enough that an operator repair (fresh
// purchase/login) is re-probed within minutes, no restart needed.
const claimReleasedTTL = 5 * time.Minute

// releasedClaimEntry is one remembered purchase_claim_released refusal:
// until is when the proxy stops failing fast and re-probes upstream.
type releasedClaimEntry struct {
	until time.Time
}

// isPurchaseClaimReleasedErr reports whether err is the error-form 409:
// the purchase behind the claim is released. Rotation stays on the
// status-form contract (refresh status branch) — this only feeds the
// fail-fast memory, never a rotation.
func isPurchaseClaimReleasedErr(err error) bool {
	var ue *upstream.UpstreamError
	if errors.As(err, &ue) {
		return strings.Contains(strings.ToLower(ue.Body), claimReleasedMarker)
	}
	return false
}

// recordClaimReleased remembers a purchase_claim_released refusal for model
// (the refresh-resolved target: "" default and named models key separately,
// so a released deepseek purchase never blocks luna/mimo admissions on the
// same token). Expired entries are pruned so the map stays bounded. Caller
// need not hold mu.
func (m *Manager) recordClaimReleased(model string) {
	now := m.now()
	m.mu.Lock()
	if m.claimReleased == nil {
		m.claimReleased = make(map[string]releasedClaimEntry)
	}
	m.claimReleased[model] = releasedClaimEntry{until: now.Add(claimReleasedTTL)}
	for k, e := range m.claimReleased {
		if !now.Before(e.until) {
			delete(m.claimReleased, k)
		}
	}
	m.mu.Unlock()
}

// claimReleasedShortCircuit fails fast when model is inside its remembered
// released window: the same honest 409 the live admission would return
// (statusError's purchase_claim_released shape), with zero upstream POSTs
// and zero claim churn, so the pool fails over exactly as before. ok is
// false when the model is not remembered or the window expired (the caller
// then admits live and re-probes). Caller need not hold mu.
func (m *Manager) claimReleasedShortCircuit(model string) (error, bool) {
	now := m.now()
	m.mu.Lock()
	e, ok := m.claimReleased[model]
	if !ok || !now.Before(e.until) {
		if ok {
			delete(m.claimReleased, model)
		}
		m.mu.Unlock()
		return nil, false
	}
	m.mu.Unlock()
	return &upstream.UpstreamError{
		Status: http.StatusConflict,
		Body:   "upstream purchase flow blocked admission (" + claimReleasedMarker + ")",
	}, true
}

// releasedModelsLocked lists the models inside their remembered released
// window, sorted, for the dashboard snapshot. Caller must hold mu.
func (m *Manager) releasedModelsLocked(now time.Time) []string {
	var out []string
	for model, e := range m.claimReleased {
		if now.Before(e.until) {
			out = append(out, model)
		}
	}
	sort.Strings(out)
	return out
}
