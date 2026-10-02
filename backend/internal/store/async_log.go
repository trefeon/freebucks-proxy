// async_log.go - async stats recorder (universal-gateway Phase 2.6).
//
// The request hot path must never block on stats writes: AsyncRecorder
// takes a sink (usually Store.RecordRequest) and drains it on a
// background goroutine. Enqueue is non-blocking — a full queue drops with
// the drop counter, and the drop only skips a redundant write, never
// request handling. Dropped() exposes the count for /metrics and the
// dashboard honesty line. Close drains what fits, then stops.
//
// Cost honesty: request_records carries no cost column, so every row is
// unpriced by schema — spend stays in the pool ledger (pool/spend.go) and
// these rollups never zero-fill a price. See activity.go.
package store

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// AsyncRecorder is a drop-counted async sink for request records.
type AsyncRecorder struct {
	sink    func(RequestRecord) error
	ch      chan RequestRecord
	dropped atomic.Int64
	wg      sync.WaitGroup
	mu      sync.Mutex
	closed  bool
}

// NewAsyncRecorder starts a recorder with room for cap pending records.
// cap <= 0 means 64. sink runs on the drain goroutine; sink errors are
// swallowed (stats must never fail a turn — the drop counter only counts
// queue-full drops, matching the pool spill discipline).
func NewAsyncRecorder(cap int, sink func(RequestRecord) error) *AsyncRecorder {
	if cap <= 0 {
		cap = 64
	}
	a := &AsyncRecorder{sink: sink, ch: make(chan RequestRecord, cap)}
	a.wg.Add(1)
	go a.drain()
	return a
}

func (a *AsyncRecorder) drain() {
	defer a.wg.Done()
	for rec := range a.ch {
		if a.sink != nil {
			_ = a.sink(rec)
		}
	}
}

// EnableAsyncRecord routes RecordRequest through a drop-counted async
// recorder with room for cap pending records. Default is synchronous
// writes; enabling never changes the row shape, only the write timing.
// Re-enabling drains and replaces the previous recorder outside the lock.
func (s *Store) EnableAsyncRecord(cap int) {
	s.asyncMu.Lock()
	old := s.asyncRec
	s.asyncRec = nil
	s.asyncMu.Unlock()
	if old != nil {
		old.Close()
	}
	rec := NewAsyncRecorder(cap, s.RecordRequestSync)
	s.asyncMu.Lock()
	s.asyncRec = rec
	s.asyncMu.Unlock()
}

// RecordRequestSync is the synchronous request-record write (the
// pre-async path, also the async sink — it bypasses the recorder so the
// drain cannot loop back into the queue).
func (s *Store) RecordRequestSync(rec RequestRecord) error {
	if rec.ReqID == "" {
		return nil
	}
	noteHistoryWrite()
	return s.withWrite(func() error {
		_, err := s.db.Exec(
			`INSERT OR REPLACE INTO request_records(req_id, ts, endpoint, model, token_idx, status, ttfb_ms, error, client_key_hash)
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.ReqID, rec.TS, rec.Endpoint, rec.Model, rec.TokenIdx, rec.Status, rec.TTFBms, rec.Err, rec.ClientKeyHash,
		)
		if err != nil {
			return fmt.Errorf("store: request insert: %w", err)
		}
		return nil
	})
}

// DisableAsyncRecord drains the recorder and restores synchronous
// writes. AsyncDropped retains the count until the next Enable.
func (s *Store) DisableAsyncRecord() {
	s.asyncMu.Lock()
	a := s.asyncRec
	s.asyncRec = nil
	s.asyncMu.Unlock()
	if a != nil {
		a.Close()
	}
}

// AsyncDropped reports the async recorder's drop count (0 when async is
// off and never enabled).
func (s *Store) AsyncDropped() int64 {
	s.asyncMu.Lock()
	defer s.asyncMu.Unlock()
	if s.asyncRec == nil {
		return 0
	}
	return s.asyncRec.Dropped()
}

// Record enqueues one record without blocking. It reports false (and
// counts a drop) when the queue is full or the recorder is closed.
func (a *AsyncRecorder) Record(rec RequestRecord) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		a.dropped.Add(1)
		return false
	}
	select {
	case a.ch <- rec:
		return true
	default:
		a.dropped.Add(1)
		return false
	}
}

// Dropped reports how many records the full queue (or a closed recorder)
// shed. Monotonic.
func (a *AsyncRecorder) Dropped() int64 {
	return a.dropped.Load()
}

// Close stops the recorder after draining what fits. Records after Close
// count as drops.
func (a *AsyncRecorder) Close() {
	a.mu.Lock()
	if !a.closed {
		a.closed = true
		close(a.ch)
	}
	a.mu.Unlock()
	a.wg.Wait()
}
