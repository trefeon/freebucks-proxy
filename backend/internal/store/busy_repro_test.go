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

// newBusyStore opens a Store on a throwaway database file. The cleanup is
// deliberately tolerant: on Windows a scanner can hold a handle on the
// just-written WAL past Close, and t.TempDir's RemoveAll would then fail the
// test after its assertions already passed. Registered before the Close, so
// Close runs first (LIFO).
func newBusyStore(t *testing.T) *Store {
	t.Helper()
	dir, err := os.MkdirTemp("", "fb-busy-repro")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	s, err := Open(filepath.Join(dir, "busy.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
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
	s := newBusyStore(t)

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

// TestReadsDoNotQueueBehindWrites decides how wide the connection pool has to
// be. The busy policy rides in the DSN, so EVERY pooled connection carries
// busy_timeout and writers are serialized in-process by withWrite — one
// connection is not what makes the store correct. Capping the pool at one
// connection would however serialize the dashboard's reads behind any
// in-flight write (and an instant-save POST behind a dashboard query), which
// WAL explicitly allows us to avoid: readers and the writer do not block each
// other on separate connections.
//
// The writer holds its transaction open (that is what pins a connection) for
// a fixed wall-clock budget while the reader runs. A read that has to wait for
// the writer cannot beat the budget, and cannot even finish before the writer
// releases the connection.
func TestReadsDoNotQueueBehindWrites(t *testing.T) {
	s := newBusyStore(t)

	const (
		writeHold  = 700 * time.Millisecond
		readBudget = 200 * time.Millisecond
	)
	started := make(chan struct{})
	released := make(chan time.Time, 1)
	go func() {
		_ = s.withWrite(func() error {
			tx, err := s.db.Begin()
			if err != nil {
				return err
			}
			// Rollback after a successful commit is a no-op, and it keeps the
			// connection from being handed back mid-transaction on any error.
			defer func() { _ = tx.Rollback() }()
			deadline := time.Now().Add(writeHold)
			opened := false
			for time.Now().Before(deadline) {
				if _, err := tx.Exec(
					`INSERT INTO log_entries(ts, level, msg, fields, req_id)
					 SELECT ts, level, msg, fields, req_id FROM log_entries LIMIT 100`,
				); err != nil {
					return err
				}
				if !opened {
					close(started)
					opened = true
				}
			}
			return tx.Commit()
		})
		released <- time.Now()
	}()

	<-started
	readStart := time.Now()
	if _, err := s.ListSettings(); err != nil {
		t.Fatalf("ListSettings during an in-flight write: %v", err)
	}
	readEnd := time.Now()
	releasedAt := <-released

	if !readEnd.Before(releasedAt) {
		t.Fatalf("read (%v) finished only after the in-flight write released the connection (write held it %v): readers must not queue behind writers",
			readEnd.Sub(readStart), releasedAt.Sub(readStart))
	}
	if d := readEnd.Sub(readStart); d > readBudget {
		t.Fatalf("read took %v while a write ran for %v, want <%v: the pool serializes readers behind writers", d, writeHold, readBudget)
	} else {
		t.Logf("read completed in %v while a write held a connection for %v", d, writeHold)
	}
}
