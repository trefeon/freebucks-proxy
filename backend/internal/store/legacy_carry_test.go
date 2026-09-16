package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestImportLegacyCarryRequiresOnePinnedConnection forces the pool to hand
// every separate Exec its own physical connection by refusing to retain idle
// connections (SetMaxIdleConns(-1)). That removes exactly the incidental
// connection reuse the attach/carry/detach sequence has always silently
// depended on: SQLite ATTACH is a per-connection property, so an ATTACH on
// one connection is invisible to an INSERT..SELECT issued on another, and
// database/sql guarantees no affinity between separate Exec calls.
//
// The carry must therefore still complete — it can only do so if the whole
// sequence runs on ONE pinned connection.
func TestImportLegacyCarryRequiresOnePinnedConnection(t *testing.T) {
	newPath := filepath.Join(t.TempDir(), "new.db")
	oldPath := filepath.Join(t.TempDir(), "old.db")
	seedLegacyHistory(t, oldPath)

	s, err := Open(newPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = s.Close() }()
	// No idle connection is ever retained, so no Exec can reuse the
	// connection that ran the ATTACH.
	s.db.SetMaxIdleConns(-1)

	n, err := ImportLegacyHistoryDB(s, newPath, oldPath)
	if err != nil {
		t.Fatalf("carry with no idle connection reuse: %v", err)
	}
	if n != 4 {
		t.Fatalf("carried rows = %d, want 4 (one per history table)", n)
	}
	var logs int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM log_entries").Scan(&logs); err != nil {
		t.Fatalf("count target log_entries: %v", err)
	}
	if logs != 1 {
		t.Fatalf("target log_entries = %d, want 1", logs)
	}
}

// TestImportLegacyCarryDetachesOnCarryError pins the release guarantee: a
// carry that fails midway must DETACH the legacy database on the pinned
// connection before that connection returns to the pool. The pool is capped
// at one connection here so the observation is deterministic — the PRAGMA
// below necessarily inspects the very connection the import used. A legacy
// file missing its first carry table fails the carry immediately, and an
// import that got ATTACHed is still expected to leave nothing attached (the
// detached state is what a pool-reusing store observes; closing the pinned
// connection is the backstop for a connection that died instead).
func TestImportLegacyCarryDetachesOnCarryError(t *testing.T) {
	newPath := filepath.Join(t.TempDir(), "new.db")
	oldPath := filepath.Join(t.TempDir(), "old.db")
	seedLegacyHistory(t, oldPath)
	old, err := Open(oldPath)
	if err != nil {
		t.Fatalf("reopen legacy: %v", err)
	}
	if _, err := old.db.Exec("DROP TABLE log_entries"); err != nil {
		t.Fatalf("drop legacy log_entries: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close legacy: %v", err)
	}

	s, err := Open(newPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = s.Close() }()
	s.db.SetMaxOpenConns(1)

	if _, err := ImportLegacyHistoryDB(s, newPath, oldPath); err == nil {
		t.Fatal("carry from a legacy file without log_entries: want error, got nil")
	} else if !strings.Contains(err.Error(), "carry log_entries") {
		t.Fatalf("carry error = %v, want it to name the failing table", err)
	}

	for _, name := range attachedDatabases(t, s) {
		if name == "legacy" {
			t.Fatal("legacy database still attached after a failed carry")
		}
	}
}

// attachedDatabases lists every database attached to the store's connection
// (PRAGMA database_list returns seq, name, file).
func attachedDatabases(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query("PRAGMA database_list")
	if err != nil {
		t.Fatalf("pragma database_list: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			t.Fatalf("scan database_list: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("database_list rows: %v", err)
	}
	return names
}
