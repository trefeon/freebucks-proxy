package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// isBusyErr reports whether err is SQLite's fail-fast lock error. The
// reproducer counts these separately from other failures so a BUSY storm is
// reported as such instead of as a generic error.
func isBusyErr(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "SQLITE_LOCKED")
}

// TestConcurrentWritersShareOneBusyPolicy reproduces the instant-save
// SQLITE_BUSY report: every writer the gateway has (the settings UPSERT, the
// logring spill transaction, the retention purge and the request/quota
// recorders) shares one *sql.DB, so under concurrent writes the pool grew
// extra connections — and only the first pool connection carried the
// one-shot busy_timeout pragma, leaving every later connection to fail fast
// with SQLITE_BUSY instead of waiting for the writer in front of it.
//
// The contract proven here: concurrent writers on one Store never surface a
// lock error. A read-only ListSettings runs alongside (the dashboard path) to
// keep the mixed read/write pattern of the real workload.
func TestConcurrentWritersShareOneBusyPolicy(t *testing.T) {
	dir, err := os.MkdirTemp("", "fb-busy-repro")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	// Tolerant cleanup on purpose: on Windows a scanner can hold a handle on
	// the just-written WAL past Close, and t.TempDir's RemoveAll would then
	// fail the test after the assertions already passed. Registered before
	// the Close below, so Close runs first (LIFO).
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	s, err := Open(filepath.Join(dir, "busy.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const (
		writers = 8
		rounds  = 25
	)
	var wg sync.WaitGroup
	errs := make(chan error, writers*rounds*3)
	start := make(chan struct{})
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < rounds; i++ {
				key := fmt.Sprintf("config:RACE_%d_%d", w, i)
				if err := s.SetSetting(key, "30s"); err != nil {
					errs <- fmt.Errorf("SetSetting: %w", err)
				}
				if err := s.AppendLogs([]LogEntry{{
					TS:    Millis(time.Now()),
					Level: "info",
					Msg:   fmt.Sprintf("busy repro %d %d", w, i),
				}}); err != nil {
					errs <- fmt.Errorf("AppendLogs: %w", err)
				}
				if _, err := s.ListSettings(); err != nil {
					errs <- fmt.Errorf("ListSettings: %w", err)
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()
	close(errs)

	busy, other := 0, 0
	var firstBusy error
	for err := range errs {
		if isBusyErr(err) {
			busy++
			if firstBusy == nil {
				firstBusy = err
			}
			continue
		}
		other++
		t.Errorf("unexpected writer error: %v", err)
	}
	if busy > 0 {
		t.Fatalf("%d SQLITE_BUSY/LOCKED errors from concurrent writers (first: %v): every connection must carry the busy policy, not just the first one", busy, firstBusy)
	}
	if other > 0 {
		t.Fatalf("%d non-lock writer errors", other)
	}
}
