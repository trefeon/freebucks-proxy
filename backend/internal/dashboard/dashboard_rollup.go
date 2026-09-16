// Logs rollup + export/import handlers (read path over the history store).
//
// GET /admin/api/logs/rollup answers the Logs Metrics panel from EXISTING
// request_records columns only: counts, per-model/per-token GROUP BYs, error
// rates, upstream-TTFB percentiles, and millis time buckets. Total latency,
// HTTP codes, pre-attempt refusals, and token volumes are honest gaps — the
// columns cannot answer them, so the wire carries no such fields.
//
// GET /admin/api/logs/export streams request_records plus log_entries (the
// two tables the rollup reads) as ONE versioned JSON document, encoded
// directly to the ResponseWriter — never buffered. POST /admin/api/logs/import
// restores exactly that document with INSERT OR IGNORE (gap-fill only, never
// overwriting a live row) behind a 64MB body cap. Imported old rows age under
// the normal 168h purge like any other row.
//
// Auth: all three are Auth=sensitive manifest rows (same guard as
// logs/history). A nil history store answers enabled:false with empty rows,
// never an error — the SPA renders its live views without branching.
package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"freebuff-proxy/backend/internal/store"
)

// logsExportVersion is the only export document version import accepts.
const logsExportVersion = 1

// logsImportMaxBytes caps the import body: 64MB, enforced by
// http.MaxBytesReader — the cap IS the row limit, no second knob.
const logsImportMaxBytes = 64 << 20

// defaultRollupBucketMs is the time-bucket width when ?bucket_ms is absent.
const defaultRollupBucketMs = 300000

// --- rollup wire (JSON-alphabetical, per the admin_wire.go rule) ---

type rollupNameCount struct {
	Errors int64  `json:"errors"`
	Name   string `json:"name"`
	Total  int64  `json:"total"`
}

type rollupTokenCount struct {
	Errors   int64 `json:"errors"`
	TokenIdx int   `json:"token_idx"`
	Total    int64 `json:"total"`
}

type rollupTTFB struct {
	Count int64 `json:"count"`
	P50   int64 `json:"p50"`
	P90   int64 `json:"p90"`
	P99   int64 `json:"p99"`
}

type rollupBucket struct {
	Errors int64 `json:"errors"`
	OK     int64 `json:"ok"`
	Total  int64 `json:"total"`
	TS     int64 `json:"ts"`
}

type logsRollupData struct {
	BucketMs  int64              `json:"bucket_ms"`
	Buckets   []rollupBucket     `json:"buckets"`
	ByError   []rollupNameCount  `json:"by_error"`
	ByModel   []rollupNameCount  `json:"by_model"`
	ByToken   []rollupTokenCount `json:"by_token"`
	Enabled   bool               `json:"enabled"`
	ErrorRate float64            `json:"error_rate"`
	Errors    int64              `json:"errors"`
	OK        int64              `json:"ok"`
	Since     int64              `json:"since"`
	Total     int64              `json:"total"`
	TTFB      rollupTTFB         `json:"ttfb"`
	Until     int64              `json:"until"`
}

// --- export/import wire ---

type exportRequestRecord struct {
	Endpoint string `json:"endpoint"`
	Err      string `json:"error"`
	Model    string `json:"model"`
	ReqID    string `json:"req_id"`
	Status   string `json:"status"`
	TokenIdx int    `json:"token_idx"`
	TS       int64  `json:"ts"`
	TTFBms   int64  `json:"ttfb_ms"`
}

type exportLogEntry struct {
	Fields string `json:"fields"`
	ID     int64  `json:"id"`
	Level  string `json:"level"`
	Msg    string `json:"msg"`
	ReqID  string `json:"req_id"`
	TS     int64  `json:"ts"`
}

// logsExportDoc is the versioned export document AND the exact shape import
// accepts — one struct, so export output always decodes as import input.
type logsExportDoc struct {
	Enabled        bool                  `json:"enabled"`
	ExportedAt     int64                 `json:"exported_at"`
	LogEntries     []exportLogEntry      `json:"log_entries"`
	RequestRecords []exportRequestRecord `json:"request_records"`
	Version        int                   `json:"version"`
}

type logsImportReport struct {
	Imported         int64 `json:"imported"`
	Rejected         int64 `json:"rejected"`
	SkippedDuplicate int64 `json:"skipped_duplicate"`
}

// parseRollupWindow resolves the shared since/until params. Absent since
// means 0 (the store clamps to the 168h bound); absent until means now.
// Malformed or negative values reject — the pages always pass sane values,
// and a 400 beats silently answering a different window.
func parseRollupWindow(r *http.Request) (since, until int64, ok bool) {
	since, until = 0, store.Millis(time.Now())
	if r == nil || r.URL == nil {
		return since, until, true
	}
	q := r.URL.Query()
	if v := q.Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, 0, false
		}
		since = n
	}
	if v := q.Get("until"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, 0, false
		}
		until = n
	}
	if since >= until {
		return 0, 0, false
	}
	return since, until, true
}

// parseRollupBucket resolves ?bucket_ms: absent means the default, explicit
// values must be positive.
func parseRollupBucket(r *http.Request) (int64, bool) {
	if r == nil || r.URL == nil || r.URL.Query().Get("bucket_ms") == "" {
		return defaultRollupBucketMs, true
	}
	n, err := strconv.ParseInt(r.URL.Query().Get("bucket_ms"), 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func emptyRollup() logsRollupData {
	return logsRollupData{
		BucketMs: defaultRollupBucketMs,
		Buckets:  []rollupBucket{},
		ByError:  []rollupNameCount{},
		ByModel:  []rollupNameCount{},
		ByToken:  []rollupTokenCount{},
	}
}

// APILogsRollup answers GET /admin/api/logs/rollup: the clamped-window
// aggregation over request_records. Invalid params are a 400 (rejected,
// never silently reinterpreted); store failures are a 500.
func (d *Dashboard) APILogsRollup(w http.ResponseWriter, r *http.Request) {
	if d.hist == nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(emptyRollup())
		return
	}
	since, until, ok := parseRollupWindow(r)
	if !ok {
		d.RenderResult(w, http.StatusBadRequest, false, "invalid since/until window (want 0 <= since < until)", "bad_request")
		return
	}
	bucket, ok := parseRollupBucket(r)
	if !ok {
		d.RenderResult(w, http.StatusBadRequest, false, "invalid bucket_ms (want a positive integer)", "bad_request")
		return
	}
	res, err := d.hist.RequestsRollup(since, until, bucket)
	if err != nil {
		d.logger.Warn("logs rollup query failed", "err", err)
		d.RenderResult(w, http.StatusInternalServerError, false, "logs rollup query failed", "internal")
		return
	}
	out := logsRollupData{
		BucketMs: res.BucketMs, Enabled: true, ErrorRate: res.ErrorRate,
		Errors: res.Errors, OK: res.OK, Since: res.Since,
		Total: res.Total, Until: res.Until,
		TTFB:    rollupTTFB{Count: res.TTFB.Count, P50: res.TTFB.P50, P90: res.TTFB.P90, P99: res.TTFB.P99},
		Buckets: []rollupBucket{}, ByError: []rollupNameCount{},
		ByModel: []rollupNameCount{}, ByToken: []rollupTokenCount{},
	}
	for _, g := range res.ByModel {
		out.ByModel = append(out.ByModel, rollupNameCount{Errors: g.Errors, Name: g.Name, Total: g.Total})
	}
	for _, g := range res.ByToken {
		out.ByToken = append(out.ByToken, rollupTokenCount{Errors: g.Errors, TokenIdx: g.TokenIdx, Total: g.Total})
	}
	for _, g := range res.ByError {
		out.ByError = append(out.ByError, rollupNameCount{Errors: g.Errors, Name: g.Name, Total: g.Total})
	}
	for _, b := range res.Buckets {
		out.Buckets = append(out.Buckets, rollupBucket{Errors: b.Errors, OK: b.OK, Total: b.Total, TS: b.TS})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// APILogsExport answers GET /admin/api/logs/export: the versioned document
// streamed row-by-row (encode directly to the ResponseWriter — a full 168h
// window is never buffered). Same window params as the rollup.
func (d *Dashboard) APILogsExport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if d.hist == nil {
		_ = json.NewEncoder(w).Encode(logsExportDoc{
			LogEntries: []exportLogEntry{}, RequestRecords: []exportRequestRecord{},
			Version: logsExportVersion,
		})
		return
	}
	since, until, ok := parseRollupWindow(r)
	if !ok {
		d.RenderResult(w, http.StatusBadRequest, false, "invalid since/until window (want 0 <= since < until)", "bad_request")
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename=\"logs-export-"+strconv.FormatInt(store.Millis(time.Now()), 10)+".json\"")
	enc := json.NewEncoder(w)
	if _, err := io.WriteString(w, `{"enabled":true,"exported_at":`+strconv.FormatInt(store.Millis(time.Now()), 10)+`,"log_entries":[`); err != nil {
		d.logger.Warn("logs export write failed", "err", err)
		return
	}
	first := true
	if err := d.hist.ExportLogs(since, until, func(e store.LogEntry) error {
		if !first {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		first = false
		return enc.Encode(exportLogEntry{
			Fields: e.Fields, ID: e.ID, Level: e.Level, Msg: e.Msg, ReqID: e.ReqID, TS: e.TS,
		})
	}); err != nil {
		d.logger.Warn("logs export query failed", "err", err)
		return
	}
	if _, err := io.WriteString(w, `],"request_records":[`); err != nil {
		d.logger.Warn("logs export write failed", "err", err)
		return
	}
	first = true
	if err := d.hist.ExportRequests(since, until, func(rec store.RequestRecord) error {
		if !first {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		first = false
		return enc.Encode(exportRequestRecord{
			Endpoint: rec.Endpoint, Err: rec.Err, Model: rec.Model, ReqID: rec.ReqID,
			Status: rec.Status, TokenIdx: rec.TokenIdx, TS: rec.TS, TTFBms: rec.TTFBms,
		})
	}); err != nil {
		d.logger.Warn("logs export query failed", "err", err)
		return
	}
	if _, err := io.WriteString(w, `],"version":`+strconv.Itoa(logsExportVersion)+`}`); err != nil {
		d.logger.Warn("logs export write failed", "err", err)
	}
}

// isImportBodyTooLarge reports a MaxBytesReader limiter trip (net/http
// surfaces it as "http: request body too large" through the JSON decoder).
func isImportBodyTooLarge(err error) bool {
	return err != nil && strings.Contains(err.Error(), "request body too large")
}

// APILogsImport answers POST /admin/api/logs/import: restores exactly the
// export document. Wrong/missing version rejects; rows failing column
// validation count as rejected without aborting the batch; writes are
// INSERT OR IGNORE so live rows are never overwritten. Over-cap bodies
// (64MB) answer 413.
func (d *Dashboard) APILogsImport(w http.ResponseWriter, r *http.Request) {
	if d.hist == nil {
		d.RenderResult(w, http.StatusServiceUnavailable, false, "history store is not configured", "history_disabled")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, logsImportMaxBytes)
	var doc logsExportDoc
	if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
		if isImportBodyTooLarge(err) {
			d.RenderResult(w, http.StatusRequestEntityTooLarge, false, "import body exceeds 64MB", "body_too_large")
			return
		}
		d.RenderResult(w, http.StatusBadRequest, false, "invalid import document (want the logs export JSON)", "bad_request")
		return
	}
	if doc.Version != logsExportVersion {
		d.RenderResult(w, http.StatusBadRequest, false, "unsupported import version "+strconv.Itoa(doc.Version)+" (want 1)", "bad_version")
		return
	}
	reqs := make([]store.RequestRecord, 0, len(doc.RequestRecords))
	for _, rec := range doc.RequestRecords {
		reqs = append(reqs, store.RequestRecord{
			ReqID: rec.ReqID, TS: rec.TS, Endpoint: rec.Endpoint, Model: rec.Model,
			TokenIdx: rec.TokenIdx, Status: rec.Status, TTFBms: rec.TTFBms, Err: rec.Err,
		})
	}
	logs := make([]store.LogEntry, 0, len(doc.LogEntries))
	for _, e := range doc.LogEntries {
		logs = append(logs, store.LogEntry{
			ID: e.ID, TS: e.TS, Level: e.Level, Msg: e.Msg, Fields: e.Fields, ReqID: e.ReqID,
		})
	}
	reqCounts, err := d.hist.ImportRequests(reqs)
	if err != nil {
		d.logger.Warn("logs import requests failed", "err", err)
		d.RenderResult(w, http.StatusInternalServerError, false, "logs import failed", "internal")
		return
	}
	logCounts, err := d.hist.ImportLogs(logs)
	if err != nil {
		d.logger.Warn("logs import logs failed", "err", err)
		d.RenderResult(w, http.StatusInternalServerError, false, "logs import failed", "internal")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(logsImportReport{
		Imported:         reqCounts.Imported + logCounts.Imported,
		Rejected:         reqCounts.Rejected + logCounts.Rejected,
		SkippedDuplicate: reqCounts.SkippedDuplicate + logCounts.SkippedDuplicate,
	})
}
