// Package store is the dashboard backend (ADR-0016): one pure-Go SQLite file
// holding log entries, quota snapshots, maturity events, request records,
// and — since the env-to-DB migration — the DB settings overlay (ADR-0019),
// the persisted home of the whole config knob set. That includes secrets
// (AUTH_TOKENS, ADMIN_TOKEN, API_KEYS, WEBHOOK_URL rows): the file is
// created and kept at mode 0600 on open, and operators must preserve that
// when copying or backing the file up. Leaf package by construction: stdlib
// + the modernc driver + pressly/goose for migrations only, zero internal
// imports (see archtest matrix). History is display and debug data, never
// control state: a missing or corrupt file degrades to live-only views, and
// retention runs off the request path.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"github.com/pressly/goose/v3"
	"io/fs"
	"math/rand/v2"
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// schemaVersion guards the on-disk format. v1 held the history tables only
// (log_entries, quota_snapshots, maturity_events, request_records); v2 adds
// the persistence tables (settings, pages_state, sessions_persist, tokens).
// v3 adds the maturity columns (maturity_json, streak_blob) to tokens.
// v4 adds the pool runtime table (pool_state: opaque blobs keyed by stable
// string keys for ledger counters, admissions, bridge usage/survivors and
// burst hits — see pool_persist.go).
// Open migrates older files in place via the embedded goose migrations
// (migrations/00001..00004, one version per legacy user_version stamp);
// anything else non-zero is rejected so a stale file is ignored instead of
// mis-parsed (mirrors session.storeVersion).
const schemaVersion = 4

// migrationsFS embeds the goose migration chain. Versions are sequential
// 1..schemaVersion on purpose: a legacy file stamped with PRAGMA
// user_version=N baselines migrations 1..N as applied (its objects already
// exist) and runs only the remainder, so every pre-goose file converges
// with zero data change.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// gooseVersionDDL is pressly/goose v3.28.0's sqlite version-table DDL
// (internal/dialects/sqlite3.go CreateTable): the legacy baseline path
// creates it before seeding stamps so goose's own ensure sees the identical
// table it would have created itself.
const gooseVersionDDL = `CREATE TABLE IF NOT EXISTS goose_db_version (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	version_id INTEGER NOT NULL,
	is_applied INTEGER NOT NULL,
	tstamp TIMESTAMP DEFAULT (datetime('now'))
)`

// LogEntry is one persisted log record. TS is Unix millis UTC; Fields carries
// the flattened "key=value" pairs exactly as logring renders them.
type LogEntry struct {
	ID     int64
	TS     int64
	Level  string
	Msg    string
	Fields string
	ReqID  string
}

// QuotaSnapshot is one per-model session quota sample. Entitlements rides as
// the raw upstream JSON object (nil-friendly: "" when absent).
type QuotaSnapshot struct {
	ID           int64
	TS           int64
	TokenIdx     int
	Model        string
	Limit        float64
	Recent       float64
	ResetAt      int64
	Entitlements string
}

// MaturityEvent is one streak/standing transition worth keeping across restarts.
type MaturityEvent struct {
	ID       int64
	TS       int64
	TokenIdx int
	Kind     string
	Detail   string
}

// RequestRecord is one /v1 inference outcome for the Logs console view.
type RequestRecord struct {
	ReqID    string
	TS       int64
	Endpoint string
	Model    string
	TokenIdx int
	Status   string
	TTFBms   int64
	Err      string
}

// LogFilter selects log rows. Zero values mean "no constraint"; Limit <= 0
// defaults, and is capped, by the query.
type LogFilter struct {
	Since    int64
	Until    int64
	Level    string
	Contains string
	ReqID    string
	Limit    int
}

// Millis converts a time to the Unix-millis domain every TS field uses.
func Millis(t time.Time) int64 { return t.UnixMilli() }

// Store wraps the history database. Zero value is unusable; Open first.
type Store struct {
	db *sql.DB
	// writeMu is the store's single write boundary: every writer method
	// (settings UPSERT, logring spill, retention purge, request/quota
	// records, pool state) runs under it, so this process never offers two
	// concurrent writers to SQLite. Reads are not serialized against it.
	writeMu sync.Mutex
	// status is the boot migration report captured by OpenWithStatus:
	// the detected from-version plus the goose versions that actually ran.
	// In-memory only (no DB reads); MigrateStatus returns a copy.
	status MigrateStatus
}

// MigrateStatus is the boot-time smart-migration report for one DB file:
// which data generation Open found and what it did to converge it.
// FromVersion is the detected PRAGMA user_version stamp before migration
// (0 means no DB file existed — a fresh init); ToVersion is always the
// current schemaVersion; Applied lists the goose versions that actually
// executed this boot (baselined versions were recorded, never run, so a
// legacy v4 takeover reports an empty Applied); Fresh reports a fresh init;
// Noop reports a strict no-op boot (no schema writes: an existing file
// already at the latest version with nothing pending). Applied is never nil
// so the dashboard payload encodes [] rather than null.
type MigrateStatus struct {
	FromVersion int   `json:"from_version"`
	ToVersion   int   `json:"to_version"`
	Applied     []int `json:"applied"`
	Fresh       bool  `json:"fresh"`
	Noop        bool  `json:"noop"`
}

// MigrateStatus returns the boot migration report captured at Open. It is a
// copy over in-memory state: read-cheap (no DB I/O), safe for per-request
// dashboard readers.
func (s *Store) MigrateStatus() MigrateStatus {
	if s == nil {
		return MigrateStatus{Applied: []int{}}
	}
	out := s.status
	out.Applied = append([]int{}, s.status.Applied...)
	return out
}

// defaultDBPath is the DB file used when DB_PATH is unset: ./data/freebuff.db
// relative to the process working directory (bind-only: the compose host dir
// mounted at /app/state, so ./data/freebuff.db under WORKDIR; the settings
// overlay in that DB beats .env).
const defaultDBPath = "data/freebuff.db"

// busyTimeoutMillis is how long every connection waits for the writer in
// front of it before giving up. It has to be a CONNECTION property, not a
// one-shot pragma: the same database file is written by the settings
// UPSERT, the logring spill transaction, the retention purge and the
// request/quota recorders, and the process used to configure only the first
// connection the pool happened to create. Every later connection inherited
// SQLite's 0ms default and failed fast with the instant-save error
// `store: set setting "config:QUEUE_WAIT": database is locked (5)
// (SQLITE_BUSY)`.
const busyTimeoutMillis = 5000

// Bounded retry for a write that still lost the lock (contention from
// outside this process — a second gateway on the same file, a sqlite3
// shell, a backup tool). Small and jittered on purpose: the connection-level
// busy_timeout already absorbs the common case, so this only covers the
// residual race without turning a POST into a long stall.
const (
	busyRetryAttempts = 3
	busyRetryBudget   = 5 * time.Second
	busyRetryBase     = 50 * time.Millisecond
)

// sqliteDSN builds the connection string for one database file. The pragmas
// ride in the DSN because the driver applies DSN query parameters to EVERY
// connection it opens (modernc.org/sqlite conn.go newConn -> applyQueryParams,
// which runs the `_pragma` list on each new connection), so each pooled
// connection starts life already configured:
//
//	_pragma=busy_timeout(5000)   wait for the writer in front instead of
//	                             failing with SQLITE_BUSY
//	_pragma=synchronous(NORMAL)  WAL's safe speed/durability trade
//	_pragma=journal_mode(WAL)    persistent file setting, re-asserted per
//	                             connection and a no-op once already WAL
//	_txlock=immediate            take the write lock at BEGIN, so a
//	                             transaction never upgrades mid-flight into
//	                             an unresolvable SQLITE_BUSY_SNAPSHOT
//
// The plain (non-"file:") form is deliberate: the driver splits the query off
// any DSN containing "?" and passes the rest to sqlite3_open_v2 verbatim, so
// a Windows path keeps its backslashes instead of being parsed as a URI.
//
// The pool is left uncapped on purpose. The busy policy riding on every
// connection plus the in-process write boundary (Store.writeMu) is what makes
// the store lock-free, so a cap buys no correctness — while capping it at one
// connection queues every reader (and every instant-save POST) behind an
// in-flight write. WAL lets a reader run against the committed snapshot while
// a transaction is open; TestReadsDoNotQueueBehindWrites pins that contract.
func sqliteDSN(path string) string {
	return path + "?_pragma=busy_timeout(" + strconv.Itoa(busyTimeoutMillis) + ")" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=journal_mode(WAL)" +
		"&_txlock=immediate"
}

// DBPathFromEnv resolves the SQLite file: DB_PATH wins (blank counts as
// unset), otherwise the ./data/freebuff.db default. Callers log the resolved
// value so a mis-pointed env is visible at startup.
func DBPathFromEnv() string {
	if v := strings.TrimSpace(os.Getenv("DB_PATH")); v != "" {
		return v
	}
	return filepath.FromSlash(defaultDBPath)
}

// Open creates the parent dir, opens (or creates) the SQLite file at path,
// and applies pragmas + the goose migration chain. Older files migrate in
// place with zero data change (a legacy user_version stamp baselines the
// matching migrations as applied; only the remainder runs); any other
// version mismatch or unusable file returns an error and the caller runs
// live-only. Open never fails the boot itself. The boot migration report is
// discarded; OpenWithStatus keeps it.
func Open(path string) (*Store, error) {
	s, _, err := OpenWithStatus(path)
	return s, err
}

// OpenWithStatus is Open plus the boot-time smart-migration report: it
// detects the file's data generation first ((a) no DB file: fresh init,
// (b) legacy pre-goose stamp: baseline + run the pending goose chain,
// (c) goose-converged file: nothing pending), converges it to the latest
// schema, and reports the detected from-version with the versions that
// actually ran. The env-to-DB import (marker config:migrated_env_v1) runs
// after this returns, owned by the caller (cli), so a marker-less file
// still imports on this boot and a marked file stays a strict no-op.
//
// Idempotency: a re-boot on a converged file (user_version at the latest,
// goose chain fully applied, mode already 0600) performs zero writes — the
// version stamp, the chmod, and the goose Up are all skipped when already
// correct, so only reads run. The 0600 enforcement itself stays: a fresh
// create or a pre-migration 0644 file is still tightened (a chmod failure
// fails the open and the caller degrades to live-only).
func OpenWithStatus(path string) (*Store, MigrateStatus, error) {
	st := MigrateStatus{ToVersion: schemaVersion, Applied: []int{}}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, st, fmt.Errorf("store: mkdir %s: %w", dir, err)
		}
	}
	// Fresh-init detection runs before sql.Open creates the file: a missing
	// path means generation (a), an existing file reports its stamp below.
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return nil, st, fmt.Errorf("store: stat %s: %w", path, err)
		}
		st.Fresh = true
	}
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, st, fmt.Errorf("store: open %s: %w", path, err)
	}
	// The busy policy is a connection property, so it has to be pinned on
	// every connection the pool opens — see sqliteDSN. Verifying it here on
	// a real connection is the boot-time guard: if the DSN ever stops
	// carrying the pragma, Open fails loudly instead of silently reverting
	// to fail-fast SQLITE_BUSY under concurrent writes.
	var gotBusyTimeout int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&gotBusyTimeout); err != nil {
		_ = db.Close()
		return nil, st, fmt.Errorf("store: read busy_timeout: %w", err)
	}
	if gotBusyTimeout != busyTimeoutMillis {
		_ = db.Close()
		return nil, st, fmt.Errorf("store: busy_timeout is %dms, want %dms (check the open DSN)", gotBusyTimeout, busyTimeoutMillis)
	}
	// No pool cap on purpose — see sqliteDSN.
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		_ = db.Close()
		return nil, st, fmt.Errorf("store: user_version: %w", err)
	}
	st.FromVersion = v
	switch v {
	case 0, 1, 2, 3, schemaVersion:
		// Fresh file or a supported legacy stamp: goose converges it.
	default:
		_ = db.Close()
		return nil, st, fmt.Errorf("store: schema v%d unsupported (want v%d)", v, schemaVersion)
	}
	seeded := false
	if v == schemaVersion && gooseAtLatest(db) {
		// Steady-state re-boot: the chain is fully applied, so Up would
		// run nothing — skip it (and the stamp below) for zero writes.
	} else if applied, didSeed, err := migrateUp(db, v); err != nil {
		_ = db.Close()
		return nil, st, err
	} else {
		st.Applied = applied
		seeded = didSeed
	}
	st.Noop = !st.Fresh && len(st.Applied) == 0 && !seeded
	// The stamp only ever moves forward: skip the write when already there
	// (steady-state boots leave the header bytes untouched).
	if v != schemaVersion {
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", schemaVersion)); err != nil {
			_ = db.Close()
			return nil, st, fmt.Errorf("store: stamp version: %w", err)
		}
	}
	// The settings table holds secrets since the env-to-DB migration
	// (AUTH_TOKENS, ADMIN_TOKEN, API_KEYS, WEBHOOK_URL overlay rows), so
	// the file is kept at 0600: a fresh create inherits umask-loosened
	// modes, and a pre-migration file may still be 0644. Best-effort is
	// not enough for credential material — a chmod failure fails the open
	// and the caller degrades to live-only. Already-0600 files skip the
	// call so steady-state boots change nothing.
	if fi, err := os.Stat(path); err != nil {
		_ = db.Close()
		return nil, st, fmt.Errorf("store: stat %s: %w", path, err)
	} else if fi.Mode().Perm() != 0o600 {
		if err := os.Chmod(path, 0o600); err != nil {
			_ = db.Close()
			return nil, st, fmt.Errorf("store: chmod 0600 %s: %w", path, err)
		}
	}
	return &Store{db: db, status: st}, st, nil
}

// migrateUp brings any supported file to the latest schema via the embedded
// goose migrations. Pre-goose files (user_version 1..4) carry no version
// rows, so versions at or below the baseline are recorded as applied without
// running — their objects already exist — and only the remainder executes.
// Every row is preserved; only DDL runs. It returns the versions that
// actually executed (never nil: empty when Up ran nothing, e.g. a legacy v4
// takeover whose whole chain baselined) and whether baseline rows were
// seeded (a write even when nothing executed, so callers can tell a
// first-boot takeover from a steady-state no-op).
func migrateUp(db *sql.DB, legacy int) (applied []int, seeded bool, err error) {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return nil, false, fmt.Errorf("store: migrations fs: %w", err)
	}
	provider, err := goose.NewProvider(
		goose.DialectSQLite3,
		db,
		sub,
		goose.WithDisableGlobalRegistry(true),
		goose.WithLogger(goose.NopLogger()),
	)
	if err != nil {
		return nil, false, fmt.Errorf("store: goose provider: %w", err)
	}
	if base := baselineVersion(db, legacy); base > 0 {
		if _, err := db.Exec(gooseVersionDDL); err != nil {
			return nil, false, fmt.Errorf("store: goose version table: %w", err)
		}
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM goose_db_version`).Scan(&n); err != nil {
			return nil, false, fmt.Errorf("store: goose version count: %w", err)
		}
		if n == 0 {
			// Seed 0..base: goose's own convention inserts a zero version
			// on creation, and its sqlite ensure falls back to reading
			// version 0 when the table-exists check is unsupported — the
			// row keeps that fallback from attempting a CREATE over the
			// table just made above.
			for v := 0; v <= base; v++ {
				if _, err := db.Exec(`INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, v); err != nil {
					return nil, false, fmt.Errorf("store: baseline goose v%d: %w", v, err)
				}
			}
			seeded = true
		}
	}
	// Versions present before Up are baselined-or-current (never executed
	// this boot); the diff after Up is exactly what ran. Version 0 is
	// goose's own bookkeeping row, never a migration step.
	before := gooseVersions(db)
	if _, err := provider.Up(context.Background()); err != nil {
		return nil, seeded, fmt.Errorf("store: migrate up: %w", err)
	}
	after := gooseVersions(db)
	applied = []int{}
	for v := 1; v <= schemaVersion; v++ {
		if after[v] && !before[v] {
			applied = append(applied, v)
		}
	}
	return applied, seeded, nil
}

// gooseVersions returns the set of applied goose version_ids (the version 0
// bookkeeping row included when present). A missing version table reads as
// empty — the caller baselines first — and any read failure degrades the
// same way: the Up diff then reports every present version as applied,
// which only over-reports the log lines, never the schema.
func gooseVersions(db *sql.DB) map[int]bool {
	out := map[int]bool{}
	if !hasTable(db, "goose_db_version") {
		return out
	}
	rows, err := db.Query(`SELECT version_id FROM goose_db_version WHERE is_applied = 1`)
	if err != nil {
		return out
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return map[int]bool{}
		}
		out[v] = true
	}
	return out
}

// gooseAtLatest reports whether the goose chain is fully applied (versions
// 1..schemaVersion recorded, tip at the latest): the read-cheap gate that
// lets steady-state boots skip Up entirely for zero writes.
func gooseAtLatest(db *sql.DB) bool {
	if !hasTable(db, "goose_db_version") {
		return false
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM goose_db_version WHERE is_applied = 1 AND version_id BETWEEN 1 AND ?`, schemaVersion).Scan(&n); err != nil {
		return false
	}
	if n != schemaVersion {
		return false
	}
	var max int
	if err := db.QueryRow(`SELECT MAX(version_id) FROM goose_db_version WHERE is_applied = 1`).Scan(&max); err != nil {
		return false
	}
	return max == schemaVersion
}

// baselineVersion maps a legacy PRAGMA user_version stamp to the goose
// baseline: versions at or below it are recorded as applied without running.
// Probes lift the baseline when the file already carries later objects — a
// half-migrated v2 file whose ALTER landed before the old chain stamped, or
// a hand-made unstamped file — so Up never replays a DDL the file has.
func baselineVersion(db *sql.DB, legacy int) int {
	base := legacy
	if base < 3 && hasColumn(db, "tokens", "maturity_json") {
		base = 3
	}
	if base < 4 && hasTable(db, "pool_state") {
		base = 4
	}
	return base
}

// hasTable reports whether a table exists in the file.
func hasTable(db *sql.DB, table string) bool {
	var one int
	return db.QueryRow(`SELECT 1 FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&one) == nil
}

// hasColumn reports whether a table carries a column (false when the table
// itself is missing: table_info on a missing table returns zero rows).
func hasColumn(db *sql.DB, table, column string) bool {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false
		}
		if name == column {
			_ = rows.Close()
			return true
		}
	}
	return false
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// withWrite runs fn inside the store's single write boundary: this process's
// writer serialization plus a bounded, jittered retry while SQLite reports
// the file as locked.
//
// One lock for every table, because every writer here touches the same
// database file and SQLite admits exactly one writer at a time. Serializing
// costs nothing as long as the callback stays a single statement or one short
// transaction — no validation, HTTP work, or computation belongs inside.
// The connection-level busy_timeout still covers writers outside this
// process (a second gateway, a sqlite3 shell).
//
// The retry covers what the lock cannot: an external writer holding the file.
// It is capped at busyRetryAttempts extra runs inside busyRetryBudget and
// jittered, so two writers that lost the same lock do not retry in lockstep.
// A write that still cannot land returns its real error — never a fabricated
// success. Retrying is safe for every caller: each callback is a statement or
// a transaction SQLite rolls back atomically, so a failed run leaves nothing
// half-applied.
//
// The lock is held ACROSS the retries, including the jittered sleeps, so a
// writer that keeps losing the file to an external process blocks the other
// writers for up to busyRetryBudget instead of letting them interleave. That
// is the deliberate trade: the wait is bounded, the queue stays FIFO, and no
// writer can starve another in this process. Nothing may call another writer
// from inside fn — the mutex is deliberately not reentrant, and withWrite is
// never nested (SaveSession's empty-blob path delegates to DeleteSession
// before it takes the lock).
func (s *Store) withWrite(fn func() error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	deadline := time.Now().Add(busyRetryBudget)
	backoff := busyRetryBase
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil || attempt == busyRetryAttempts || !isBusyError(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(backoff + time.Duration(rand.Int64N(int64(backoff))))
		backoff *= 2
	}
}

// isBusyError reports whether err is SQLite refusing to write because
// another connection holds the lock. The driver returns extended result
// codes (SQLITE_BUSY_SNAPSHOT, SQLITE_BUSY_TIMEOUT, SQLITE_LOCKED_*), so the
// primary code byte decides; a wrapped or reworded error falls back to the
// canonical message text.
func isBusyError(err error) bool {
	if err == nil {
		return false
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return true
		}
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "database table is locked")
}

// Purge deletes rows older than the given Unix-millis cutoffs. Retention runs
// from a background goroutine the server wires later — never on the request
// path (ADR-0016). No VACUUM: freed pages are reused by later inserts.
func (s *Store) Purge(logsBefore, quotaBefore, maturityBefore, requestsBefore int64) error {
	return s.withWrite(func() error {
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("store: purge begin: %w", err)
		}
		cuts := []struct {
			table string
			ts    int64
		}{
			{"log_entries", logsBefore},
			{"quota_snapshots", quotaBefore},
			{"maturity_events", maturityBefore},
			{"request_records", requestsBefore},
		}
		for _, c := range cuts {
			if _, err := tx.Exec("DELETE FROM "+c.table+" WHERE ts < ?", c.ts); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("store: purge %s: %w", c.table, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: purge commit: %w", err)
		}
		return nil
	})
}
