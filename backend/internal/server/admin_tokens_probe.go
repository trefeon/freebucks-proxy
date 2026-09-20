package server

import (
	"context"
	"errors"
	"fmt"
	"freebucks-proxy/backend/internal/upstream"
	"net/http"
	"strconv"
	"time"
)

func (a *adminHandlers) handleTokenTest(w http.ResponseWriter, r *http.Request) {
	id, err := tokenActionID(r)
	var state *upstream.SessionState
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		state, err = a.pool.ProbeToken(ctx, id)
	}
	if err != nil {
		if errors.Is(err, upstream.ErrNoActiveSession) {
			a.logfunc().Info("dashboard token probe ok (no active session)", "token", id)
			a.dash.RenderConfigResult(w, r, true, "Token "+strconv.Itoa(id)+" OK — zero-cost probe succeeded (no active session).")
			return
		}
		a.logfunc().Warn("dashboard token probe failed", "token", id, "err", err)
		a.dash.RenderConfigResult(w, r, false, "Token "+strconv.Itoa(id)+" test failed: "+err.Error())
		return
	}
	msg := "Token " + strconv.Itoa(id) + " OK — zero-cost probe succeeded"
	if q := quotaSummary(state); q != "" {
		msg += " (" + q + ")"
	}
	msg += "."
	a.logfunc().Info("dashboard token probe ok", "token", id)
	a.dash.RenderConfigResult(w, r, true, msg)
}

// handleTokensTestAll probes every pool token with the zero-cost detailed
// probe and renders the frozen pool.ProbeTokenOutcome slice as ONE JSON
// array (200). Zero-cost: ProbeAllTokens claims no session and touches no
// model (bounded concurrency 4, 15s per-token timeout inside the pool).
func (a *adminHandlers) handleTokensTestAll(w http.ResponseWriter, r *http.Request) {
	outcomes, err := a.pool.ProbeAllTokens(r.Context())
	if err != nil {
		a.logfunc().Warn("dashboard tokens probe-all failed", "err", err)
		a.dash.RenderConfigResult(w, r, false, "Tokens test-all failed: "+err.Error())
		return
	}
	a.logfunc().Info("dashboard tokens probe-all ok", "tokens", len(outcomes))
	a.dash.RenderProbeAllResults(w, r, outcomes)
}

func (a *adminHandlers) probeTokenGate(ctx context.Context, token string) (*upstream.SessionState, error) {
	state, err := a.pool.ProbeNewToken(ctx, token)
	if err != nil {
		if errors.Is(err, upstream.ErrNoActiveSession) {
			// No active session is fine: the pool will create one on first
			// use. Treat as usable.
			return state, nil
		}
		return nil, err
	}
	if state != nil {
		switch state.Status {
		case "banned":
			return nil, fmt.Errorf("token is banned upstream (status banned): %w", upstream.ErrBanned)
		case "country_blocked":
			return nil, fmt.Errorf("token is country-blocked upstream: %w", upstream.ErrCountryBlocked)
		}
	}
	return state, nil
}
