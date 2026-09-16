package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// Synthetic fixtures only — every value below is an obviously-fake
// test string. No real credential may ever appear in code or tests.
const (
	synthPersistToken = "synthetic-test-token-AAA"
	synthHashA        = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	synthHashB        = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// seedLegacyPersisted builds a legacy-shaped dashboard DB holding one row
// per persist table: three overlay knobs (one credential-shaped value to
// prove opaque secret carry) plus the env-to-DB marker row, one page
// snapshot, one session, one token maturity row and one pool blob.
func seedLegacyPersisted(t *testing.T, path string) {
	t.Helper()
	old, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	if err := old.SetSetting("config:SAFE_MODE", "true"); err != nil {
		t.Fatal(err)
	}
	if err := old.SetSetting("config:QUEUE_WAIT", "60s"); err != nil {
		t.Fatal(err)
	}
	if err := old.SetSetting("config:AUTH_TOKENS", synthPersistToken); err != nil {
		t.Fatal(err)
	}
	// config:migrated_env_v1 marker literal (config.MigrationMarkerRow; the
	// store stays a leaf package so the test names it literally): carrying
	// it keeps the env-to-DB import a no-op after the migration.
	if err := old.SetSetting("config:migrated_env_v1", "1"); err != nil {
		t.Fatal(err)
	}
	if err := old.PutPageState("overview", `{"scroll":12}`); err != nil {
		t.Fatal(err)
	}
	if err := old.SaveSession(synthHashA, `{"s":1}`, `{"r":1}`); err != nil {
		t.Fatal(err)
	}
	if err := old.SaveTokenMaturity(synthHashB, `{"cfg":true}`, []byte(`{"streak":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := old.SavePoolState("pool/admissions", []byte(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
}

func targetCount(t *testing.T, st *Store, table string) int64 {
	t.Helper()
	var n int64
	if err := st.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count target %s: %v", table, err)
	}
	return n
}

// TestImportLegacyPersistedStateCarriesRows pins the full migrate: every
// persist table lands byte-identically (secrets opaque), and the immediate
// re-run is a strict no-op with the target unchanged.
func TestImportLegacyPersistedStateCarriesRows(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "legacy.db")
	seedLegacyPersisted(t, oldPath)
	newPath := filepath.Join(dir, "live.db")
	st, err := Open(newPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	n, err := ImportLegacyPersistedState(st, newPath, oldPath)
	if err != nil {
		t.Fatal(err)
	}
	// settings 4 (2 knobs + credential-shaped row + marker) + one row per
	// other persist table.
	if n != 8 {
		t.Fatalf("carried rows = %d, want 8", n)
	}
	rows, err := st.ListSettings()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("live settings rows = %d, want 4", len(rows))
	}
	if rows["config:AUTH_TOKENS"] != synthPersistToken {
		t.Errorf("credential-shaped row did not carry opaquely: got %q", rows["config:AUTH_TOKENS"])
	}
	if rows["config:migrated_env_v1"] != "1" {
		t.Errorf("env-to-DB marker did not carry: got %q", rows["config:migrated_env_v1"])
	}
	if got, ok, err := st.GetPageState("overview"); err != nil || !ok || got != `{"scroll":12}` {
		t.Errorf("page state = (%q, %v, %v), want (scroll snapshot, true, nil)", got, ok, err)
	}
	if sd, rd, found, err := st.LoadSession(synthHashA); err != nil || !found || sd != `{"s":1}` || rd != `{"r":1}` {
		t.Errorf("session = (%q, %q, %v, %v), want blobs found", sd, rd, found, err)
	}
	if mj, blob, ok, err := st.LoadTokenMaturity(synthHashB); err != nil || !ok || mj != `{"cfg":true}` || string(blob) != `{"streak":1}` {
		t.Errorf("token maturity = (%q, %q, %v, %v), want blobs found", mj, blob, ok, err)
	}
	if pv, ok, err := st.LoadPoolState("pool/admissions"); err != nil || !ok || string(pv) != `{"x":1}` {
		t.Errorf("pool state = (%q, %v, %v), want blob found", pv, ok, err)
	}

	// Second boot is a no-op: every target table is populated, no
	// duplicates, no writes.
	n, err = ImportLegacyPersistedState(st, newPath, oldPath)
	if err != nil || n != 0 {
		t.Fatalf("second import = (%d, %v), want (0, nil)", n, err)
	}
	if got := targetCount(t, st, "settings"); got != 4 {
		t.Errorf("settings after re-run = %d, want 4", got)
	}
}

// TestImportLegacyPersistedStateKeepsExistingRows pins the per-table
// no-merge contract: a populated live table keeps its rows untouched and
// the legacy file's rows for that table stay behind, while empty tables
// still carry.
func TestImportLegacyPersistedStateKeepsExistingRows(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "legacy.db")
	seedLegacyPersisted(t, oldPath)
	newPath := filepath.Join(dir, "live.db")
	st, err := Open(newPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.SetSetting("config:SAFE_MODE", "false"); err != nil {
		t.Fatal(err)
	}

	n, err := ImportLegacyPersistedState(st, newPath, oldPath)
	if err != nil {
		t.Fatal(err)
	}
	// settings skipped (live already populated); the other four tables
	// carry one row each.
	if n != 4 {
		t.Fatalf("carried rows = %d, want 4 (settings left behind)", n)
	}
	rows, err := st.ListSettings()
	if err != nil {
		t.Fatal(err)
	}
	if rows["config:SAFE_MODE"] != "false" {
		t.Errorf("live SAFE_MODE = %q, want live value kept", rows["config:SAFE_MODE"])
	}
	if _, ok := rows["config:AUTH_TOKENS"]; ok {
		t.Error("legacy AUTH_TOKENS merged into a populated settings table, want no-merge")
	}
	if got := targetCount(t, st, "tokens"); got != 1 {
		t.Errorf("tokens = %d, want 1 carried", got)
	}
}

// TestImportLegacyPersistedStateSkipsPrePersistFile pins the v1
// difference from the history carry: a legacy file that predates the
// persist tables skips them (0, nil) instead of failing, and history rows
// are out of scope for this function (still zero afterwards).
func TestImportLegacyPersistedStateSkipsPrePersistFile(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "legacy.db")
	raw, err := sql.Open("sqlite", oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE log_entries(
	  id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, level TEXT NOT NULL,
	  msg TEXT NOT NULL, fields TEXT NOT NULL DEFAULT '', req_id TEXT NOT NULL DEFAULT '')`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO log_entries(ts, level, msg) VALUES(1, 'info', 'old')`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	newPath := filepath.Join(dir, "live.db")
	st, err := Open(newPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	n, err := ImportLegacyPersistedState(st, newPath, oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("pre-persist import = %d, want 0", n)
	}
	if got := targetCount(t, st, "settings"); got != 0 {
		t.Errorf("settings = %d, want 0", got)
	}
	if got := targetCount(t, st, "log_entries"); got != 0 {
		t.Errorf("log_entries = %d, want 0 (history out of scope)", got)
	}
}

func TestImportLegacyPersistedStateNoops(t *testing.T) {
	dir := t.TempDir()
	newPath := filepath.Join(dir, "live.db")
	st, err := Open(newPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if n, err := ImportLegacyPersistedState(st, newPath, filepath.Join(dir, "absent.db")); err != nil || n != 0 {
		t.Errorf("missing file = (%d, %v), want (0, nil)", n, err)
	}
	if n, err := ImportLegacyPersistedState(st, newPath, newPath); err != nil || n != 0 {
		t.Errorf("same path = (%d, %v), want (0, nil)", n, err)
	}
	bad := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(bad, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportLegacyPersistedState(st, newPath, bad); err == nil {
		t.Error("garbage file accepted, want error")
	}
}

// TestCountLegacyPersistedRows pins the split-brain inspector: per-table
// counts from the staged copy, zeroes for missing files, an error for
// garbage — and the source left importable afterwards.
func TestCountLegacyPersistedRows(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "legacy.db")
	seedLegacyPersisted(t, oldPath)
	newPath := filepath.Join(dir, "live.db")
	st, err := Open(newPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	counts, err := CountLegacyPersistedRows(st, oldPath)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{
		"settings": 4, "pages_state": 1, "sessions_persist": 1, "tokens": 1, "pool_state": 1,
	}
	for table, w := range want {
		if counts[table] != w {
			t.Errorf("%s count = %d, want %d", table, counts[table], w)
		}
	}

	absent, err := CountLegacyPersistedRows(st, filepath.Join(dir, "absent.db"))
	if err != nil {
		t.Fatal(err)
	}
	for table, c := range absent {
		if c != 0 {
			t.Errorf("absent %s count = %d, want 0", table, c)
		}
	}

	bad := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(bad, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CountLegacyPersistedRows(st, bad); err == nil {
		t.Error("garbage file counted, want error")
	}

	// The inspected file still carries afterwards: counting is read-only.
	if n, err := ImportLegacyPersistedState(st, newPath, oldPath); err != nil || n != 8 {
		t.Fatalf("carry after inspect = (%d, %v), want (8, nil)", n, err)
	}
}
