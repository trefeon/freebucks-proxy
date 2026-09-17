package store

// Instant-save write-path stress (−race). The dashboard persists one settings
// row per edit burst while background writers keep the same SQLite file busy:
// the log spill (AppendLogs txn), retention (Purge txn), pool persistence
// (SavePoolState), and the quota/request recorders — none of which take the
// server's adminSaveMu. The bug this file pins: Open armed busy_timeout on the
// single pooled connection that ran the pragma, so any write landing on a
// freshly opened pool member failed fast with SQLITE_BUSY ("database is locked
// (5)") and the settings POST answered 500 persist_failed for a value the
// operator had just typed.
//
// The assertions are the observable contract of that path, not its internals:
// no call may lose a write-lock race, no row may be lost to a concurrent purge,
// no goroutine may outlive the handle, the slow tail must stay below the busy
// timeout, and the close must not leave WAL debt behind. Determinism comes from
// WaitGroup-joined workers plus bounded deadline guards — never from sleeps that
// hope the race happened to land.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// stressBusyTimeout mirrors the PRAGMA busy_timeout Open configures (see
// store.go): the write path must stay far below it, which is only possible when
// the busy handler is armed on every connection pool member instead of just the
// one connection that executed the pragma.
const stressBusyTimeout = 5 * time.Second

// stressDeadline bounds a whole concurrency exercise. Without it a deadlocked
// writer surfaces as the package-level `go test` timeout, which names no test
// and dumps no stack; with it the guard fails in seconds and points here.
const stressDeadline = 60 * time.Second

// stressBusyMarkers are the fragments SQLite and the driver use for a write that lost
// the race for the WAL write lock. Matching on text (not just errno) is
// deliberate: the driver wraps the code, and the string form is what reached the
// operator's dashboard as the persist_failed message.
var stressBusyMarkers = []string{"database is locked", "SQLITE_BUSY", "(5)"}

// stressIsBusyErr reports whether err is a lost write-lock race.
func stressIsBusyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range stressBusyMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// stressCall records one timed store call: the latency the operator waits on and
// the error that decides between "saved" and "persist_failed".
type stressCall struct {
	op  string
	dur time.Duration
	err error
}

// stressCallLog collects timed calls from worker goroutines. Each worker fills its own
// slice and appends once under the mutex, so the timed section itself never
// contends on shared state (which would make the latency numbers meaningless).
type stressCallLog struct {
	mu    sync.Mutex
	calls []stressCall
}

func (c *stressCallLog) add(calls []stressCall) {
	c.mu.Lock()
	c.calls = append(c.calls, calls...)
	c.mu.Unlock()
}

func (c *stressCallLog) snapshot() []stressCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]stressCall, len(c.calls))
	copy(out, c.calls)
	return out
}

// stressRunBounded runs a blocking section in its own goroutine and fails the test if
// it has not returned by deadline. This is the deadlock guard: a writer parked
// forever on a lock nobody releases fails here instead of hanging the suite.
func stressRunBounded(t *testing.T, deadline time.Duration, work func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		work()
	}()
	select {
	case <-done:
	case <-time.After(deadline):
		t.Fatalf("stress work still running after %v (deadlock guard)", deadline)
	}
}

// stressWaitSettled polls fn until it reports true or the deadline passes. Bounded
// polling for a condition, not a racing sleep: the goroutine counters it guards
// move at their own pace (finalizers, pool teardown), so the assertion is "no
// permanent leak", which is what the observer of a leak can actually check.
func stressWaitSettled(deadline time.Duration, fn func() bool) bool {
	end := time.Now().Add(deadline)
	for {
		if fn() {
			return true
		}
		if time.Now().After(end) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// stressAssertWritePathHealth pins the properties the busy bug violated: no lost
// write-lock race, no other failure on the timed path, and a p99 below the busy
// timeout. Sorted-duration percentiles on the fully joined call set (no sampling
// under load), so the numbers reported in the failure message are reproducible
// for the same workload shape.
func stressAssertWritePathHealth(t *testing.T, calls []stressCall, elapsed time.Duration) {
	t.Helper()
	if len(calls) == 0 {
		t.Fatal("no calls recorded — the stress workload did not run")
	}
	var busy, other []error
	durs := make([]time.Duration, 0, len(calls))
	for _, c := range calls {
		durs = append(durs, c.dur)
		switch {
		case stressIsBusyErr(c.err):
			busy = append(busy, c.err)
		case c.err != nil:
			other = append(other, fmt.Errorf("%s: %w", c.op, c.err))
		}
	}
	if len(busy) > 0 {
		t.Errorf("%d/%d calls lost the write-lock race (want 0): first = %v [elapsed %v]",
			len(busy), len(calls), busy[0], elapsed.Round(time.Millisecond))
	}
	if len(other) > 0 {
		t.Errorf("%d/%d calls failed with a non-BUSY error (want 0): first = %v",
			len(other), len(calls), other[0])
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	p50 := durs[len(durs)/2]
	p99 := durs[len(durs)*99/100]
	t.Logf("write path: calls=%d elapsed=%v p50=%v p99=%v max=%v",
		len(calls), elapsed.Round(time.Millisecond), p50.Round(time.Microsecond),
		p99.Round(time.Millisecond), durs[len(durs)-1].Round(time.Millisecond))
	if p99 >= stressBusyTimeout {
		t.Errorf("write p99 = %v, want < %v (busy_timeout): the tail is parking on the WAL write lock",
			p99, stressBusyTimeout)
	}
}

// TestConcurrentSetSettingStress drives the instant-save path itself: many
// goroutines writing a mix of config and dashboard keys, exactly the shape of a
// burst of dashboard edits (each row auto-POSTs) racing a second browser tab.
// Every write is a single-statement UPSERT, so the only failure mode available
// is the lost write-lock race this file exists to catch.
func TestConcurrentSetSettingStress(t *testing.T) {
	s := openTest(t)

	const (
		writers = 48
		iters   = 25
	)
	keys := []string{
		"config:QUEUE_WAIT",
		"config:QUEUE_DEPTH",
		"config:SLOTS_PER_ACCOUNT",
		"config:MAX_SPILL_ACCOUNTS",
		"config:MATURITY_ENABLED",
		"config:LOG_LEVEL",
		"ui:active-tab",
		"ui:log-filter",
	}

	var log stressCallLog
	var wg sync.WaitGroup
	began := time.Now()
	t.Logf("stress: goroutines=%d ops=%d keys=%d", writers, writers*iters, len(keys))
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			local := make([]stressCall, 0, iters)
			for i := range iters {
				key := keys[(w+i)%len(keys)]
				val := fmt.Sprintf("%d:%d", w, i)
				start := time.Now()
				err := s.SetSetting(key, val)
				local = append(local, stressCall{op: "SetSetting", dur: time.Since(start), err: err})
			}
			log.add(local)
		}(w)
	}
	stressRunBounded(t, stressDeadline, wg.Wait)
	elapsed := time.Since(began)

	calls := log.snapshot()
	if len(calls) != writers*iters {
		t.Errorf("recorded %d calls, want %d (a worker exited early)", len(calls), writers*iters)
	}
	stressAssertWritePathHealth(t, calls, elapsed)

	// Every key must be readable afterwards, and a concurrent scan must survive
	// the burst: the settings console re-reads the whole overlay right after a
	// save to render the source badge.
	all, err := s.ListSettings()
	if err != nil {
		t.Fatalf("ListSettings after burst: %v", err)
	}
	for _, key := range keys {
		if _, ok := all[key]; !ok {
			t.Errorf("setting %q missing after %d concurrent writes", key, writers*iters)
		}
	}
}

// TestConcurrentBackgroundWritersStress runs the write paths that bypass the
// admin save mutex against each other: batched log spills (transactional), log
// scans, retention purges (transactional), pool-state upserts, and the
// quota/request recorders. This is the production interleaving that produced
// "database is locked (5)" for the settings POST — a background transaction
// holding the WAL write lock while the operator's save arrives.
//
// The volume is deliberately modest while every writer family stays in play:
// each log-spill batch is a real WAL transaction, so a few dozen of them under
// -race already cost seconds on slow storage, and the p99 assertion must measure
// a save that is queued, not how long the queue is — a healthy implementation
// clears the bound by an order of magnitude, and the volume that trips the bug
// needs only >2 concurrent writers, which this shape keeps.
func TestConcurrentBackgroundWritersStress(t *testing.T) {
	s := openTest(t)

	const (
		logWriters  = 6
		logBatches  = 4
		batchSize   = 25
		scanners    = 4
		scans       = 25
		purgers     = 1
		purges      = 5
		recorders   = 4
		records     = 6
		baseTSMilli = 1_700_000_000_000
	)
	wantLogRows := logWriters * logBatches * batchSize

	var log stressCallLog
	var wg sync.WaitGroup
	began := time.Now()
	t.Logf("stress: goroutines=%d (log spill %d x %d batches of %d, scanners %d x %d, purges %d x %d, recorders %d x %d)",
		logWriters+scanners+purgers+recorders,
		logWriters, logBatches, batchSize, scanners, scans, purgers, purges, recorders, records)

	// Log spill: one transaction per batch, the ring's shutdown/overflow path.
	for w := range logWriters {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			local := make([]stressCall, 0, logBatches)
			entries := make([]LogEntry, batchSize)
			for b := range logBatches {
				for i := range entries {
					entries[i] = LogEntry{
						TS:     int64(baseTSMilli + w*logBatches*batchSize + b*batchSize + i),
						Level:  "INFO",
						Msg:    "stress spill",
						Fields: fmt.Sprintf("writer=%d batch=%d", w, b),
						ReqID:  fmt.Sprintf("r-%d-%d-%d", w, b, i),
					}
				}
				start := time.Now()
				err := s.AppendLogs(entries)
				local = append(local, stressCall{op: "AppendLogs", dur: time.Since(start), err: err})
			}
			log.add(local)
		}(w)
	}

	// Concurrent readers: the Logs console and the settings GET path scanning
	// while the file is under write pressure.
	for range scanners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]stressCall, 0, scans)
			for range scans {
				start := time.Now()
				_, err := s.ListSettings()
				if err == nil {
					_, err = s.QueryLogs(LogFilter{Limit: 100})
				}
				local = append(local, stressCall{op: "scan", dur: time.Since(start), err: err})
			}
			log.add(local)
		}()
	}

	// Retention: cutoff 0 means "nothing is older than the epoch", so the
	// transaction deletes no rows but still takes the write lock — the exact
	// window a concurrent writer must queue behind instead of failing.
	for range purgers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]stressCall, 0, purges)
			for range purges {
				start := time.Now()
				err := s.Purge(0, 0, 0, 0)
				local = append(local, stressCall{op: "Purge", dur: time.Since(start), err: err})
			}
			log.add(local)
		}()
	}

	// Pool state + recorders: the other unsynchronized writers on the file.
	for c := range recorders {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			local := make([]stressCall, 0, records*3)
			for i := range records {
				start := time.Now()
				err := s.SavePoolState(fmt.Sprintf("stress:%d", c), []byte(fmt.Sprintf("v%d", i)))
				local = append(local, stressCall{op: "SavePoolState", dur: time.Since(start), err: err})

				start = time.Now()
				if err == nil {
					err = s.RecordQuota(QuotaSnapshot{
						TS: int64(baseTSMilli + i), TokenIdx: c, Model: "stress/model",
						Limit: 100, Recent: float64(i), ResetAt: int64(baseTSMilli + 60_000),
					})
				}
				local = append(local, stressCall{op: "RecordQuota", dur: time.Since(start), err: err})

				start = time.Now()
				if err == nil {
					err = s.RecordRequest(RequestRecord{
						ReqID: fmt.Sprintf("req-%d-%d", c, i), TS: int64(baseTSMilli + i),
						Endpoint: "/v1/chat/completions", Model: "stress/model",
						TokenIdx: c, Status: "200", TTFBms: int64(i),
					})
				}
				local = append(local, stressCall{op: "RecordRequest", dur: time.Since(start), err: err})
			}
			log.add(local)
		}(c)
	}

	stressRunBounded(t, stressDeadline, wg.Wait)
	elapsed := time.Since(began)
	stressAssertWritePathHealth(t, log.snapshot(), elapsed)

	// No row may be lost to the concurrent purge (its cutoff predates every
	// row): a batched spill that fails partway would silently drop logs, and
	// the whole history view would degrade without saying so.
	rows, err := s.QueryLogs(LogFilter{Limit: maxLogLimit})
	if err != nil {
		t.Fatalf("QueryLogs after stress: %v", err)
	}
	if len(rows) != wantLogRows {
		t.Errorf("log rows = %d, want %d (rows lost under concurrent writes)", len(rows), wantLogRows)
	}
}

// TestTwoHandlesShareOneBusyPolicy pins the half of the fix that an in-process
// write mutex cannot cover. A second Store on the same file — the CLI, another
// gateway process, a backup tool — writes through its own connections, so the
// only thing between a cross-handle collision and "database is locked (5)" is
// the busy handler armed on EVERY connection. A fix that serializes writers
// inside one Store but leaves the pragma on whichever pooled connection ran it
// passes the single-handle tests and fails here, which is what makes this the
// regression guard for the pragma itself rather than for the serializer.
func TestTwoHandlesShareOneBusyPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("Open first: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := Open(path)
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	const (
		writers = 12
		iters   = 10
	)
	var log stressCallLog
	var wg sync.WaitGroup
	began := time.Now()
	t.Logf("stress: goroutines=%d ops=%d handles=2", writers, writers*iters)
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			// Alternate handles so the collisions are cross-handle by
			// construction: same-process hits never contend on one mutex.
			handle, other := first, second
			if w%2 == 1 {
				handle, other = second, first
			}
			local := make([]stressCall, 0, iters)
			for i := range iters {
				key := fmt.Sprintf("config:QUEUE_WAIT%d", w%2)
				val := fmt.Sprintf("handle:%d:%d", w, i)
				start := time.Now()
				err := handle.SetSetting(key, val)
				local = append(local, stressCall{op: "SetSetting", dur: time.Since(start), err: err})
				// Interleave reads on the other handle: the file must stay
				// readable for the dashboard while both writers collide.
				start = time.Now()
				if err == nil {
					_, _, err = other.GetSetting(key)
				}
				local = append(local, stressCall{op: "GetSetting", dur: time.Since(start), err: err})
			}
			log.add(local)
		}(w)
	}
	stressRunBounded(t, stressDeadline, wg.Wait)
	stressAssertWritePathHealth(t, log.snapshot(), time.Since(began))
}

// TestStressLeavesNoGoroutineOrWALDebt checks the two cleanup properties the
// same burst must preserve: no goroutine outlives the handle, and the WAL does
// not grow without bound while the handle is open nor survive its close (a
// leftover multi-megabyte -wal is uncheckpointed debt a crash would replay).
func TestStressLeavesNoGoroutineOrWALDebt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stress.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	baseline := runtime.NumGoroutine()

	const (
		writers = 32
		iters   = 20
	)
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range iters {
				_ = s.SetSetting("config:QUEUE_WAIT", fmt.Sprintf("%ds", 20+i))
				_ = s.AppendLogs([]LogEntry{{
					TS: int64(1_700_000_000_000 + w*iters + i), Level: "INFO",
					Msg: "wal debt probe", Fields: "k=v", ReqID: fmt.Sprintf("wal-%d-%d", w, i),
				}})
			}
		}(w)
	}
	stressRunBounded(t, stressDeadline, wg.Wait)

	// Auto-checkpoint keeps the WAL near the 1000-page threshold (~4 MiB); the
	// ceiling leaves headroom for the burst without letting an un-checkpointed
	// log of arbitrary size pass as healthy.
	const walCeiling = 32 << 20
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() > walCeiling {
		t.Errorf("WAL grew to %d bytes (> %d) during the burst", fi.Size(), walCeiling)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() != 0 {
		t.Errorf("WAL left %d bytes behind after Close, want checkpointed/removed", fi.Size())
	}

	if !stressWaitSettled(5*time.Second, func() bool { return runtime.NumGoroutine() <= baseline+2 }) {
		t.Errorf("goroutine leak: %d live, baseline %d (stress workers never exited)",
			runtime.NumGoroutine(), baseline)
	}
}

// TestBusyErrClassifierKeepsItsTeeth guards the classifier the two stress tests
// lean on: if it stopped recognising the driver's wrapped form, every BUSY would
// be misfiled as "non-BUSY error" and the assertions would keep failing for the
// wrong reason (or worse, pass on an unrelated failure).
func TestBusyErrClassifierKeepsItsTeeth(t *testing.T) {
	busy := []error{
		errors.New("store: set setting \"config:QUEUE_WAIT\": database is locked (5) (SQLITE_BUSY)"),
		errors.New("store: logs commit: SQLITE_BUSY: database is locked"),
		fmt.Errorf("store: purge request_records: %w", errors.New("database is locked (5) (SQLITE_BUSY)")),
	}
	for _, err := range busy {
		if !stressIsBusyErr(err) {
			t.Errorf("stressIsBusyErr(%v) = false, want true", err)
		}
	}
	for _, err := range []error{
		nil,
		errors.New("store: set setting \"x\": constraint failed: UNIQUE constraint failed: settings.key"),
		errors.New("store: list settings: no such table: settings"),
	} {
		if stressIsBusyErr(err) {
			t.Errorf("stressIsBusyErr(%v) = true, want false", err)
		}
	}
}
