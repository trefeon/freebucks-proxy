// gateway.go - universal-gateway Phase 2 pool integration: breaker gate,
// key rotation, routing policies, and alias expansion.
//
// All four machines default to today's behavior — closed breakers, strict
// index order, fallback policy, no aliases — so green paths are unchanged:
// failover and reordering only engage after failures (breaker/keypool) or
// explicit opt-in (SetRoutePolicy/SetAliases). Pool isolation per account
// holds: every machine keys by (model, account) and never moves state
// across accounts.
package pool

import (
	"freebuff-proxy/backend/internal/upstream"
	"time"
)

// gatewayState carries the Phase-2 routing machines. It lives behind gwMu;
// the Acquire hot path takes at most one lock per call (gateway snapshot)
// and never blocks on stats (the stats func must be non-blocking and may
// return nil).
type gatewayState struct {
	breakers *BreakerRegistry
	keys     *KeyPool
	policy   RoutePolicy
	statsFn  func(model string) []LaneStat
	aliases  map[string]string
}

// gateway snapshots the routing configuration for one Acquire.
func (p *Pool) gateway() *gatewayState {
	p.gwMu.Lock()
	defer p.gwMu.Unlock()
	if p.gw == nil {
		p.gw = &gatewayState{
			breakers: NewBreakerRegistry(),
			keys:     &KeyPool{},
			policy:   RouteFallback,
		}
	}
	return p.gw
}

// breakerReg returns the lane breaker registry (lazy: zero-value test
// pools still gate correctly).
func (p *Pool) breakerReg() *BreakerRegistry {
	return p.gateway().breakers
}

// keyRotation returns the multi-key rotation pool.
func (p *Pool) keyRotation() *KeyPool {
	return p.gateway().keys
}

// SetRoutePolicy opts the pool into a named lane-choice policy
// (RouteFallback default; RouteCheapest/RouteFastest opt-in). It only
// reorders the eligible order — eligibility, buckets, and tail precedence
// are unchanged.
func (p *Pool) SetRoutePolicy(pol RoutePolicy) {
	p.gwMu.Lock()
	defer p.gwMu.Unlock()
	if p.gw == nil {
		p.gw = &gatewayState{breakers: NewBreakerRegistry(), keys: &KeyPool{}}
	}
	if pol == "" {
		pol = RouteFallback
	}
	p.gw.policy = pol
}

// SetRouteStats installs the lane-signal source the cheapest/fastest
// policies rank on. Nil restores no-signal ranking (unknown-last, stable).
// The func must be non-blocking; it runs on the Acquire path.
func (p *Pool) SetRouteStats(fn func(model string) []LaneStat) {
	p.gwMu.Lock()
	defer p.gwMu.Unlock()
	if p.gw == nil {
		p.gw = &gatewayState{breakers: NewBreakerRegistry(), keys: &KeyPool{}}
	}
	p.gw.statsFn = fn
}

// SetAliases installs the virtual-model alias table, compiled (nested,
// cycle-rejected) at load. A bad table fails here and leaves the previous
// table in place — never mid-walk.
func (p *Pool) SetAliases(raw map[string]string) error {
	compiled, err := CompileAliases(raw)
	if err != nil {
		return err
	}
	p.gwMu.Lock()
	defer p.gwMu.Unlock()
	if p.gw == nil {
		p.gw = &gatewayState{breakers: NewBreakerRegistry(), keys: &KeyPool{}}
	}
	p.gw.aliases = compiled
	return nil
}

// resolveModel expands one model through the alias table (nil table =
// identity).
func (p *Pool) resolveModel(model string) string {
	p.gwMu.Lock()
	defer p.gwMu.Unlock()
	if p.gw == nil {
		return model
	}
	return ResolveAlias(p.gw.aliases, model)
}

// routeOrder applies the lane-choice policy to the eligible order.
// Fallback returns the order untouched (today's spill walk, no log line);
// other policies log their verbatim reason. A nil reorder (all lanes
// cooling under rotation) keeps the strict order so exhaustion semantics
// stay today's 429/waiting-room shapes.
func (p *Pool) routeOrder(model string, order []int) []int {
	g := p.gateway()
	if g.policy == "" || g.policy == RouteFallback {
		return order
	}
	var stats []LaneStat
	if g.statsFn != nil {
		stats = g.statsFn(model)
	}
	if len(stats) == 0 {
		out := g.keys.RotateOrder(model, order)
		if out == nil {
			return order
		}
		p.logger.Debug("pool: routing decision", "model", model,
			"policy", string(g.policy), "reason", "rotation: round-robin skipping cooling lanes (no signals)")
		return out
	}
	d := g.policy.Reorder(order, stats)
	p.logger.Debug("pool: routing decision", "model", model,
		"policy", string(d.Policy), "reason", d.Reason)
	return d.Order
}

// breakerOpen reports whether the (model, account) lane's breaker rejects
// attempts right now.
func (p *Pool) breakerOpen(model string, account int) bool {
	return !p.breakerReg().For(model, account).Allow()
}

// noteLaneSuccess closes the lane's breaker after a granted lease.
func (p *Pool) noteLaneSuccess(model string, account int) {
	p.breakerReg().For(model, account).RecordSuccess()
}

// noteLaneRateLimit records one lane refusal on both failover machines:
// the breaker counts a consecutive failure, and the rotation pool parks
// the (model, account) lane for the refusal window (429 rotates without
// backoff — the current draw moves on, no sleep). RetryAfter <= 0 takes
// the default window.
func (p *Pool) noteLaneRateLimit(tok *tokenEntry, model string, rle *upstream.RateLimitError) {
	if tok == nil || rle == nil {
		return
	}
	idx := p.indexOfEntry(tok)
	if idx < 0 {
		return
	}
	p.breakerReg().For(model, idx).RecordFailure()
	retryAfter := rle.RetryAfter
	if retryAfter <= 0 && !rle.ResetAt.IsZero() {
		retryAfter = time.Until(rle.ResetAt)
	}
	p.keyRotation().NoteRateLimited(model, idx, retryAfter)
}
