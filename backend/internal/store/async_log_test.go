package store

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestAsyncRecorderDelivers(t *testing.T) {
	var got atomic.Int64
	a := NewAsyncRecorder(16, func(RequestRecord) error { got.Add(1); return nil })
	t.Cleanup(a.Close)
	for range 5 {
		if !a.Record(RequestRecord{ReqID: "r"}) {
			t.Fatal("Record dropped on an empty queue, want delivered")
		}
	}
	a.Close()
	if got.Load() != 5 {
		t.Errorf("delivered = %d, want 5 (drain-then-stop)", got.Load())
	}
	if a.Dropped() != 0 {
		t.Errorf("dropped = %d, want 0", a.Dropped())
	}
}

func TestAsyncRecorderDropsWhenFull(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	a := NewAsyncRecorder(2, func(RequestRecord) error {
		once.Do(func() { close(entered) })
		<-release
		return nil
	})
	t.Cleanup(func() { close(release); a.Close() })
	// First record enters the sink and blocks it; the next two fill the
	// cap-2 queue; the rest must drop, never block.
	if !a.Record(RequestRecord{ReqID: "block"}) {
		t.Fatal("first Record dropped, want accepted")
	}
	<-entered
	if !a.Record(RequestRecord{ReqID: "q1"}) || !a.Record(RequestRecord{ReqID: "q2"}) {
		t.Fatal("queue fill dropped, want accepted (cap 2)")
	}
	drops := 0
	for range 10 {
		if !a.Record(RequestRecord{ReqID: "x"}) {
			drops++
		}
	}
	if drops == 0 {
		t.Error("no drops on a full queue, want drop-counted (hot path never blocks)")
	}
	if a.Dropped() != int64(drops) {
		t.Errorf("Dropped() = %d, want %d", a.Dropped(), drops)
	}
}

func TestAsyncRecorderClosedDrops(t *testing.T) {
	a := NewAsyncRecorder(4, nil)
	a.Close()
	if a.Record(RequestRecord{ReqID: "late"}) {
		t.Error("Record after Close accepted, want drop")
	}
	if a.Dropped() != 1 {
		t.Errorf("Dropped() = %d, want 1", a.Dropped())
	}
}

func TestStoreAsyncRecordEndToEnd(t *testing.T) {
	s := openTest(t)
	t.Cleanup(func() { _ = s.Close() })
	s.EnableAsyncRecord(16)
	rec := RequestRecord{ReqID: "async-1", TS: 1_000, Endpoint: "/v1/chat/completions", Model: "m/a", TokenIdx: 0, Status: "ok"}
	if err := s.RecordRequest(rec); err != nil {
		t.Fatalf("async RecordRequest: %v", err)
	}
	s.DisableAsyncRecord() // drains
	got, err := s.RequestsRollup(0, 2_000, 1_000)
	if err != nil {
		t.Fatalf("RequestsRollup: %v", err)
	}
	if got.Total != 1 {
		t.Errorf("rollup total = %d, want 1 (async write landed)", got.Total)
	}
	// Disabled means synchronous again: the row lands before return.
	if err := s.RecordRequest(RequestRecord{ReqID: "sync-1", TS: 1_500, Endpoint: "/v1/chat/completions", Model: "m/a", TokenIdx: 0, Status: "ok"}); err != nil {
		t.Fatalf("sync RecordRequest: %v", err)
	}
	got, err = s.RequestsRollup(0, 2_000, 1_000)
	if err != nil {
		t.Fatalf("RequestsRollup: %v", err)
	}
	if got.Total != 2 {
		t.Errorf("rollup total = %d, want 2", got.Total)
	}
}
