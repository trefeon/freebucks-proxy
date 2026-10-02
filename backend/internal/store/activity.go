// activity.go - per-day/per-hour rollups with TopN caps (universal-gateway
// Phase 2.6/2.7 store layer).
//
// Pre-aggregated GROUP BY queries over the EXISTING request_records
// columns (no migration): day/hour buckets plus TopN by model and by
// token. The dashboard activity views read these instead of scanning rows.
// TopN caps follow the adminrollup precedent (ByKey/ByUser=100); callers
// pass smaller limits for the activity panels.
//
// Cost honesty: request_records has no cost column, so spend-per-day is
// not answerable here and is not invented — spend stays in the pool
// ledger (pool/spend.go). Rows are unpriced by schema, flagged by the
// absence of any cost field, never zero-filled.
package store

import (
	"fmt"
)

// ActivityTopN caps Top-model/Top-token groups (adminrollup ByKey/ByUser
// precedent). Larger limits clamp, never scan unbounded.
const ActivityTopN = 100

// DayCount is one UTC-day bucket: [Day, Day+86400).
type DayCount struct {
	Day    int64
	Total  int64
	Errors int64
}

// HourCount is one UTC-hour bucket: [Hour, Hour+3600).
type HourCount struct {
	Hour   int64
	Total  int64
	Errors int64
}

// TopCount is one TopN row: Name is the model id (TopModels) or the token
// index rendered as "token-N" with "-1" for the bridge (TopTokens).
type TopCount struct {
	Name   string
	Total  int64
	Errors int64
}

// RequestsByDay aggregates request_records into UTC-day buckets over
// [since, until). Empty windows return an empty (non-nil) slice.
func (s *Store) RequestsByDay(since, until int64) ([]DayCount, error) {
	out := []DayCount{}
	var err error
	if since, until, err = clampRollupWindow(since, until); err != nil {
		return out, err
	}
	rows, err := s.db.Query(
		`SELECT (ts / 86400000) * 86400000, COUNT(*), COALESCE(SUM(status = 'error'), 0)
		 FROM request_records WHERE ts >= ? AND ts < ? GROUP BY (ts / 86400000) ORDER BY 1`,
		since, until,
	)
	if err != nil {
		return out, fmt.Errorf("store: activity by-day: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var d DayCount
		if err := rows.Scan(&d.Day, &d.Total, &d.Errors); err != nil {
			return out, fmt.Errorf("store: scan activity day: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: rows activity day: %w", err)
	}
	return out, nil
}

// RequestsByHour aggregates request_records into UTC-hour buckets over
// [since, until). Empty windows return an empty (non-nil) slice.
func (s *Store) RequestsByHour(since, until int64) ([]HourCount, error) {
	out := []HourCount{}
	var err error
	if since, until, err = clampRollupWindow(since, until); err != nil {
		return out, err
	}
	rows, err := s.db.Query(
		`SELECT (ts / 3600000) * 3600000, COUNT(*), COALESCE(SUM(status = 'error'), 0)
		 FROM request_records WHERE ts >= ? AND ts < ? GROUP BY (ts / 3600000) ORDER BY 1`,
		since, until,
	)
	if err != nil {
		return out, fmt.Errorf("store: activity by-hour: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var h HourCount
		if err := rows.Scan(&h.Hour, &h.Total, &h.Errors); err != nil {
			return out, fmt.Errorf("store: scan activity hour: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: rows activity hour: %w", err)
	}
	return out, nil
}

// TopModels returns the top-N models by volume over [since, until),
// ties broken alphabetically for stable ranks. limit <= 0 means
// ActivityTopN; larger limits clamp to it.
func (s *Store) TopModels(since, until int64, limit int) ([]TopCount, error) {
	out := []TopCount{}
	var err error
	if since, until, err = clampRollupWindow(since, until); err != nil {
		return out, err
	}
	if limit <= 0 || limit > ActivityTopN {
		limit = ActivityTopN
	}
	rows, err := s.db.Query(
		`SELECT model, COUNT(*), COALESCE(SUM(status = 'error'), 0)
		 FROM request_records WHERE ts >= ? AND ts < ?
		 GROUP BY model ORDER BY COUNT(*) DESC, model LIMIT ?`,
		since, until, limit,
	)
	if err != nil {
		return out, fmt.Errorf("store: activity top-models: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var g TopCount
		if err := rows.Scan(&g.Name, &g.Total, &g.Errors); err != nil {
			return out, fmt.Errorf("store: scan activity top-models: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: rows activity top-models: %w", err)
	}
	return out, nil
}

// TopTokens returns the top-N token lanes by volume over [since, until).
// token_idx renders as "token-N" ("token-bridge" for -1). Same limit
// contract as TopModels.
func (s *Store) TopTokens(since, until int64, limit int) ([]TopCount, error) {
	out := []TopCount{}
	var err error
	if since, until, err = clampRollupWindow(since, until); err != nil {
		return out, err
	}
	if limit <= 0 || limit > ActivityTopN {
		limit = ActivityTopN
	}
	rows, err := s.db.Query(
		`SELECT token_idx, COUNT(*), COALESCE(SUM(status = 'error'), 0)
		 FROM request_records WHERE ts >= ? AND ts < ?
		 GROUP BY token_idx ORDER BY COUNT(*) DESC, token_idx LIMIT ?`,
		since, until, limit,
	)
	if err != nil {
		return out, fmt.Errorf("store: activity top-tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var idx, total, errs int64
		if err := rows.Scan(&idx, &total, &errs); err != nil {
			return out, fmt.Errorf("store: scan activity top-tokens: %w", err)
		}
		name := fmt.Sprintf("token-%d", idx)
		if idx < 0 {
			name = "token-bridge"
		}
		out = append(out, TopCount{Name: name, Total: total, Errors: errs})
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: rows activity top-tokens: %w", err)
	}
	return out, nil
}
