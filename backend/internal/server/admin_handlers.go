package server

// adminHandlers own the /admin surface (issue #250): every admin handler is
// a method on this struct instead of *Server, so the API/engine surface and
// the admin/dashboard surface no longer share one mutable god struct. The
// deps below are exactly what the admin handlers reach into; Server.New
// wires them once and server.adminHandler dispatches through s.admin.

import (
	"encoding/json"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/dashboard"
	"freebuff-proxy/backend/internal/egress"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/ratelimit"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/store"
	"freebuff-proxy/backend/internal/upstream"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

type adminHandlers struct {
	dash *dashboard.Dashboard
	// logfunc reads the Server's CURRENT logger: tests replace it after
	// New (logging_wave2_test.newLoggingServer), so the admin surface must
	// follow it.
	logfunc    func() *slog.Logger
	pool       *pool.Pool
	reg        *registry.Registry
	cfgLoad    func() *config.Config
	cfgStore   func(*config.Config)
	configPath string

	adminAuth   *adminAuth
	adminSaveMu sync.Mutex
	loginMu     sync.Mutex
	loginFlows  map[string]*loginFlow
	// authClientFunc reads the Server's current login-wizard client: options
	// may install it after construction.
	authClientFunc func() *upstream.Client
	// settings is the DB settings overlay store (ADR-0019): the same handle
	// as Server.hist (one SQLite file). Nil keeps the settings endpoints on
	// file/env/default with mutations 503.
	settings *store.Store
	// spill is the settings WAL drain (unified-store, Lane A): mutations
	// swap mem synchronously and persist the overlay delta behind via
	// enqueueSettingsSpill. Lazily started on first mutation (nil until
	// then); spillMu guards start/flush/close. Nil store means mem-only.
	spillMu    sync.Mutex
	spillState *settingsSpill
	// pages is the page-state mem snapshot (unified store, Lane D): reads
	// serve from mem, PUTs swap mem synchronously and spill to the DB
	// behind. Lazily built by admin_pages.go pageMem; nil until first use.
	// pagesMu also guards rebuilds when a.settings is swapped post-boot
	// (tests attaching a store after construction).
	pagesMu     sync.Mutex
	pages       *pageStateMem
	rateLimiter *ratelimit.Limiter
	// handleChat forwards the playground's synthetic chat request to the
	// normal chat pipeline (admin.go:176).
	handleChat func(w http.ResponseWriter, r *http.Request)
	// egressResult reports the last known direct-egress probe result
	// without touching the network (nil = region detection off). Wired
	// from Server.egressTracker so diag stays hermetic in tests.
	egressResult func() (egress.Result, bool)
}

func (a *adminHandlers) handleAdminRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Validate configuration before restart to prevent exiting on broken settings
	if _, err := a.loadConfig(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(dashboard.RestartResponse{
			Message: "Config validation failed — aborting restart: " + err.Error(),
		})
		return
	}

	a.logfunc().Info("admin restart initiated via dashboard", "remote", remoteHost(r))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(dashboard.RestartResponse{
		Message: "Gateway process restart initiated.",
		OK:      true,
	})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	go func() {
		time.Sleep(200 * time.Millisecond)
		restartProcess()
	}()
}
