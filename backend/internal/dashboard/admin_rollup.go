// Activity rollups (universal-gateway Phase 2.7): per-day/per-hour
// buckets with TopN caps backing the activity views.
//
// Two sources behind one seam (the instawork adminrollup precedent: totals
// + dims hashes, TopN caps, swappable stores):
//
//   - MemoryRollupStore: in-process RWMutex maps + 50-entry recent ring +
//     UTC-day rollover. The live accumulation point; same interface a
//     persistent store would implement later.
//   - The SQLite path: APIActivityRollup/APIActivityTop read the
//     pre-aggregated store queries (store/activity.go), never full scans.
//
// Auth: Auth=sensitive manifest rows (same guard as logs/history). A nil
// history store answers enabled:false with empty rows, never an error.
package dashboard

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// activityRecentCap bounds the in-memory recent ring (instawork's
// 50-entry recent ring precedent).
const activityRecentCap = 50

// RollupEvent is one recorded metric sample in the recent ring.
type RollupEvent struct {
	TS     int64   `json:"ts"`
	Metric string  `json:"metric"`
	Key    string  `json:"key"`
	User   string  `json:"user"`
	Value  float64 `json:"value"`
}

// RollupEntry is one TopN row: Name with its total, capped at request time.
type RollupEntry struct {
	Name  string  `json:"name"`
	Total float64 `json:"total"`
	Count int64   `json:"count"`
}

// RollupDay is one UTC-day accumulation: per-key and per-user totals plus
// the day total.
type RollupDay struct {
	Day    string             `json:"day"`
	ByKey  map[string]float64 `json:"by_key"`
	ByUser map[string]float64 `json:"by_user"`
	Count  int64              `json:"count"`
	Total  float64            `json:"total"`
}

// RollupStore is the swappable activity-accumulation seam: memory today,
// same methods for a persistent implementation later.
type RollupStore interface {
	// Record accumulates one metric sample (UTC-day rolled).
	Record(metric, key, user string, value float64, at time.Time)
	// Day returns the UTC-day accumulation for one metric (nil when empty).
	Day(metric, day string) *RollupDay
	// TopKeys returns the top-n keys for one metric+day (n <= 0 = all,
	// callers cap at store.ActivityTopN).
	TopKeys(metric, day string, n int) []RollupEntry
	// TopUsers is TopKeys over users.
	TopUsers(metric, day string, n int) []RollupEntry
	// Recent returns the recent ring, newest first.
	Recent() []RollupEvent
}

// MemoryRollupStore is the in-process RollupStore: RWMutex maps keyed by
// metric+UTC-day with a 50-entry recent ring. Restart resets it (same
// discipline as the slot ledger: transient accumulation, never control
// state).
type MemoryRollupStore struct {
	mu     sync.RWMutex
	days   map[string]*RollupDay
	recent []RollupEvent
	head   int
	full   bool
}

// NewMemoryRollupStore builds an empty store.
func NewMemoryRollupStore() *MemoryRollupStore {
	return &MemoryRollupStore{days: make(map[string]*RollupDay)}
}

// utcDay renders the UTC-day key (rollover at UTC midnight).
func utcDay(at time.Time) string {
	return at.UTC().Format("2006-01-02")
}

// Record implements RollupStore.
func (m *MemoryRollupStore) Record(metric, key, user string, value float64, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := metric + "\x00" + utcDay(at)
	d, ok := m.days[k]
	if !ok {
		d = &RollupDay{Day: utcDay(at), ByKey: make(map[string]float64), ByUser: make(map[string]float64)}
		m.days[k] = d
	}
	d.ByKey[key] += value
	if user != "" {
		d.ByUser[user] += value
	}
	d.Count++
	d.Total += value
	ev := RollupEvent{TS: at.UnixMilli(), Metric: metric, Key: key, User: user, Value: value}
	if len(m.recent) < activityRecentCap {
		m.recent = append(m.recent, ev)
		return
	}
	m.recent[m.head] = ev
	m.head = (m.head + 1) % activityRecentCap
	m.full = true
}

// Day implements RollupStore: a detached copy (callers never alias live
// state).
func (m *MemoryRollupStore) Day(metric, day string) *RollupDay {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.days[metric+"\x00"+day]
	if !ok {
		return nil
	}
	out := &RollupDay{
		Day: d.Day, Count: d.Count, Total: d.Total,
		ByKey: make(map[string]float64, len(d.ByKey)), ByUser: make(map[string]float64, len(d.ByUser)),
	}
	for k, v := range d.ByKey {
		out.ByKey[k] = v
	}
	for k, v := range d.ByUser {
		out.ByUser[k] = v
	}
	return out
}

func topEntries(m map[string]float64, counts map[string]int64, n int) []RollupEntry {
	out := make([]RollupEntry, 0, len(m))
	for k, v := range m {
		out = append(out, RollupEntry{Name: k, Total: v, Count: counts[k]})
	}
	// Stable rank: total desc, name asc (unknown-last callers pre-group).
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Name < out[j].Name
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// TopKeys implements RollupStore (stable: total desc, name asc).
func (m *MemoryRollupStore) TopKeys(metric, day string, n int) []RollupEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.days[metric+"\x00"+day]
	if !ok {
		return []RollupEntry{}
	}
	return topEntries(d.ByKey, nil, n)
}

// TopUsers implements RollupStore.
func (m *MemoryRollupStore) TopUsers(metric, day string, n int) []RollupEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.days[metric+"\x00"+day]
	if !ok {
		return []RollupEntry{}
	}
	return topEntries(d.ByUser, nil, n)
}

// Recent implements RollupStore: newest first, detached.
func (m *MemoryRollupStore) Recent() []RollupEvent {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.recent) == 0 {
		return []RollupEvent{}
	}
	out := make([]RollupEvent, 0, len(m.recent))
	if !m.full {
		for i := len(m.recent) - 1; i >= 0; i-- {
			out = append(out, m.recent[i])
		}
		return out
	}
	for i := range m.recent {
		out = append(out, m.recent[(m.head+len(m.recent)-1-i)%len(m.recent)])
	}
	return out
}

// --- SQLite-backed activity endpoints (read path over store/activity.go) ---

type activityBucket struct {
	TS     int64 `json:"ts"`
	Total  int64 `json:"total"`
	Errors int64 `json:"errors"`
}

type activityRollupData struct {
	Buckets     []activityBucket `json:"buckets"`
	Enabled     bool             `json:"enabled"`
	Granularity string           `json:"granularity"`
	Since       int64            `json:"since"`
	Until       int64            `json:"until"`
}

type activityTopRow struct {
	Name   string `json:"name"`
	Total  int64  `json:"total"`
	Errors int64  `json:"errors"`
}

type activityTopData struct {
	Dimension string           `json:"dimension"`
	Enabled   bool             `json:"enabled"`
	Limit     int              `json:"limit"`
	Rows      []activityTopRow `json:"rows"`
	Since     int64            `json:"since"`
	Until     int64            `json:"until"`
}

// parseActivityGranularity resolves ?granularity=: day (default) or hour.
// Anything else rejects — the pages always pass sane values.
func parseActivityGranularity(r *http.Request) (string, bool) {
	g := r.URL.Query().Get("granularity")
	if g == "" {
		return "day", true
	}
	if g == "day" || g == "hour" {
		return g, true
	}
	return "", false
}

// parseActivityTop resolves the ?dimension= + ?limit= params for the top
// endpoint: model (default) or token; limit must be positive (store clamps
// to ActivityTopN).
func parseActivityTop(r *http.Request) (dimension string, limit int, ok bool) {
	dimension = r.URL.Query().Get("dimension")
	if dimension == "" {
		dimension = "model"
	}
	if dimension != "model" && dimension != "token" {
		return "", 0, false
	}
	limit = 10
	if raw := r.URL.Query().Get("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 {
			return "", 0, false
		}
		limit = v
	}
	return dimension, limit, true
}

// APIActivityRollup answers GET /admin/api/activity/rollup: per-day (or
// per-hour) UTC buckets over the clamped window from the pre-aggregated
// store queries. Invalid params are a 400; store failures a 500; nil
// history answers enabled:false.
func (d *Dashboard) APIActivityRollup(w http.ResponseWriter, r *http.Request) {
	if d.hist == nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(activityRollupData{Buckets: []activityBucket{}, Granularity: "day"})
		return
	}
	since, until, ok := parseRollupWindow(r)
	if !ok {
		d.RenderResult(w, http.StatusBadRequest, false, "invalid since/until window (want 0 <= since < until)", "bad_request")
		return
	}
	granularity, ok := parseActivityGranularity(r)
	if !ok {
		d.RenderResult(w, http.StatusBadRequest, false, "invalid granularity (want day|hour)", "bad_request")
		return
	}
	out := activityRollupData{Buckets: []activityBucket{}, Enabled: true, Granularity: granularity, Since: since, Until: until}
	if granularity == "hour" {
		hours, err := d.hist.RequestsByHour(since, until)
		if err != nil {
			d.logger.Warn("activity rollup query failed", "err", err)
			d.RenderResult(w, http.StatusInternalServerError, false, "activity rollup query failed", "internal")
			return
		}
		out.Since, out.Until = since, until
		for _, h := range hours {
			out.Buckets = append(out.Buckets, activityBucket{Errors: h.Errors, TS: h.Hour, Total: h.Total})
		}
	} else {
		days, err := d.hist.RequestsByDay(since, until)
		if err != nil {
			d.logger.Warn("activity rollup query failed", "err", err)
			d.RenderResult(w, http.StatusInternalServerError, false, "activity rollup query failed", "internal")
			return
		}
		for _, day := range days {
			out.Buckets = append(out.Buckets, activityBucket{Errors: day.Errors, TS: day.Day, Total: day.Total})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// APIActivityTop answers GET /admin/api/activity/top: TopN models or token
// lanes by volume over the clamped window. Same auth/empty semantics as
// the rollup endpoint.
func (d *Dashboard) APIActivityTop(w http.ResponseWriter, r *http.Request) {
	if d.hist == nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(activityTopData{Dimension: "model", Limit: 10, Rows: []activityTopRow{}})
		return
	}
	since, until, ok := parseRollupWindow(r)
	if !ok {
		d.RenderResult(w, http.StatusBadRequest, false, "invalid since/until window (want 0 <= since < until)", "bad_request")
		return
	}
	dimension, limit, ok := parseActivityTop(r)
	if !ok {
		d.RenderResult(w, http.StatusBadRequest, false, "invalid dimension/limit (want model|token, positive limit)", "bad_request")
		return
	}
	out := activityTopData{Dimension: dimension, Enabled: true, Limit: limit, Rows: []activityTopRow{}, Since: since, Until: until}
	if dimension == "token" {
		rows, err := d.hist.TopTokens(since, until, limit)
		if err != nil {
			d.logger.Warn("activity top query failed", "err", err)
			d.RenderResult(w, http.StatusInternalServerError, false, "activity top query failed", "internal")
			return
		}
		for _, row := range rows {
			out.Rows = append(out.Rows, activityTopRow{Errors: row.Errors, Name: row.Name, Total: row.Total})
		}
	} else {
		rows, err := d.hist.TopModels(since, until, limit)
		if err != nil {
			d.logger.Warn("activity top query failed", "err", err)
			d.RenderResult(w, http.StatusInternalServerError, false, "activity top query failed", "internal")
			return
		}
		for _, row := range rows {
			out.Rows = append(out.Rows, activityTopRow{Errors: row.Errors, Name: row.Name, Total: row.Total})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
