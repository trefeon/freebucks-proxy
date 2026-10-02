package pool

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFallbackReorderIsIdentity(t *testing.T) {
	order := []int{0, 1, 2}
	stats := []LaneStat{{Account: 2, Cost: 0.1, HasCost: true}}
	d := RouteFallback.Reorder(order, stats)
	if !slices.Equal(d.Order, order) {
		t.Errorf("fallback Order = %v, want identity %v", d.Order, order)
	}
	if d.Policy != RouteFallback || d.Reason == "" {
		t.Errorf("fallback decision = %+v, want policy + verbatim reason", d)
	}
	// Unknown policy also falls back (never a new error shape mid-walk).
	d2 := RoutePolicy("bogus").Reorder(order, stats)
	if !slices.Equal(d2.Order, order) {
		t.Errorf("unknown policy Order = %v, want identity", d2.Order)
	}
}

func TestCheapestRanksLowestFirstUnknownLast(t *testing.T) {
	stats := []LaneStat{
		{Account: 0, Cost: 5.0, HasCost: true},
		{Account: 1, Cost: 1.0, HasCost: true},
		{Account: 2}, // unknown cost
	}
	d := RouteCheapest.Reorder([]int{0, 1, 2}, stats)
	if !slices.Equal(d.Order, []int{1, 0, 2}) {
		t.Errorf("cheapest Order = %v, want [1 0 2] (lowest first, unknown last)", d.Order)
	}
	if !strings.HasPrefix(d.Reason, "cheapest: ") || !strings.Contains(d.Reason, "unknown-last") {
		t.Errorf("cheapest Reason = %q, want verbatim chain with unknown-last", d.Reason)
	}
}

func TestCheapestStableOnTies(t *testing.T) {
	stats := []LaneStat{
		{Account: 0, Cost: 2.0, HasCost: true},
		{Account: 1, Cost: 2.0, HasCost: true},
	}
	d := RouteCheapest.Reorder([]int{1, 0}, stats)
	if !slices.Equal(d.Order, []int{1, 0}) {
		t.Errorf("cheapest tie Order = %v, want input order preserved (stable)", d.Order)
	}
}

func TestCheapestMissingStatsRankUnknown(t *testing.T) {
	stats := []LaneStat{{Account: 1, Cost: 1.0, HasCost: true}}
	d := RouteCheapest.Reorder([]int{0, 1, 2}, stats)
	if !slices.Equal(d.Order, []int{1, 0, 2}) {
		t.Errorf("cheapest Order = %v, want [1 0 2] (missing stats rank unknown, stably)", d.Order)
	}
}

func TestFastestColdFirstThenTTFT(t *testing.T) {
	stats := []LaneStat{
		{Account: 0, TTFT: 100 * time.Millisecond},
		{Account: 1, TTFT: 50 * time.Millisecond},
		{Account: 2, Cold: true},
	}
	d := RouteFastest.Reorder([]int{0, 1, 2}, stats)
	if !slices.Equal(d.Order, []int{2, 1, 0}) {
		t.Errorf("fastest Order = %v, want [2 1 0] (cold first, then TTFT asc)", d.Order)
	}
	if !strings.HasPrefix(d.Reason, "fastest: ") || !strings.Contains(d.Reason, "cold-first") {
		t.Errorf("fastest Reason = %q, want verbatim chain with cold-first", d.Reason)
	}
}

func TestCompileAliasesNested(t *testing.T) {
	aliases, err := CompileAliases(map[string]string{"fast": "cheap", "cheap": "model-x"})
	if err != nil {
		t.Fatalf("CompileAliases: %v", err)
	}
	if got := ResolveAlias(aliases, "fast"); got != "model-x" {
		t.Errorf("ResolveAlias(fast) = %q, want model-x (nested)", got)
	}
	if got := ResolveAlias(aliases, "model-x"); got != "model-x" {
		t.Errorf("ResolveAlias(terminal) = %q, want itself", got)
	}
	if got := ResolveAlias(nil, "model-x"); got != "model-x" {
		t.Errorf("ResolveAlias(nil table) = %q, want itself", got)
	}
}

func TestCompileAliasesRejects(t *testing.T) {
	for name, raw := range map[string]map[string]string{
		"cycle":     {"a": "b", "b": "a"},
		"self":      {"a": "a"},
		"empty":     {"a": ""},
		"longcycle": {"a": "b", "b": "c", "c": "a"},
	} {
		if _, err := CompileAliases(raw); err == nil {
			t.Errorf("CompileAliases(%s) succeeded, want cycle/empty rejection at load", name)
		}
	}
	if _, err := CompileAliases(nil); err != nil {
		t.Errorf("CompileAliases(nil) = %v, want nil table (aliasing off)", err)
	}
}
