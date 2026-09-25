package dashboard

import (
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/logring"
	"freebucks-proxy/backend/internal/pool"
	"freebucks-proxy/backend/internal/store"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// History spill path (unified store): every history-table writer feeds one
// buffered channel, and one background goroutine batch-inserts per table.
// The enqueue never blocks the caller (a full buffer drops with a counter)
// and never touches disk: mem-apply is synchronous (the pending overlay
// below), persistence rides the 100-row / 1s batch. Retention and lifecycle
// stay out of request handlers.
//
// Tables: log_entries (logring tap), request_records (chat path),
// quota_snapshots (full-view sampler), maturity_events (pool sink),
// pages_state (dashboard PUT). Readers merge the pending overlay so a
// mutation is visible to the next read synchronously; rollups stay
// mem-recomputed over persisted rows (history_rollup.go, unchanged shape).
const (
	spillBufSize      = 1024
	spillFlushSize    = 100
	spillFlushEvery   = time.Second
	spillShutdownWait = 5 * time.Second
)

// History retention: the background consumer purges rows older than these
// ages on retentionEvery. Logs and request outcomes are high-volume display
// data and follow the frozen 168h (7d) table retention; quota
// and maturity are sparse change points (90d, not tunable). Purge runs on
// the spill goroutine only — never on a request path.
var retentionEvery = time.Hour

const (
	retentionQuotaDays    = 90
	retentionMaturityDays = 90
)

// spillKind identifies one history table on the unified channel.
type spillKind uint8

const (
	spillKindLog spillKind = iota
	spillKindRequest
	spillKindQuota
	spillKindMaturity
	spillKindPage
	spillKindCount
)

// pageSpill is one pages_state upsert riding the spill.
type pageSpill struct {
	id   string
	data string
}

// spillItem is one pending history write. Exactly one payload field carries
// the row, selected by kind. Raw client tokens never ride any payload — the
// request path passes the token index plus the hex(sha256)[:16] key identity
// (ClientKeyHash, "" for bridge/no-key), mirroring the store rule.
type spillItem struct {
	kind  spillKind
	seq   uint64
	log   store.LogEntry
	req   store.RequestRecord
	quota store.QuotaSnapshot
	mat   store.MaturityEvent
	page  pageSpill
}

// WithHistory attaches the history store and starts the unified spill
// consumer. A nil store is a no-op: the dashboard runs live-only. The caller
// owns the store handle; Close stops the consumer (flushing first) and
// closes it.
func WithHistory(st *store.Store) Option {
	return func(d *Dashboard) {
		if st == nil {
			return
		}
		d.hist = st
		d.spillCh = make(chan spillItem, spillBufSize)
		d.spillDone = make(chan struct{})
		d.spillWg.Add(1)
		go d.spillLoop()
		if d.logs != nil {
			d.logs.SetSpill(d.enqueueSpill)
		}
	}
}

// enqueue is the single spill entry point: the mem-apply lands synchronously
// on the pending overlay (readers merge it, so the write is visible to the
// next read), then the item rides the channel for background persist. It
// never blocks and never touches disk; a full buffer drops with a counter
// (total + per-kind) and the overlay entry is withdrawn, so reads never show
// rows that will never persist.
func (d *Dashboard) enqueue(item spillItem) {
	if d.spillCh == nil {
		return
	}
	item.seq = d.spillSeq.Add(1)
	d.pendMu.Lock()
	d.pend = append(d.pend, item)
	d.pendMu.Unlock()
	select {
	case d.spillCh <- item:
	default:
		d.spillDropped.Add(1)
		d.spillDroppedByKind[item.kind].Add(1)
		d.dropPending(item.seq)
	}
}

// enqueueSpill is the logring tap: non-blocking, drops on a full buffer.
func (d *Dashboard) enqueueSpill(e logring.Entry) {
	if d.spillCh == nil {
		return
	}
	d.enqueue(spillItem{kind: spillKindLog, log: spillEntry(e)})
}

// EnqueueRequest stages one /v1 inference outcome for the spill. Empty
// req_ids never persist (the PRIMARY KEY cannot distinguish pre-attempt
// refusals) and are refused here, matching the store rule. The chat path
// calls this instead of a synchronous insert: no disk on the request path.
func (d *Dashboard) EnqueueRequest(rec store.RequestRecord) {
	if d.spillCh == nil || rec.ReqID == "" {
		return
	}
	d.enqueue(spillItem{kind: spillKindRequest, req: rec})
}

// EnqueueQuota stages one per-model quota sample for the spill. The
// change-point dedupe lives in sampleQuota (quotaSeen); direct callers stage
// unconditionally.
func (d *Dashboard) EnqueueQuota(q store.QuotaSnapshot) {
	if d.spillCh == nil {
		return
	}
	d.enqueue(spillItem{kind: spillKindQuota, quota: q})
}

// EnqueueMaturity stages one streak/standing event for the spill.
func (d *Dashboard) EnqueueMaturity(e store.MaturityEvent) {
	if d.spillCh == nil {
		return
	}
	d.enqueue(spillItem{kind: spillKindMaturity, mat: e})
}

// RecordMaturity implements pool.HistorySink: pool maturity events ride the
// spill instead of synchronous single-row inserts, so pool goroutines and
// admin handlers never block on disk. A nil channel (live-only) no-ops.
func (d *Dashboard) RecordMaturity(e pool.MaturityHistoryEvent) {
	if d.spillCh == nil {
		return
	}
	d.enqueue(spillItem{kind: spillKindMaturity, mat: store.MaturityEvent{
		TS:       e.TS,
		TokenIdx: e.TokenIdx,
		Kind:     e.Kind,
		Detail:   e.Detail,
	}})
}

// EnqueuePageState stages one pages_state upsert for the spill. The PUT
// handler validates the page id against its allowlist first; the store
// rejects empty ids. Visibility is synchronous via PendingPageState; the DB
// write rides the batch.
func (d *Dashboard) EnqueuePageState(id, data string) {
	if d.spillCh == nil || id == "" {
		return
	}
	d.enqueue(spillItem{kind: spillKindPage, page: pageSpill{id: id, data: data}})
}

// PendingPageState reports the newest staged (not necessarily flushed)
// snapshot for id. The pages GET handler merges this over the DB row so a
// PUT is visible to the next GET synchronously.
func (d *Dashboard) PendingPageState(id string) (string, bool) {
	d.pendMu.Lock()
	defer d.pendMu.Unlock()
	for i := len(d.pend) - 1; i >= 0; i-- {
		if d.pend[i].kind == spillKindPage && d.pend[i].page.id == id {
			return d.pend[i].page.data, true
		}
	}
	return "", false
}

// pendingKind snapshots the staged items of one kind, oldest-first.
func (d *Dashboard) pendingKind(kind spillKind) []spillItem {
	d.pendMu.Lock()
	defer d.pendMu.Unlock()
	var out []spillItem
	for _, it := range d.pend {
		if it.kind == kind {
			out = append(out, it)
		}
	}
	return out
}

// dropPending withdraws one staged item by sequence number (the buffer-full
// drop path: the row will never persist, so it must not stay visible).
func (d *Dashboard) dropPending(seq uint64) {
	d.pendMu.Lock()
	defer d.pendMu.Unlock()
	for i, it := range d.pend {
		if it.seq == seq {
			d.pend = append(d.pend[:i], d.pend[i+1:]...)
			return
		}
	}
}

// dropPendingSet withdraws every staged item in seqs (the post-flush path:
// persisted or warn-dropped, the items are no longer pending either way).
func (d *Dashboard) dropPendingSet(seqs map[uint64]struct{}) {
	if len(seqs) == 0 {
		return
	}
	d.pendMu.Lock()
	defer d.pendMu.Unlock()
	kept := d.pend[:0]
	for _, it := range d.pend {
		if _, ok := seqs[it.seq]; !ok {
			kept = append(kept, it)
		}
	}
	d.pend = kept
}

// spillPending reports staged plus buffered-but-unconsumed items. Tests poll
// it to zero instead of sleeping: background delivery is bounded by the 1s
// flush tick, and shutdown drains deterministically.
func (d *Dashboard) spillPending() int {
	d.pendMu.Lock()
	defer d.pendMu.Unlock()
	return len(d.pend) + len(d.spillCh)
}

// SpillPending reports staged plus buffered-but-unconsumed items.
// Test-only: cross-package callers (server invariant tests) poll it to zero
// instead of sleeping.
func (d *Dashboard) SpillPending() int {
	return d.spillPending()
}

func (d *Dashboard) spillLoop() {
	defer d.spillWg.Done()
	batch := make([]spillItem, 0, spillFlushSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		d.flushSpillBatch(batch)
		batch = batch[:0]
	}
	tick := time.NewTicker(spillFlushEvery)
	defer tick.Stop()
	retention := time.NewTicker(retentionEvery)
	defer retention.Stop()
	for {
		select {
		case e, ok := <-d.spillCh:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= spillFlushSize {
				flush()
			}
		case <-tick.C:
			flush()
		case <-retention.C:
			flush()
			d.purgeHistory()
		case <-d.spillDone:
			for {
				select {
				case e := <-d.spillCh:
					batch = append(batch, e)
					if len(batch) >= spillFlushSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// flushSpillBatch persists one mixed batch per table (single transaction
// each) and withdraws every staged item from the pending overlay — flushed
// or warn-dropped, the items are no longer pending either way. Failures only
// warn: history is display data, and the next tick retries with fresh rows.
func (d *Dashboard) flushSpillBatch(batch []spillItem) {
	if len(batch) == 0 {
		return
	}
	var logs []store.LogEntry
	var reqs []store.RequestRecord
	var quotas []store.QuotaSnapshot
	var mats []store.MaturityEvent
	var pages []pageSpill
	seqs := make(map[uint64]struct{}, len(batch))
	for _, it := range batch {
		seqs[it.seq] = struct{}{}
		switch it.kind {
		case spillKindLog:
			logs = append(logs, it.log)
		case spillKindRequest:
			reqs = append(reqs, it.req)
		case spillKindQuota:
			quotas = append(quotas, it.quota)
		case spillKindMaturity:
			mats = append(mats, it.mat)
		case spillKindPage:
			pages = append(pages, it.page)
		}
	}
	if len(logs) > 0 {
		if err := d.hist.AppendLogs(logs); err != nil {
			d.logger.Warn("history spill failed", "err", err, "table", "log_entries", "rows", len(logs))
		}
	}
	if len(reqs) > 0 {
		if err := d.hist.AppendRequests(reqs); err != nil {
			d.logger.Warn("history spill failed", "err", err, "table", "request_records", "rows", len(reqs))
		}
	}
	if len(quotas) > 0 {
		if err := d.hist.AppendQuotas(quotas); err != nil {
			d.logger.Warn("history spill failed", "err", err, "table", "quota_snapshots", "rows", len(quotas))
		}
	}
	if len(mats) > 0 {
		if err := d.hist.AppendMaturities(mats); err != nil {
			d.logger.Warn("history spill failed", "err", err, "table", "maturity_events", "rows", len(mats))
		}
	}
	for _, p := range pages {
		if err := d.hist.PutPageState(p.id, p.data); err != nil {
			d.logger.Warn("history spill failed", "err", err, "table", "pages_state", "page", p.id)
		}
	}
	d.dropPendingSet(seqs)
}

// purgeHistory deletes history rows older than the retention ages. Called
// from the spill goroutine on retentionEvery — never from request handlers.
// Failures only warn: history is display data, and the next tick retries.
func (d *Dashboard) purgeHistory() {
	if d.hist == nil {
		return
	}
	now := store.Millis(time.Now())
	day := int64(24 * time.Hour / time.Millisecond)
	// The table retention is frozen at the 168h (7d) default: the
	// LOG_TABLE_RETENTION knob is excised, so the purge never reads the
	// config here and can never resolve to "delete everything".
	tableRetention := config.DefaultLogTableRetention
	tableBefore := now - tableRetention.Milliseconds()
	if err := d.hist.Purge(
		tableBefore,
		now-retentionQuotaDays*day,
		now-retentionMaturityDays*day,
		tableBefore,
	); err != nil {
		d.logger.Warn("history purge failed", "err", err)
	}
}

// spillEntry converts one ring record to the store domain. TS falls back to
// now when the ring timestamp is unparsable; level is lowercased to the
// store's filter domain; req_id is recovered from the flattened fields when
// the handler logged one.
func spillEntry(e logring.Entry) store.LogEntry {
	ts := store.Millis(time.Now())
	if t, err := time.Parse(time.RFC3339, e.Time); err == nil {
		ts = store.Millis(t)
	}
	return store.LogEntry{
		TS:     ts,
		Level:  strings.ToLower(e.Level),
		Msg:    e.Message,
		Fields: strings.Join(e.Fields, "\n"),
		ReqID:  spillReqID(e.Fields),
	}
}

func spillReqID(fields []string) string {
	for _, f := range fields {
		if v, ok := strings.CutPrefix(f, "req_id="); ok {
			return v
		}
	}
	return ""
}

// spillStats reports consumer health for tests and the dashboard itself.
func (d *Dashboard) spillStats() (dropped int64) {
	return d.spillDropped.Load()
}

// SpillDroppedByKind reports buffer-full drops for one history table.
// Test-only.
func (d *Dashboard) SpillDroppedByKind(kind spillKind) int64 {
	return d.spillDroppedByKind[kind].Load()
}

// Close stops the history spill consumer (flushing what is buffered) and
// closes the history store. Safe to call without WithHistory.
func (d *Dashboard) Close() error {
	if d.spillCh != nil {
		close(d.spillDone)
		done := make(chan struct{})
		go func() { d.spillWg.Wait(); done <- struct{}{} }()
		select {
		case <-done:
		case <-time.After(spillShutdownWait):
		}
		if d.logs != nil {
			d.logs.SetSpill(nil)
		}
	}
	if d.hist != nil {
		return d.hist.Close()
	}
	return nil
}

// quotaKey identifies one sampled quota row; quotaPoint is its staged
// state. The full-view sampler (tokensData) records a row only when the
// point differs from the last staged one, so history holds change points
// rather than per-poll duplicates. The hot poll never samples.
type quotaKey struct {
	idx   int
	model string
}

type quotaPoint struct {
	recent  float64
	limit   float64
	resetAt int64
}

// sampleQuota stages one quota row when its state changed since the last
// sample. No-op without a history store. The quotaSeen mem-swap lands
// synchronously (history holds change points, not per-poll duplicates) and
// the row rides the spill: no disk on the full-view path (once per mount +
// 5min cadence + mutations), never on the 10s hot poll.
func (d *Dashboard) sampleQuota(idx int, model string, limit, recent float64, resetAt time.Time, entitlements string) {
	if d.hist == nil {
		return
	}
	pt := quotaPoint{recent: recent, limit: limit, resetAt: store.Millis(resetAt)}
	key := quotaKey{idx: idx, model: model}
	d.quotaSeenMu.Lock()
	if old, ok := d.quotaSeen[key]; ok && old == pt {
		d.quotaSeenMu.Unlock()
		return
	}
	if d.quotaSeen == nil {
		d.quotaSeen = make(map[quotaKey]quotaPoint)
	}
	d.quotaSeen[key] = pt
	d.quotaSeenMu.Unlock()
	d.EnqueueQuota(store.QuotaSnapshot{
		TS: store.Millis(time.Now()), TokenIdx: idx, Model: model,
		Limit: limit, Recent: recent, ResetAt: pt.resetAt, Entitlements: entitlements,
	})
}

// --- history queries (ADR-0016) ---
//
// Read-only views over the history store. Every shape carries Enabled: false
// with empty rows when the dashboard runs live-only (nil store), so the SPA
// renders its live views without branching on endpoint availability.
// Missing/invalid filter params yield empty rows, never an error: the pages
// always pass them, and a 200 with no rows is the honest answer for "no
// samples yet".

type quotaSample struct {
	TS           int64   `json:"ts"`
	Limit        float64 `json:"limit"`
	Recent       float64 `json:"recent"`
	ResetAt      int64   `json:"reset_at"`
	Entitlements string  `json:"entitlements,omitempty"`
}

type quotaHistoryData struct {
	Enabled   bool          `json:"enabled"`
	Token     int           `json:"token"`
	Model     string        `json:"model"`
	Snapshots []quotaSample `json:"snapshots"`
}

type maturityHistoryItem struct {
	TS     int64  `json:"ts"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

type maturityHistoryData struct {
	Enabled bool                  `json:"enabled"`
	Token   int                   `json:"token"`
	Events  []maturityHistoryItem `json:"events"`
}

type logHistoryItem struct {
	TS     int64  `json:"ts"`
	Level  string `json:"level"`
	Msg    string `json:"msg"`
	Fields string `json:"fields"`
	ReqID  string `json:"req_id,omitempty"`
}

type logsHistoryData struct {
	Enabled bool             `json:"enabled"`
	Entries []logHistoryItem `json:"entries"`
}

func queryInt(r *http.Request, key string, def int) int {
	if r == nil || r.URL == nil {
		return def
	}
	v, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		return def
	}
	return v
}

func queryInt64(r *http.Request, key string, def int64) int64 {
	if r == nil || r.URL == nil {
		return def
	}
	v, err := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	if err != nil {
		return def
	}
	return v
}

func (d *Dashboard) quotaHistoryData(r *http.Request) quotaHistoryData {
	qd := quotaHistoryData{Token: -1}
	if r != nil && r.URL != nil {
		qd.Token = queryInt(r, "token", -1)
		qd.Model = r.URL.Query().Get("model")
	}
	if d.hist == nil {
		return qd
	}
	qd.Enabled = true
	if qd.Token < 0 || qd.Model == "" {
		return qd
	}
	since := queryInt64(r, "since", 0)
	limit := queryInt(r, "limit", 500)
	rows, err := d.hist.QuotaHistory(qd.Token, qd.Model, since, limit)
	if err != nil {
		d.logger.Warn("quota history query failed", "err", err)
		return qd
	}
	for _, s := range rows {
		qd.Snapshots = append(qd.Snapshots, quotaSample{
			TS:           s.TS,
			Limit:        s.Limit,
			Recent:       s.Recent,
			ResetAt:      s.ResetAt,
			Entitlements: s.Entitlements,
		})
	}
	// The pending overlay merges staged-but-unflushed rows so a sample is
	// visible to the next read synchronously; the flush withdraws them, so
	// no row ever renders twice. Same oldest-first shape, same limit cap.
	for _, it := range d.pendingKind(spillKindQuota) {
		q := it.quota
		if q.TokenIdx != qd.Token || q.Model != qd.Model || q.TS < since {
			continue
		}
		qd.Snapshots = append(qd.Snapshots, quotaSample{
			TS:           q.TS,
			Limit:        q.Limit,
			Recent:       q.Recent,
			ResetAt:      q.ResetAt,
			Entitlements: q.Entitlements,
		})
	}
	sort.SliceStable(qd.Snapshots, func(i, j int) bool { return qd.Snapshots[i].TS < qd.Snapshots[j].TS })
	if limit > 0 && len(qd.Snapshots) > limit {
		qd.Snapshots = qd.Snapshots[:limit]
	}
	return qd
}

func (d *Dashboard) maturityHistoryData(r *http.Request) maturityHistoryData {
	md := maturityHistoryData{Token: -1}
	if r != nil && r.URL != nil {
		md.Token = queryInt(r, "token", -1)
	}
	if d.hist == nil {
		return md
	}
	md.Enabled = true
	if md.Token < 0 {
		return md
	}
	since := queryInt64(r, "since", 0)
	limit := queryInt(r, "limit", 200)
	rows, err := d.hist.MaturityHistory(md.Token, since, limit)
	if err != nil {
		d.logger.Warn("maturity history query failed", "err", err)
		return md
	}
	for _, e := range rows {
		md.Events = append(md.Events, maturityHistoryItem{TS: e.TS, Kind: e.Kind, Detail: e.Detail})
	}
	// Pending overlay: same sync-visibility merge as the quota reader.
	for _, it := range d.pendingKind(spillKindMaturity) {
		e := it.mat
		if e.TokenIdx != md.Token || e.TS < since {
			continue
		}
		md.Events = append(md.Events, maturityHistoryItem{TS: e.TS, Kind: e.Kind, Detail: e.Detail})
	}
	sort.SliceStable(md.Events, func(i, j int) bool { return md.Events[i].TS < md.Events[j].TS })
	if limit > 0 && len(md.Events) > limit {
		md.Events = md.Events[:limit]
	}
	return md
}

func (d *Dashboard) logsHistoryData(r *http.Request) logsHistoryData {
	ld := logsHistoryData{}
	if d.hist == nil {
		return ld
	}
	ld.Enabled = true
	f := store.LogFilter{Limit: queryInt(r, "limit", 500)}
	if r != nil && r.URL != nil {
		q := r.URL.Query()
		f.Level = q.Get("level")
		f.Contains = q.Get("msg")
		f.ReqID = q.Get("req_id")
		f.Since = queryInt64(r, "since", 0)
		f.Until = queryInt64(r, "until", 0)
	}
	rows, err := d.hist.QueryLogs(f)
	if err != nil {
		d.logger.Warn("logs history query failed", "err", err)
		return ld
	}
	for _, e := range rows {
		ld.Entries = append(ld.Entries, logHistoryItem{
			TS: e.TS, Level: e.Level, Msg: e.Msg, Fields: e.Fields, ReqID: e.ReqID,
		})
	}
	// Pending overlay: staged-but-unflushed rows merge newest-first under the
	// same filter, so a ring record is visible here before the batch lands.
	// The flush withdraws staged rows, so none renders twice. Contains
	// matches case-insensitively, mirroring SQLite LIKE semantics.
	limit := f.Limit
	if limit <= 0 {
		limit = 500
	}
	if limit > 5000 {
		limit = 5000
	}
	for _, it := range d.pendingKind(spillKindLog) {
		e := it.log
		if f.Since > 0 && e.TS < f.Since {
			continue
		}
		if f.Until > 0 && e.TS > f.Until {
			continue
		}
		if f.Level != "" && !strings.EqualFold(e.Level, f.Level) {
			continue
		}
		if f.ReqID != "" && e.ReqID != f.ReqID {
			continue
		}
		if f.Contains != "" && !strings.Contains(strings.ToLower(e.Msg), strings.ToLower(f.Contains)) && !strings.Contains(strings.ToLower(e.Fields), strings.ToLower(f.Contains)) {
			continue
		}
		ld.Entries = append(ld.Entries, logHistoryItem{
			TS: e.TS, Level: e.Level, Msg: e.Msg, Fields: e.Fields, ReqID: e.ReqID,
		})
	}
	sort.SliceStable(ld.Entries, func(i, j int) bool { return ld.Entries[i].TS > ld.Entries[j].TS })
	if len(ld.Entries) > limit {
		ld.Entries = ld.Entries[:limit]
	}
	return ld
}
