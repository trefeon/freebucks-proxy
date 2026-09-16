package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// persistCarryTables are the operator-state tables copied from a legacy
// dashboard DB on first boot with a fresh live store. They extend the
// display-history carry (historyCarryTables) to the full persisted state:
// the settings overlay (config: rows, credentials included), page
// snapshots, sessions, token maturity rows and pool runtime blobs.
//
// Copy semantics mirror the history precedent with two deliberate
// differences: the gate is per-table (a table carries only into an empty
// target table, so a re-boot or a pre-populated live store is a strict
// no-op per table and never merges) and a legacy file that predates a
// table (v1 files predate every persist table) skips that table instead
// of failing — old pre-unified files are common and must stay warn-free.
//
// Secrets migrate as opaque values inside the DB copy: settings rows
// (config:AUTH_TOKENS, config:ADMIN_TOKEN, ...) move byte-identically and
// no value is ever logged — boot reports table names with row counts only.
var persistCarryTables = []string{
	"settings",
	"pages_state",
	"sessions_persist",
	"tokens",
	"pool_state",
}

// ImportLegacyPersistedState carries operator persisted state forward when
// the dashboard DB path moves: if oldPath exists, differs from newPath,
// and a persist table in the open store is empty, it copies that table's
// rows from the legacy file (ATTACH + INSERT SELECT) and returns the total
// row count. Any other state is a no-op (0, nil): missing file, same file,
// fully populated target, or a legacy file without persist tables. A
// corrupt legacy file returns an error and copies nothing (callers only
// warn). The legacy file is left in place — it is never deleted.
//
// Attach strategy matches the history carry: direct ATTACH first (a live
// read-write legacy file carries with its WAL intact), falling back to a
// staged temp copy of the main+-wal+-shm trio. The source is never
// modified by either path.
//
// The whole empty-gate + carry sequence runs inside the store's write
// boundary on one pinned connection (ATTACH is per-connection state;
// see ImportLegacyHistoryDB).
func ImportLegacyPersistedState(s *Store, newPath, oldPath string) (int64, error) {
	if oldPath == "" {
		return 0, nil
	}
	newAbs, err := filepath.Abs(newPath)
	if err != nil {
		return 0, fmt.Errorf("store: resolve new db path: %w", err)
	}
	oldAbs, err := filepath.Abs(oldPath)
	if err != nil {
		return 0, fmt.Errorf("store: resolve legacy state path: %w", err)
	}
	if newAbs == oldAbs {
		return 0, nil
	}
	if _, err := os.Stat(oldAbs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("store: stat legacy state: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), legacyCarryTimeout)
	defer cancel()
	var total int64
	err = s.withWrite(func() error {
		// Per-table empty gate shares one boundary with the copy: a
		// writer that landed rows in the meantime must skip the carry
		// for that table, never merge into a table that stopped being
		// empty.
		empty := make(map[string]bool, len(persistCarryTables))
		needs := false
		for _, t := range persistCarryTables {
			var n int64
			if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+t).Scan(&n); err != nil {
				return fmt.Errorf("store: count %s: %w", t, err)
			}
			empty[t] = n == 0
			needs = needs || n == 0
		}
		if !needs {
			return nil
		}
		conn, err := s.db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("store: pin legacy state connection: %w", err)
		}
		defer func() { _ = conn.Close() }()
		detach, err := attachLegacy(ctx, conn, oldAbs)
		if err != nil {
			staged, cleanup, stageErr := stageLegacyTrio(oldAbs)
			if stageErr != nil {
				return stageErr
			}
			defer cleanup()
			if detach, err = attachLegacy(ctx, conn, staged); err != nil {
				return fmt.Errorf("store: attach legacy state: %w", err)
			}
		}
		defer detach()
		total, err = carryPersistedRows(ctx, conn, empty)
		return err
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// carryPersistedRows copies every empty persist table from the attached
// "legacy" database into the store's main database on the pinned
// connection. Tables the legacy file predates are skipped (not an error).
// A failure returns the rows copied so far — the caller reports and boots
// on, and the deferred DETACH still releases the file.
func carryPersistedRows(ctx context.Context, conn *sql.Conn, empty map[string]bool) (int64, error) {
	var total int64
	for _, t := range persistCarryTables {
		if !empty[t] {
			continue
		}
		var exists int64
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM legacy.sqlite_master WHERE type='table' AND name=?", t).Scan(&exists); err != nil {
			return total, fmt.Errorf("store: inspect legacy %s: %w", t, err)
		}
		if exists == 0 {
			continue
		}
		cols, err := commonColumns(ctx, conn, t)
		if err != nil {
			return total, err
		}
		if len(cols) == 0 {
			continue
		}
		list := quoteColumns(cols)
		res, err := conn.ExecContext(ctx, "INSERT OR IGNORE INTO main."+t+"("+list+") SELECT "+list+" FROM legacy."+t)
		if err != nil {
			return total, fmt.Errorf("store: carry %s: %w", t, err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// commonColumns returns the column names present in both the live and the
// legacy copy of table, in live-table order. Generations disagree on
// tokens (v3 adds maturity_json/streak_blob), so the copy addresses the
// intersection instead of SELECT *: a v2 legacy file still carries its
// six shared columns with the new columns left at their defaults.
func commonColumns(ctx context.Context, conn *sql.Conn, table string) ([]string, error) {
	mainCols, err := tableColumns(ctx, conn, "main", table)
	if err != nil {
		return nil, err
	}
	legacyCols, err := tableColumns(ctx, conn, "legacy", table)
	if err != nil {
		return nil, err
	}
	inLegacy := make(map[string]bool, len(legacyCols))
	for _, c := range legacyCols {
		inLegacy[c] = true
	}
	var out []string
	for _, c := range mainCols {
		if inLegacy[c] {
			out = append(out, c)
		}
	}
	return out, nil
}

// tableColumns lists one attached table's column names in ordinal order.
// Schema and table name from internal constants only, never operator input.
func tableColumns(ctx context.Context, conn *sql.Conn, schema, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, "PRAGMA "+schema+".table_info("+table+")")
	if err != nil {
		return nil, fmt.Errorf("store: inspect %s %s: %w", schema, table, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("store: inspect %s %s: %w", schema, table, err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: inspect %s %s: %w", schema, table, err)
	}
	return out, nil
}

// quoteColumns renders an identifier list for the INSERT..SELECT column
// position. Columns originate from PRAGMA table_info, never operator
// input; quoting still shields unusual catalog names.
func quoteColumns(cols []string) string {
	quoted := make([]string, 0, len(cols))
	for _, c := range cols {
		quoted = append(quoted, `"`+strings.ReplaceAll(c, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, ",")
}

// CountLegacyPersistedRows inspects a legacy dashboard file WITHOUT
// importing it: the main+-wal+-shm trio stages into a temp dir (the source
// is never modified), the staged copy ATTACHes, and every
// persistCarryTables entry gets a COUNT(*). Tables the file predates count
// as zero; a missing file is all-zeroes with nil error; an unreadable or
// corrupt file errors. Boot uses it to log split-brain skips: the carry
// fills an empty table from the FIRST file that holds it, so a later file
// holding rows for an already-filled table is reported with its counts,
// never silently covered. Counts only — values are never selected.
func CountLegacyPersistedRows(s *Store, oldPath string) (map[string]int64, error) {
	counts := make(map[string]int64, len(persistCarryTables))
	for _, t := range persistCarryTables {
		counts[t] = 0
	}
	if oldPath == "" {
		return counts, nil
	}
	oldAbs, err := filepath.Abs(oldPath)
	if err != nil {
		return nil, fmt.Errorf("store: resolve legacy state path: %w", err)
	}
	if _, err := os.Stat(oldAbs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return counts, nil
		}
		return nil, fmt.Errorf("store: stat legacy state: %w", err)
	}
	staged, cleanup, err := stageLegacyTrio(oldAbs)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if _, err := s.db.Exec("ATTACH DATABASE '" + strings.ReplaceAll(staged, "'", "''") + "' AS legacy_count"); err != nil {
		return nil, fmt.Errorf("store: attach legacy state: %w", err)
	}
	defer func() { _, _ = s.db.Exec("DETACH DATABASE legacy_count") }()
	for _, t := range persistCarryTables {
		var exists int64
		if err := s.db.QueryRow("SELECT COUNT(*) FROM legacy_count.sqlite_master WHERE type='table' AND name=?", t).Scan(&exists); err != nil {
			return nil, fmt.Errorf("store: inspect legacy state: %w", err)
		}
		if exists == 0 {
			continue
		}
		var n int64
		if err := s.db.QueryRow("SELECT COUNT(*) FROM legacy_count." + t).Scan(&n); err != nil {
			return nil, fmt.Errorf("store: count legacy %s: %w", t, err)
		}
		counts[t] = n
	}
	return counts, nil
}
