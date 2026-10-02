// router.go - named lane-choice routing policies with verbatim reasons.
//
// Precedent: llmrelay router/policies.go (fallback literal chain, cheapest
// blended-cost/unknown-last, fastest rolling-TTFT/cold-first, weighted
// sticky + fallback tail) with alias nesting/cycle rules, and openziti
// construction-time validateRoutes failing fast on dead rules. The default
// policy is fallback — today's strict index order, byte-identical — while
// cheapest/fastest are opt-in reorderings of the same order. Every decision
// carries a verbatim Reason (router-returns-chain): the walk logs why this
// lane order was chosen.
package pool

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// RoutePolicy names the lane-choice policy. Fallback is the default and
// preserves today's spill walk exactly; cheapest and fastest only reorder
// the same eligible order.
type RoutePolicy string

const (
	// RouteFallback walks the strict roster index order.
	RouteFallback RoutePolicy = "fallback"
	// RouteCheapest walks lowest blended cost first; lanes with unknown
	// cost sort last (never first on missing data).
	RouteCheapest RoutePolicy = "cheapest"
	// RouteFastest walks cold lanes (no TTFT samples) first, then lowest
	// rolling TTFT — a cold lane may be the fastest, and it must be
	// measured before it can rank.
	RouteFastest RoutePolicy = "fastest"
)

// LaneStat is one eligible lane's routing signal. Cost unknown means
// HasCost is false (not zero cost); TTFT unknown means Cold is true (not
// instant).
type LaneStat struct {
	Account int
	Cost    float64
	HasCost bool
	TTFT    time.Duration
	Cold    bool
}

// RoutingDecision is one reorder answer: the lane visit order plus the
// verbatim reason the walk logs.
type RoutingDecision struct {
	Policy RoutePolicy
	Order  []int
	Reason string
}

// Reorder applies policy to order given the lane stats. Lanes without
// usable stats rank with the unknown group (cheapest: unknown-last;
// fastest: after measured lanes), stably — so equal or unknown lanes keep
// strict index order and missing data can never promote a lane.
func (p RoutePolicy) Reorder(order []int, stats []LaneStat) RoutingDecision {
	switch p {
	case RouteCheapest:
		return reorderCheapest(order, stats)
	case RouteFastest:
		return reorderFastest(order, stats)
	default:
		out := append([]int(nil), order...)
		return RoutingDecision{Policy: RouteFallback, Order: out, Reason: "fallback: strict roster order"}
	}
}

func statByAccount(stats []LaneStat) map[int]LaneStat {
	m := make(map[int]LaneStat, len(stats))
	for _, s := range stats {
		if _, ok := m[s.Account]; !ok {
			m[s.Account] = s
		}
	}
	return m
}

// costRank maps a lane to its cheapest-sort key: known cost sorts by
// value, anything else (no stats, or stats without cost) ranks unknown
// and sorts last, stably.
func costRank(byAcct map[int]LaneStat, idx int) (known bool, cost float64) {
	s, ok := byAcct[idx]
	if !ok || !s.HasCost {
		return false, 0
	}
	return true, s.Cost
}

func reorderCheapest(order []int, stats []LaneStat) RoutingDecision {
	byAcct := statByAccount(stats)
	out := append([]int(nil), order...)
	sort.SliceStable(out, func(i, j int) bool {
		aknown, acost := costRank(byAcct, out[i])
		bknown, bcost := costRank(byAcct, out[j])
		if aknown != bknown {
			return aknown
		}
		if !aknown {
			return false
		}
		return acost < bcost
	})
	var chain []string
	for _, idx := range out {
		if s, ok := byAcct[idx]; ok && s.HasCost {
			chain = append(chain, fmt.Sprintf("%d(cost=%.4g)", idx, s.Cost))
		} else {
			chain = append(chain, fmt.Sprintf("%d(cost=unknown-last)", idx))
		}
	}
	return RoutingDecision{Policy: RouteCheapest, Order: out, Reason: "cheapest: " + strings.Join(chain, " → ")}
}

func reorderFastest(order []int, stats []LaneStat) RoutingDecision {
	byAcct := statByAccount(stats)
	out := append([]int(nil), order...)
	sort.SliceStable(out, func(i, j int) bool {
		a, aok := byAcct[out[i]]
		b, bok := byAcct[out[j]]
		if !aok && !bok {
			return false
		}
		if !aok {
			return false
		}
		if !bok {
			return true
		}
		if a.Cold != b.Cold {
			return a.Cold
		}
		return a.TTFT < b.TTFT
	})
	var chain []string
	for _, idx := range out {
		if s, ok := byAcct[idx]; ok {
			if s.Cold {
				chain = append(chain, fmt.Sprintf("%d(cold-first)", idx))
			} else {
				chain = append(chain, fmt.Sprintf("%d(ttft=%s)", idx, s.TTFT))
			}
		} else {
			chain = append(chain, fmt.Sprintf("%d(no-signal)", idx))
		}
	}
	return RoutingDecision{Policy: RouteFastest, Order: out, Reason: "fastest: " + strings.Join(chain, " → ")}
}

// CompileAliases resolves a virtual-model alias table to terminal model
// ids, following nesting. It rejects empty targets, self-aliases, and
// cycles at load (openziti validateRoutes discipline): a bad table fails
// here, never mid-walk. A nil/empty table compiles to nil (aliasing off,
// every model resolves to itself).
func CompileAliases(raw map[string]string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(raw))
	for name := range raw {
		seen := map[string]bool{name: true}
		target := raw[name]
		for {
			if target == "" {
				return nil, fmt.Errorf("pool: alias %q has an empty target", name)
			}
			next, aliased := raw[target]
			if !aliased {
				break
			}
			if seen[target] {
				return nil, fmt.Errorf("pool: alias cycle at %q", target)
			}
			seen[target] = true
			target = next
		}
		if target == name {
			return nil, fmt.Errorf("pool: alias %q resolves to itself", name)
		}
		out[name] = target
	}
	return out, nil
}

// ResolveAlias expands one model through the compiled table. Unaliased
// models (and a nil table) return unchanged.
func ResolveAlias(aliases map[string]string, model string) string {
	if target, ok := aliases[model]; ok {
		return target
	}
	return model
}
