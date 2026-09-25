package server

// Per-page UI state endpoints (slice 4): display-only dashboard snapshots
// that survive a gateway restart (last-visited hash, expanded rows, filter
// text). The SPA stays fetch-only: every page loads its snapshot on mount
// and saves it back debounced; failures are warn-only client-side.
//
// Unified store: the snapshot is mem-authoritative (pageStateMem below).
// Reads serve from mem with no DB I/O; PUT swaps mem synchronously (the
// next GET sees it) and spills to pages_state behind via the recorded
// spill pattern (buffered channel, batch 100/1s, drop counter). Boot
// snapshots the DB into mem once (lazy on first page request); restart
// recovery rides the spill (I3), and pages never touch .env (I4 vacuous).
//
//	GET /admin/api/pages/{id}  {data} (absent row -> {}, never 404)
//	PUT /admin/api/pages/{id}  {data} upserts (data must be a JSON object,
//	64KB cap; envelopes over the 72KB body limiter 413 as page_too_large)
//
// Unknown ids 404 against the allowlist below. A nil store (DB failed at
// boot) degrades GET to {} and 503s PUT — the same split the settings
// overlay uses, so a write never silently lands nowhere. PUT carries the
// CSRF gate like every other state-changing admin row.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"freebucks-proxy/backend/internal/dashboard"
	"freebucks-proxy/backend/internal/store"
)

const maxPageStateBytes = 64 << 10

// validPageIDs is the pages_state allowlist: every NAV_ITEMS id from the SPA
// nav registry (frontend/src/lib/nav.js) plus "shell" — chrome state that is
// not a page itself (currently just {lastHash}, restored by App.svelte on
// boot). The four deep-link-only ids (setup/metrics/traces/playground,
// inSidebar:false, with playground aliasing the DevTools component) persist
// exactly like sidebar pages: the shell restores lastHash across them and
// each mounts its own snapshot key, so rejecting them would 404 real visits.
var validPageIDs = map[string]bool{
	"ads":        true,
	"overview":   true,
	"tokens":     true,
	"maturity":   true,
	"quota":      true,
	"models":     true,
	"logs":       true,
	"settings":   true,
	"devtools":   true,
	"setup":      true,
	"metrics":    true,
	"traces":     true,
	"playground": true,
	"shell":      true,
}

// Page-state spill tuning (recorded pattern: spillCh 1024, batch 100/1s,
// drop counter — dashboard_history.go:19-67). pageSpillFlushEvery is a var
// so plane tests can pin the sync/async boundary deterministically.
const (
	pageSpillBufSize   = 1024
	pageSpillFlushSize = 100
)

var pageSpillFlushEvery = time.Second

// pageSpillEntry is one staged snapshot write: the page id plus its raw
// JSON document (validated as an object by the PUT handler).
type pageSpillEntry struct {
	id   string
	data string
}

// pageStateMem is the mem-authoritative pages_state snapshot. swap/get are
// synchronous under an RWMutex (I1: a PUT is visible to the next GET);
// persistence flows through the buffered spill channel drained by run
// (I2: no disk I/O on the request path). persist is injectable so plane
// tests count writes without a database.
type pageStateMem struct {
	mu      sync.RWMutex
	snap    map[string]string
	st      *store.Store
	ch      chan pageSpillEntry
	dropped atomic.Int64
	persist func(id, data string) error
}

func newPageStateMem(st *store.Store) *pageStateMem {
	m := &pageStateMem{
		snap: make(map[string]string),
		st:   st,
		ch:   make(chan pageSpillEntry, pageSpillBufSize),
	}
	if st != nil {
		st := st
		m.persist = func(id, data string) error { return st.PutPageState(id, data) }
	} else {
		m.persist = func(id, data string) error { return nil }
	}
	return m
}

// pageMem returns the handler's snapshot, building it once from the DB
// (boot snapshot to mem). A post-boot store swap (tests attaching a store
// after construction) rebuilds it so reads never serve a stale handle's
// rows.
func (a *adminHandlers) pageMem() *pageStateMem {
	a.pagesMu.Lock()
	defer a.pagesMu.Unlock()
	if a.pages != nil && a.pages.st == a.settings {
		return a.pages
	}
	m := newPageStateMem(a.settings)
	if a.settings != nil {
		for id := range validPageIDs {
			if data, ok, err := a.settings.GetPageState(id); err == nil && ok {
				m.snap[id] = data
			}
		}
		go m.run(a.logfunc)
	}
	a.pages = m
	return m
}

func (m *pageStateMem) swap(id, data string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snap[id] = data
}

func (m *pageStateMem) get(id string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	data, ok := m.snap[id]
	return data, ok
}

// enqueue stages one spill write without blocking the request: a full
// buffer drops with a counter, exactly like the history spill.
func (m *pageStateMem) enqueue(e pageSpillEntry) {
	select {
	case m.ch <- e:
	default:
		m.dropped.Add(1)
	}
}

func (m *pageStateMem) droppedCount() int64 {
	return m.dropped.Load()
}

// run drains the spill channel into the store: up to pageSpillFlushSize
// rows per batch, flushed every pageSpillFlushEvery. Last write per page
// wins within a batch. Errors warn (the mem snapshot stays authoritative;
// the next PUT re-stages the row).
func (m *pageStateMem) run(logfunc func() *slog.Logger) {
	batch := make(map[string]string, pageSpillFlushSize)
	var timer *time.Timer
	var tick <-chan time.Time
	flush := func() {
		for id, data := range batch {
			if err := m.persist(id, data); err != nil {
				if logfunc != nil {
					if l := logfunc(); l != nil {
						l.Warn("page state spill failed", "page", id, "err", err)
					}
				}
			}
			delete(batch, id)
		}
		if timer != nil {
			timer.Stop()
			timer = nil
			tick = nil
		}
	}
	for {
		select {
		case e, ok := <-m.ch:
			if !ok {
				flush()
				return
			}
			batch[e.id] = e.data
			if len(batch) >= pageSpillFlushSize {
				flush()
			} else if timer == nil {
				timer = time.NewTimer(pageSpillFlushEvery)
				tick = timer.C
			}
		case <-tick:
			flush()
		}
	}
}

func (a *adminHandlers) handlePageStateGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validPageIDs[id] {
		a.dash.RenderResult(w, http.StatusNotFound, false, "Unknown page "+id+".", "unknown_page")
		return
	}
	// Server-side DEVTOOLS_ENABLED gate: the devtools/playground snapshots
	// belong to hidden pages; a direct GET must 404 when the knob is off.
	if (id == "devtools" || id == "playground") && !a.cfgLoad().DevToolsEnabled {
		a.dash.RenderResult(w, http.StatusNotFound, false, "dev tools are disabled — set DEVTOOLS_ENABLED=true to enable the playground", "devtools_disabled")
		return
	}
	if a.settings == nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
		return
	}
	// Mem-authoritative read: no DB I/O on the request path. Stored rows
	// are validated JSON on PUT (and on boot snapshot), so echoing the
	// snapshot verbatim is safe.
	if data, ok := a.pageMem().get(id); ok && data != "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":` + data + `}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"data":{}}`))
}

func (a *adminHandlers) handlePageStatePut(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validPageIDs[id] {
		a.dash.RenderResult(w, http.StatusNotFound, false, "Unknown page "+id+".", "unknown_page")
		return
	}
	// Server-side DEVTOOLS_ENABLED gate: the devtools/playground snapshots
	// belong to hidden pages; a direct PUT must 404 when the knob is off.
	// It leads the store check so the knob wins even on a live-only gateway.
	if (id == "devtools" || id == "playground") && !a.cfgLoad().DevToolsEnabled {
		a.dash.RenderResult(w, http.StatusNotFound, false, "dev tools are disabled — set DEVTOOLS_ENABLED=true to enable the playground", "devtools_disabled")
		return
	}
	if a.settings == nil {
		a.dash.RenderResult(w, http.StatusServiceUnavailable, false,
			"Page state store unavailable — the dashboard runs live-only (the DB failed to open at boot).", "pages_unavailable")
		return
	}
	// Envelope slack above the 64KB data cap: the cap applies to data, not
	// the {"data":...} wrapper.
	r.Body = http.MaxBytesReader(w, r.Body, (maxPageStateBytes + 8<<10))
	var req dashboard.PageStateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// An envelope over the 72KB body limiter trips MaxBytesReader: the
		// client blew past even the wrapper slack, so it keys truncation
		// on page_too_large (413), not a generic bad_request (400).
		if isBodyTooLarge(err) {
			a.dash.RenderResult(w, http.StatusRequestEntityTooLarge, false, "Page state exceeds 64KB.", "page_too_large")
			return
		}
		a.dash.RenderResult(w, http.StatusBadRequest, false, "Invalid JSON body (want {data}).", "bad_request")
		return
	}
	if len(req.Data) == 0 {
		a.dash.RenderResult(w, http.StatusBadRequest, false, "Missing data (want {data}).", "bad_request")
		return
	}
	// Every consumer (loadPageState's object merge, the shell restore)
	// expects an object: null/number/string/array snapshots would echo back
	// verbatim and break the spread on the next load.
	if !isJSONObject(req.Data) {
		a.dash.RenderResult(w, http.StatusBadRequest, false, "Page state data must be a JSON object (want {data: {...}}).", "bad_request")
		return
	}
	if len(req.Data) > maxPageStateBytes {
		a.dash.RenderResult(w, http.StatusRequestEntityTooLarge, false, "Page state exceeds 64KB.", "page_too_large")
		return
	}
	// Sync apply + spill persist: the snapshot swaps in-request (the next
	// GET sees it) while the DB write goes through the spill channel. The
	// handler returns the applied receipt without waiting on disk.
	mem := a.pageMem()
	mem.swap(id, string(req.Data))
	mem.enqueue(pageSpillEntry{id: id, data: string(req.Data)})
	a.dash.RenderResult(w, http.StatusOK, true, "Page state saved.", "page_saved")
}

// isBodyTooLarge reports a MaxBytesReader limiter trip (net/http surfaces it
// as "http: request body too large" through the JSON decoder).
func isBodyTooLarge(err error) bool {
	return err != nil && strings.Contains(err.Error(), "request body too large")
}

// isJSONObject reports whether raw JSON is an object (leading whitespace
// skipped). Missing data never reaches here (len == 0 rejects first).
func isJSONObject(raw json.RawMessage) bool {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return b == '{'
		}
	}
	return false
}
