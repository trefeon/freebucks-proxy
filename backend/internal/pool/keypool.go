// keypool.go - multi-key rotation pool: per-(model, account) cooldown with
// round-robin draw skipping cooling keys.
//
// Precedent: llmrelay KeyPool (round-robin skipping cooldown, 30s default,
// nil for <2 keys) with 429 rotate-no-backoff. The draw cursor lives per
// model: consecutive Acquires for one model rotate across healthy accounts
// instead of hammering the head lane, while a 429 marks only the refusing
// (model, account) pair cooling — no backoff sleep, the next draw simply
// skips it. Pool exhaustion keeps today's 429/waiting-room semantics: the
// draw reports all-cooling and the walk tail surfaces the real refusal,
// never a new error shape.
package pool

import (
	"sync"
	"time"
)

// keyCooldownDefault is the fallback per-(model, account) cooldown when a
// refusal carries no Retry-After. Refusals with an explicit window use that
// window instead.
const keyCooldownDefault = 30 * time.Second

// KeyPool rotates draws across accounts per model, skipping lanes in
// their cooldown window. Zero value is usable (system clock).
type KeyPool struct {
	mu       sync.Mutex
	cursors  map[string]int
	cooldown map[keyCoolKey]time.Time
	now      func() time.Time
}

type keyCoolKey struct {
	model   string
	account int
}

func (k *KeyPool) clock() time.Time {
	if k.now != nil {
		return k.now()
	}
	return time.Now()
}

// NoteCooling parks one (model, account) lane until until. A 429 rotates
// without backoff: the lane is marked, not slept — the current draw simply
// moves on and a later draw re-admits the lane once the window lapses.
func (k *KeyPool) NoteCooling(model string, account int, until time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cooldown == nil {
		k.cooldown = make(map[keyCoolKey]time.Time)
	}
	k.cooldown[keyCoolKey{model: model, account: account}] = until
}

// NoteRateLimited parks one lane for d (zero/negative means the default).
// It is the 429 entry point: rotate-no-backoff, no sleep on the hot path.
func (k *KeyPool) NoteRateLimited(model string, account int, d time.Duration) {
	if d <= 0 {
		d = keyCooldownDefault
	}
	k.NoteCooling(model, account, k.clock().Add(d))
}

// Cooling reports whether the lane is still parked. Expired entries are
// dropped so the map cannot grow across long uptimes.
func (k *KeyPool) Cooling(model string, account int) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	until, ok := k.cooldown[keyCoolKey{model: model, account: account}]
	if !ok {
		return false
	}
	if !k.clock().Before(until) {
		delete(k.cooldown, keyCoolKey{model: model, account: account})
		return false
	}
	return true
}

// RotateOrder cursor-rotates an eligible order, filtering cooling lanes
// (expiry-purged). It returns nil when every lane cools: the caller keeps
// the strict order so exhaustion semantics stay unchanged. Successive
// calls rotate the head, spreading same-model turns across accounts.
func (k *KeyPool) RotateOrder(model string, order []int) []int {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.clock()
	var healthy []int
	for _, idx := range order {
		if until, ok := k.cooldown[keyCoolKey{model: model, account: idx}]; ok {
			if now.Before(until) {
				continue
			}
			delete(k.cooldown, keyCoolKey{model: model, account: idx})
		}
		healthy = append(healthy, idx)
	}
	if len(healthy) == 0 {
		return nil
	}
	if k.cursors == nil {
		k.cursors = make(map[string]int)
	}
	start := k.cursors[model] % len(healthy)
	out := make([]int, 0, len(healthy))
	out = append(out, healthy[start:]...)
	out = append(out, healthy[:start]...)
	k.cursors[model] = (start + 1) % len(healthy)
	return out
}

// Draw returns the account indexes of n lanes in round-robin order,
// skipping cooling lanes. See RotateOrder for the nil contract.
func (k *KeyPool) Draw(model string, n int) []int {
	if n <= 0 {
		return nil
	}
	seq := make([]int, n)
	for i := range seq {
		seq[i] = i
	}
	return k.RotateOrder(model, seq)
}
