// Logs rollup, export, and import over the history tables (ADR-0016).
//
// Read-only aggregation plus gap-filling restore, all over EXISTING columns:
// request_records (req_id PK, ts unix-millis, endpoint, model, token_idx with
// -1 = bridge, status ok/error, ttfb_ms upstream-TTFB-only, error
// chatErrClass bucket) and log_entries (rowid id PK, ts, level, msg, fields,
// req_id). No migration, no new columns, no writes except the import path's
// INSERT OR IGNORE (which never overwrites a live row).
//
// Every window is bounded by RollupMaxWindowMillis (the 168h table retention
// the purge path enforces): a wider window clamps instead of scanning, so no
// query can run unbounded. Total latency, HTTP codes, pre-attempt refusals,
// and token volumes are honest gaps — the columns cannot answer them, and
// this file does not invent them.
package store

import (
	"fmt"
	"time"
)

// RollupMaxWindowMillis bounds every rollup/export window to the frozen 168h
// (7d) request/log table retention. Older rows are purged, so a wider window
// can never match more rows — clamp, never scan unbounded.
const RollupMaxWindowMillis = int64(168 * 60 * 60 * 1000)

// rollupFutureSkewMillis tolerates mildly future-dated rows on import
// (clock skew between writers); anything further out is rejected.
const rollupFutureSkewMillis = int64(60 * 60 * 1000)

// RollupNameCount is one GROUP BY row over a TEXT column (model, error).
type RollupNameCount struct {
	Name   string
	Total  int64
	Errors int64
}

// RollupTokenCount is one GROUP BY row over token_idx (-1 = bridge).
type RollupTokenCount struct {
	TokenIdx int
	Total    int64
	Errors   int64
}

// RollupTTFB is the upstream-TTFB distribution over rows with a measured
// value (ttfb_ms > 0): error rows that never reached upstream carry 0 and
// are excluded from the percentiles, not averaged in as instant.
type RollupTTFB struct {
	Count int64
	P50   int64
	P90   int64
	P99   int64
}

// RollupBucket is one millis time bucket: [TS, TS+BucketMs).
type RollupBucket struct {
	TS     int64
	Total  int64
	OK     int64
	Errors int64
}

// RequestsRollupResult is the full aggregation for one clamped window.
type RequestsRollupResult struct {
	Since     int64
	Until     int64
	BucketMs  int64
	Total     int64
	OK        int64
	Errors    int64
	ErrorRate float64
	ByModel   []RollupNameCount
	ByToken   []RollupTokenCount
	ByError   []RollupNameCount
	TTFB      RollupTTFB
	Buckets   []RollupBucket
}

// clampRollupWindow validates the window and clamps it to the retention
// bound. until <= 0 means now. Over-wide windows clamp since forward — the
// recent end is what the operator asked about. Invalid windows error.
func clampRollupWindow(since, until int64) (int64, int64, error) {
	if since < 0 {
		return 0, 0, fmt.Errorf("store: rollup since %d < 0", since)
	}
	if until <= 0 {
		until = Millis(time.Now())
	}
	if since >= until {
		return 0, 0, fmt.Errorf("store: rollup since %d >= until %d", since, until)
	}
	if until-since > RollupMaxWindowMillis {
		since = until - RollupMaxWindowMillis
	}
	return since, until, nil
}

// RequestsRollup aggregates request_records over [since, until) into counts,
// per-model/per-token/per-error GROUP BYs, the error rate, upstream-TTFB
// percentiles (OFFSET selects over the ttfb_ms > 0 population), and millis
// time buckets (ts/:bucket)*:bucket. An empty window returns zeros with
// empty (non-nil) groups so the JSON wire stays an array, never null.
func (s *Store) RequestsRollup(since, until, bucketMs int64) (RequestsRollupResult, error) {
	out := RequestsRollupResult{
		ByModel: []RollupNameCount{},
		ByToken: []RollupTokenCount{},
		ByError: []RollupNameCount{},
		Buckets: []RollupBucket{},
	}
	if bucketMs <= 0 {
		return out, fmt.Errorf("store: rollup bucket %d <= 0", bucketMs)
	}
	var err error
	if since, until, err = clampRollupWindow(since, until); err != nil {
		return out, err
	}
	out.Since, out.Until, out.BucketMs = since, until, bucketMs

	if err := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(status = 'ok'), 0), COALESCE(SUM(status = 'error'), 0)
		 FROM request_records WHERE ts >= ? AND ts < ?`,
		since, until,
	).Scan(&out.Total, &out.OK, &out.Errors); err != nil {
		return out, fmt.Errorf("store: rollup totals: %w", err)
	}
	if out.Total > 0 {
		out.ErrorRate = float64(out.Errors) / float64(out.Total)
	}

	rows, err := s.db.Query(
		`SELECT model, COUNT(*), COALESCE(SUM(status = 'error'), 0)
		 FROM request_records WHERE ts >= ? AND ts < ? GROUP BY model ORDER BY model`,
		since, until,
	)
	if err != nil {
		return out, fmt.Errorf("store: rollup by-model: %w", err)
	}
	for rows.Next() {
		var g RollupNameCount
		if err := rows.Scan(&g.Name, &g.Total, &g.Errors); err != nil {
			_ = rows.Close()
			return out, fmt.Errorf("store: scan rollup model: %w", err)
		}
		out.ByModel = append(out.ByModel, g)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return out, fmt.Errorf("store: rows rollup model: %w", err)
	}
	_ = rows.Close()

	trows, err := s.db.Query(
		`SELECT token_idx, COUNT(*), COALESCE(SUM(status = 'error'), 0)
		 FROM request_records WHERE ts >= ? AND ts < ? GROUP BY token_idx ORDER BY token_idx`,
		since, until,
	)
	if err != nil {
		return out, fmt.Errorf("store: rollup by-token: %w", err)
	}
	for trows.Next() {
		var g RollupTokenCount
		if err := trows.Scan(&g.TokenIdx, &g.Total, &g.Errors); err != nil {
			_ = trows.Close()
			return out, fmt.Errorf("store: scan rollup token: %w", err)
		}
		out.ByToken = append(out.ByToken, g)
	}
	if err := trows.Err(); err != nil {
		_ = trows.Close()
		return out, fmt.Errorf("store: rows rollup token: %w", err)
	}
	_ = trows.Close()

	erows, err := s.db.Query(
		`SELECT error, COUNT(*), COUNT(*)
		 FROM request_records WHERE ts >= ? AND ts < ? AND status = 'error'
		 GROUP BY error ORDER BY error`,
		since, until,
	)
	if err != nil {
		return out, fmt.Errorf("store: rollup by-error: %w", err)
	}
	for erows.Next() {
		var g RollupNameCount
		if err := erows.Scan(&g.Name, &g.Total, &g.Errors); err != nil {
			_ = erows.Close()
			return out, fmt.Errorf("store: scan rollup error: %w", err)
		}
		out.ByError = append(out.ByError, g)
	}
	if err := erows.Err(); err != nil {
		_ = erows.Close()
		return out, fmt.Errorf("store: rows rollup error: %w", err)
	}
	_ = erows.Close()

	if out.TTFB, err = s.rollupTTFB(since, until); err != nil {
		return out, err
	}

	brows, err := s.db.Query(
		`SELECT (ts / ?) * ?, COUNT(*), COALESCE(SUM(status = 'ok'), 0), COALESCE(SUM(status = 'error'), 0)
		 FROM request_records WHERE ts >= ? AND ts < ? GROUP BY (ts / ?) * ? ORDER BY 1`,
		bucketMs, bucketMs, since, until, bucketMs, bucketMs,
	)
	if err != nil {
		return out, fmt.Errorf("store: rollup buckets: %w", err)
	}
	for brows.Next() {
		var b RollupBucket
		if err := brows.Scan(&b.TS, &b.Total, &b.OK, &b.Errors); err != nil {
			_ = brows.Close()
			return out, fmt.Errorf("store: scan rollup bucket: %w", err)
		}
		out.Buckets = append(out.Buckets, b)
	}
	if err := brows.Err(); err != nil {
		_ = brows.Close()
		return out, fmt.Errorf("store: rows rollup bucket: %w", err)
	}
	_ = brows.Close()
	return out, nil
}

// rollupTTFB computes p50/p90/p99 over the measured-TTFB population
// (ttfb_ms > 0, oldest-to-newest rank) via OFFSET selects. idx = q*(n-1):
// the median of five is element 2, and p90/p99 of five both land on element
// 3 — exact ranks, not interpolation.
func (s *Store) rollupTTFB(since, until int64) (RollupTTFB, error) {
	var t RollupTTFB
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM request_records WHERE ts >= ? AND ts < ? AND ttfb_ms > 0`,
		since, until,
	).Scan(&t.Count); err != nil {
		return t, fmt.Errorf("store: rollup ttfb count: %w", err)
	}
	if t.Count == 0 {
		return t, nil
	}
	quantile := func(q float64) (int64, error) {
		off := int64(q * float64(t.Count-1))
		var v int64
		if err := s.db.QueryRow(
			`SELECT ttfb_ms FROM request_records
			 WHERE ts >= ? AND ts < ? AND ttfb_ms > 0
			 ORDER BY ttfb_ms LIMIT 1 OFFSET ?`,
			since, until, off,
		).Scan(&v); err != nil {
			return 0, fmt.Errorf("store: rollup ttfb quantile: %w", err)
		}
		return v, nil
	}
	var err error
	if t.P50, err = quantile(0.50); err != nil {
		return t, err
	}
	if t.P90, err = quantile(0.90); err != nil {
		return t, err
	}
	if t.P99, err = quantile(0.99); err != nil {
		return t, err
	}
	return t, nil
}

// ExportRequests streams request_records in [since, until) oldest-first to
// fn, one row at a time — the export path never buffers the window in
// memory. Same retention clamp as the rollup.
func (s *Store) ExportRequests(since, until int64, fn func(RequestRecord) error) error {
	var err error
	if since, until, err = clampRollupWindow(since, until); err != nil {
		return err
	}
	rows, err := s.db.Query(
		`SELECT req_id, ts, endpoint, model, token_idx, status, ttfb_ms, error, client_key_hash
		 FROM request_records WHERE ts >= ? AND ts < ? ORDER BY ts, req_id`,
		since, until,
	)
	if err != nil {
		return fmt.Errorf("store: export requests: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var rec RequestRecord
		if err := rows.Scan(&rec.ReqID, &rec.TS, &rec.Endpoint, &rec.Model, &rec.TokenIdx, &rec.Status, &rec.TTFBms, &rec.Err, &rec.ClientKeyHash); err != nil {
			return fmt.Errorf("store: scan export request: %w", err)
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ExportLogs streams log_entries in [since, until) oldest-first to fn. Same
// window semantics as ExportRequests.
func (s *Store) ExportLogs(since, until int64, fn func(LogEntry) error) error {
	var err error
	if since, until, err = clampRollupWindow(since, until); err != nil {
		return err
	}
	rows, err := s.db.Query(
		`SELECT id, ts, level, msg, fields, req_id
		 FROM log_entries WHERE ts >= ? AND ts < ? ORDER BY ts, id`,
		since, until,
	)
	if err != nil {
		return fmt.Errorf("store: export logs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var e LogEntry
		if err := rows.Scan(&e.ID, &e.TS, &e.Level, &e.Msg, &e.Fields, &e.ReqID); err != nil {
			return fmt.Errorf("store: scan export log: %w", err)
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ImportCounts reports one import batch: rows filled, rows already present,
// rows failing column validation (counted, never aborting the batch).
type ImportCounts struct {
	Imported         int64
	SkippedDuplicate int64
	Rejected         int64
}

// validImportTS reports whether ts is sane for an imported history row:
// positive and not beyond the future-skew tolerance.
func validImportTS(ts int64) bool {
	return ts > 0 && ts <= Millis(time.Now())+rollupFutureSkewMillis
}

// ImportRequests restores request rows with INSERT OR IGNORE on the req_id
// PK: an import fills gaps but NEVER overwrites a live row. Rows failing
// column validation (empty req_id, insane ts, empty endpoint, status outside
// {ok,error}, negative ttfb_ms) count as rejected. Imported old rows age
// under the normal 168h purge — no special-casing.
func (s *Store) ImportRequests(recs []RequestRecord) (ImportCounts, error) {
	var c ImportCounts
	if len(recs) == 0 {
		return c, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return c, fmt.Errorf("store: import requests begin: %w", err)
	}
	stmt, err := tx.Prepare(
		`INSERT OR IGNORE INTO request_records(req_id, ts, endpoint, model, token_idx, status, ttfb_ms, error, client_key_hash)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		_ = tx.Rollback()
		return c, fmt.Errorf("store: import requests prepare: %w", err)
	}
	for _, rec := range recs {
		if rec.ReqID == "" || !validImportTS(rec.TS) || rec.Endpoint == "" ||
			(rec.Status != "ok" && rec.Status != "error") || rec.TTFBms < 0 {
			c.Rejected++
			continue
		}
		res, err := stmt.Exec(rec.ReqID, rec.TS, rec.Endpoint, rec.Model, rec.TokenIdx, rec.Status, rec.TTFBms, rec.Err, rec.ClientKeyHash)
		if err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return c, fmt.Errorf("store: import requests exec: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return c, fmt.Errorf("store: import requests rows: %w", err)
		}
		if n == 0 {
			c.SkippedDuplicate++
		} else {
			c.Imported++
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		return c, fmt.Errorf("store: import requests commit: %w", err)
	}
	return c, nil
}

// ImportLogs restores log rows with INSERT OR IGNORE on the rowid PK: same
// gap-fill-only semantics as ImportRequests. Rows with id <= 0, insane ts,
// or empty level count as rejected.
func (s *Store) ImportLogs(entries []LogEntry) (ImportCounts, error) {
	var c ImportCounts
	if len(entries) == 0 {
		return c, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return c, fmt.Errorf("store: import logs begin: %w", err)
	}
	stmt, err := tx.Prepare(
		`INSERT OR IGNORE INTO log_entries(id, ts, level, msg, fields, req_id)
		 VALUES(?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		_ = tx.Rollback()
		return c, fmt.Errorf("store: import logs prepare: %w", err)
	}
	for _, e := range entries {
		if e.ID <= 0 || !validImportTS(e.TS) || e.Level == "" {
			c.Rejected++
			continue
		}
		res, err := stmt.Exec(e.ID, e.TS, e.Level, e.Msg, e.Fields, e.ReqID)
		if err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return c, fmt.Errorf("store: import logs exec: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return c, fmt.Errorf("store: import logs rows: %w", err)
		}
		if n == 0 {
			c.SkippedDuplicate++
		} else {
			c.Imported++
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		return c, fmt.Errorf("store: import logs commit: %w", err)
	}
	return c, nil
}
