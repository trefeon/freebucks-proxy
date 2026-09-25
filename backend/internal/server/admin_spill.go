package server

import (
	"freebucks-proxy/backend/internal/store"
	"log/slog"
	"sync"
	"sync/atomic"
)

// Settings spill (unified-store, config plane): mem-swap first (instant,
// in-request), WAL write behind. Every dashboard mutation derives the new
// snapshot from mem via config.ApplyOverlay, swaps it synchronously via
// applyReloadedConfig, and enqueues the overlay delta here — never a
// synchronous store write on the request path (I2).
//
// Control state must not drop: unlike the history spill (display data,
// drop-on-full with a counter), the settings channel blocks the enqueuer
// when full (backpressure, 1024 deep — practically never) so a password or
// token write is never silently lost. A failed WAL apply only warns: the
// handler already returned the applied receipt, and the next mutation or
// restart converges (I3).
//
// Ops carry their own store handle (tests attach the store post-boot via
// attachShadowStore); the loop itself is store-agnostic.

// settingsSpillBuf is the enqueue depth shared with the recorded history
// spill pattern (dashboard_history.go spillBufSize).
const settingsSpillBuf = 1024

// settingsSpillOp is one persisted delta: full settings-table row keys
// (config.OverlayRowKey(...) or the auth/ marker namespace) to set/delete.
// A barrier op carries no payload; the loop closes barrier once every
// preceding op applied, so flush is exact without timing.
type settingsSpillOp struct {
	st      *store.Store
	set     map[string]string
	del     []string
	barrier chan struct{}
}

// settingsSpill is the background WAL drain. Zero value unusable; use
// newSettingsSpill.
type settingsSpill struct {
	ch   chan settingsSpillOp
	wg   sync.WaitGroup
	log  func() *slog.Logger
	gate atomic.Pointer[chan struct{}]

	mu     sync.Mutex
	closed bool

	applied atomic.Int64
	failed  atomic.Int64
}

func newSettingsSpill(log func() *slog.Logger) *settingsSpill {
	sp := &settingsSpill{ch: make(chan settingsSpillOp, settingsSpillBuf), log: log}
	sp.wg.Add(1)
	go sp.run()
	return sp
}

func (sp *settingsSpill) logger() *slog.Logger {
	if sp.log == nil {
		return slog.Default()
	}
	if l := sp.log(); l != nil {
		return l
	}
	return slog.Default()
}

// enqueue persists op behind the WAL drain. It blocks when the buffer is
// full (control state never drops) and no-ops after stop (warns: that path
// means a mutation raced server shutdown).
func (sp *settingsSpill) enqueue(op settingsSpillOp) {
	set := make(map[string]string, len(op.set))
	for k, v := range op.set {
		set[k] = v
	}
	op.set = set
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.closed {
		sp.logger().Warn("settings spill closed; dropping overlay write", "rows", len(set)+len(op.del))
		return
	}
	sp.ch <- op
}

func (sp *settingsSpill) run() {
	defer sp.wg.Done()
	for op := range sp.ch {
		if op.barrier != nil {
			close(op.barrier)
			continue
		}
		if g := sp.gate.Load(); g != nil {
			<-(*g)
		}
		sp.apply(op)
	}
}

// apply writes one delta to its store, key by key, best-effort: a failed
// key warns (with its row count) while the rest still land, and the failed
// counter stays observable. Callers must not hold adminSaveMu here —
// withWrite serializes writers itself.
func (sp *settingsSpill) apply(op settingsSpillOp) {
	defer sp.applied.Add(1)
	if op.st == nil {
		return
	}
	for k, v := range op.set {
		if err := op.st.SetSetting(k, v); err != nil {
			sp.failed.Add(1)
			sp.logger().Warn("settings spill set failed", "key", k, "err", err)
		}
	}
	for _, k := range op.del {
		if err := op.st.DeleteSetting(k); err != nil {
			sp.failed.Add(1)
			sp.logger().Warn("settings spill delete failed", "key", k, "err", err)
		}
	}
}

// pauseSpill blocks payload applies behind a gate (tests: the I2 proof
// mutates while paused and asserts the store is untouched — the handler
// performed zero synchronous writes). The barrier still flows while
// paused, so flush-while-paused would deadlock: always resume first.
// It returns the resume func.
func (a *adminHandlers) pauseSpill() func() {
	ch := make(chan struct{})
	sp := a.spill()
	sp.gate.Store(&ch)
	return func() { close(ch) }
}

// flush blocks until every op enqueued before it applied (barrier, no
// timing). A paused gate does not block the barrier itself — it only holds
// payload applies — so tests can assert the pre-drain absence first.
func (sp *settingsSpill) flush() {
	b := make(chan struct{})
	sp.mu.Lock()
	if sp.closed {
		sp.mu.Unlock()
		return
	}
	sp.ch <- settingsSpillOp{barrier: b}
	sp.mu.Unlock()
	<-b
}

// stop drains the queue, stops the loop, and waits for the in-flight apply.
// Later enqueues warn-and-drop.
func (sp *settingsSpill) stop() {
	sp.mu.Lock()
	sp.closed = true
	sp.mu.Unlock()
	close(sp.ch)
	sp.wg.Wait()
}

// spill lazily starts the loop (store-agnostic: ops carry their handle, so
// post-boot store attaches are picked up per mutation, never cached).
func (a *adminHandlers) spill() *settingsSpill {
	a.spillMu.Lock()
	defer a.spillMu.Unlock()
	if a.spillState == nil {
		a.spillState = newSettingsSpill(a.logfunc)
	}
	return a.spillState
}

// enqueueSettingsSpill persists the overlay delta behind the WAL drain. A
// nil store (live-only boot) skips the persist: the mem swap the caller
// already did is the whole write, lost on restart.
func (a *adminHandlers) enqueueSettingsSpill(set map[string]string, del []string) {
	if a.settings == nil {
		return
	}
	a.spill().enqueue(settingsSpillOp{st: a.settings, set: set, del: del})
}

// flushSettingsSpill drains the queue (tests + shutdown). No spill started
// (or no store) is a no-op.
func (a *adminHandlers) flushSettingsSpill() {
	a.spillMu.Lock()
	sp := a.spillState
	a.spillMu.Unlock()
	if sp == nil {
		return
	}
	sp.flush()
}

// closeSettingsSpill drains and stops the loop. Safe without a started
// spill; idempotent via the nil-out.
func (a *adminHandlers) closeSettingsSpill() {
	a.spillMu.Lock()
	sp := a.spillState
	a.spillState = nil
	a.spillMu.Unlock()
	if sp == nil {
		return
	}
	sp.stop()
}
