// Package cli implements the freebucks-proxy serve mode (the default when no
// subcommand flag is set): config loading, log construction, registry,
// pool/session wiring, the HTTP server, and graceful drain on shutdown.
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"
	_ "time/tzdata"

	// Embed the IANA tzdata so NextPacificMidnight keeps exact DST math on
	// minimal images (alpine:3.20 has no /usr/share/zoneinfo) and Windows
	// hosts without the timezone registry entries. Without this, Pacific
	// resets fall back to a month-based approximation.
	"freebucks-proxy/backend/internal/cli/port"
	"freebucks-proxy/backend/internal/clicreds"
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/egress"
	"freebucks-proxy/backend/internal/logring"
	"freebucks-proxy/backend/internal/notify"
	"freebucks-proxy/backend/internal/pool"
	"freebucks-proxy/backend/internal/registry"
	"freebucks-proxy/backend/internal/server"
	"freebucks-proxy/backend/internal/session"
	"freebucks-proxy/backend/internal/telemetry"
	"freebucks-proxy/backend/internal/updatecheck"
	"freebucks-proxy/backend/internal/upstream"

	history "freebucks-proxy/backend/internal/store"
)

// Serve runs the default serve mode: load config, construct the logger,
// build the registry and pool bound to one upstream client + session manager
// per token, start the background registry refresh and pool, serve HTTP, and
// drain gracefully on a shutdown signal. It returns the process exit code
// (0 normal, 1 server failure); the caller maps it to os.Exit.
func Serve(configPath string, verbose bool, version string) int {
	// DB settings overlay (ADR-0019): the store opens BEFORE the first Load
	// so UI-persisted knobs apply from boot (env > db > file > default).
	// DB_PATH resolves from the process environment alone, so no config is
	// needed to open it; a failure only warns (live-only), and the same
	// handle feeds the history wiring below (never opened twice).
	// OpenWithStatus also detects the file's data generation for the boot
	// smart-migration report below: fresh init, legacy pre-goose stamp, or
	// goose-converged (see the env-to-DB block after the logger exists).
	var histStore *history.Store
	var bootOverlay map[string]string
	bootMigrate := history.MigrateStatus{Applied: []int{}}
	{
		dbPath := history.DBPathFromEnv()
		if st, ms, err := history.OpenWithStatus(dbPath); err != nil {
			fmt.Fprintln(os.Stderr, "freebucks-proxy: settings store unavailable; running live-only:", err)
		} else {
			histStore = st
			bootMigrate = ms
			if rows, err := st.ListSettings(); err != nil {
				fmt.Fprintln(os.Stderr, "freebucks-proxy: settings overlay unreadable; running on file/env:", err)
			} else if ov := config.OverlayFromRows(rows); len(ov) > 0 {
				bootOverlay = ov
				fmt.Fprintln(os.Stderr, "freebucks-proxy: applying", len(ov), "DB setting override(s)")
			}
		}
	}

	cfg, err := config.LoadOpts(configPath, config.LoadOptions{DiscoverCLIToken: clicreds.DiscoverToken, Overlay: bootOverlay})
	if err != nil {
		fmt.Fprintln(os.Stderr, "freebucks-proxy: invalid config:", err)
		holdForExitIfConsole()
		return 1
	}

	// Effective log level: LOG_LEVEL config wins, else -v → debug, else debug
	// (release builds default to debug so error reports carry context).
	level := resolveLogLevel(cfg.LogLevel, verbose, version)
	logger := telemetry.New(level, cfg.LogFile, cfg.LogFormat)
	// The dashboard log viewer reads from an in-memory ring that mirrors
	// every record the process logger emits (no log file or docker needed).
	// Ring capacity is hardcoded to 500 (LOG_RING_SIZE is excised).
	logringHandler := logring.NewHandler(logger.Handler(), 500)
	logger = slog.New(logringHandler)
	// The pool/upstream/session/runs log through slog.Default(); route it
	// through our logger so the configured level and log file cover them too.
	slog.SetDefault(logger)

	// The proxy reads the resolved .env (issue #39): ./.env in the working
	// directory wins; otherwise the platform config dir is tried
	// ($XDG_CONFIG_HOME / %APPDATA% / ~/Library/Application Support, under
	// freebucks-proxy/). Log the absolute path used, and warn when a .env
	// sitting next to the executable is silently ignored — that is the
	// usual reason config "seems to vanish" under a non-interactive
	// launcher (Task Scheduler, shortcuts, services).
	envFile := cfg.EnvFile
	if envFile != "" {
		if abs, err := filepath.Abs(envFile); err == nil {
			envFile = abs
		}
	}
	logger.Info("config loaded", "env_file", envFile, "config_file", configPath)
	if cfg.EnvFile == "" {
		if cwd, err := os.Getwd(); err == nil {
			exe, exeErr := os.Executable()
			if exeErr == nil {
				if p := ignoredExeAdjacentEnv(cwd, exe); p != "" {
					logger.Warn("found .env next to the executable, but no .env candidate exists in the config search path — that file is NOT applied",
						"cwd", cwd, "exe_dir", filepath.Dir(exe))
				}
			}
		}
	} else if cwd, err := os.Getwd(); err == nil {
		exe, exeErr := os.Executable()
		if exeErr == nil {
			if p := ignoredExeAdjacentEnv(cwd, exe); p != "" {
				logger.Warn("found .env next to the executable, but the config search resolved a different .env — that file is NOT applied",
					"cwd", cwd, "exe_dir", filepath.Dir(exe), "env_file", envFile)
			}
		}
	}
	// One-time persisted-state carry (the production update lesson), BEFORE the
	// env-to-DB import below: the same legacy candidates fold their operator
	// state — settings overlay (config: rows, credentials included),
	// pages_state, sessions_persist, tokens, pool_state — into a fresh live
	// store so a recreate never strands state. Order is load-bearing: the
	// env import upserts every catalog key, so running it first would fill
	// settings and make the carry skip the table, stranding dashboard-only
	// knobs. Carry-first, the legacy overlay (including the
	// config:migrated_env_v1 marker) lands verbatim and the env import
	// no-ops; explicit process env still wins at runtime by precedence.
	// Per-table empty gate (populated tables are skipped, never merged),
	// staged read-only counting for the skip report, secrets as opaque DB
	// values with table names + row counts logged only. Steady-state boots
	// just walk cheap COUNT(*) no-ops. Warn-only, legacy files stay.
	if histStore != nil {
		stateCarriedFrom := ""
		for _, legacyState := range history.LegacyHistoryCandidates(history.DBPathFromEnv(), cfg.SessionStateFile) {
			n, err := history.ImportLegacyPersistedState(histStore, history.DBPathFromEnv(), legacyState)
			if err != nil {
				logger.Warn("legacy state carry skipped", "file", legacyState, "err", err)
				continue
			}
			if n > 0 {
				logger.Info("carried legacy state into dashboard store", "file", legacyState, "rows", n)
				if stateCarriedFrom == "" {
					stateCarriedFrom = legacyState
				}
				continue
			}
			if stateCarriedFrom == "" {
				continue
			}
			counts, cerr := history.CountLegacyPersistedRows(histStore, legacyState)
			if cerr != nil {
				logger.Warn("legacy state carry skipped; later era left in place unreadable (no cross-table merge)", "file", legacyState, "carried_from", stateCarriedFrom, "err", cerr)
				continue
			}
			var skipped int64
			for _, c := range counts {
				skipped += c
			}
			if skipped > 0 {
				logger.Warn("legacy state skipped: store already carries an earlier era; later file left in place (no cross-table merge)", "file", legacyState, "carried_from", stateCarriedFrom, "rows", skipped, "settings", counts["settings"], "pages_state", counts["pages_state"], "sessions_persist", counts["sessions_persist"], "tokens", counts["tokens"], "pool_state", counts["pool_state"])
			}
		}
	}
	// Boot smart migration, in order: the store open above already ran the
	// pending goose chain for the detected data generation ((a) no DB file:
	// fresh init, (b) legacy pre-goose stamp: baseline + remainder, (c)
	// goose-converged: nothing pending); the env-to-DB import below runs
	// when the marker row is missing and no-ops once it is present (d), so
	// a marked, latest-version boot performs zero writes. Detection is
	// version-aware: one INFO line per applied goose version plus a
	// from->to summary, and the same facts stay readable for the dashboard
	// via the store's in-memory MigrateStatus (GET /admin/api/settings
	// "migrate"). Key names and counts log here, never values.
	envImported := 0
	migrateFailed := false
	if histStore != nil {
		if n, err := migrateEnvToDB(histStore, cfg); err != nil {
			migrateFailed = true
			logger.Warn("env-to-DB migration failed; running on file/env/overlay", "err", err)
		} else {
			envImported = n
			if n > 0 {
				logger.Info("migrated env config to DB overlay", "keys", n, "marker", config.MigrationMarkerRow)
			}
		}
		for _, v := range bootMigrate.Applied {
			logger.Info("store migration applied", "version", v, "from_version", bootMigrate.FromVersion, "to_version", bootMigrate.ToVersion)
		}
		// A failed env import retries on the next boot (the marker is only
		// set after a full import), so it poisons the no-op verdict.
		logger.Info("smart migrate complete",
			"from_version", bootMigrate.FromVersion,
			"to_version", bootMigrate.ToVersion,
			"store_applied", bootMigrate.Applied,
			"fresh", bootMigrate.Fresh,
			"env_imported", envImported,
			"noop", !migrateFailed && bootMigrate.Noop && envImported == 0)
	}

	// Load the hardcoded fallback immediately so the registry is usable
	// offline; the first background refresh replaces it on success.
	reg := registry.New(&cfg, &http.Client{Timeout: 30 * time.Second})
	reg.LoadFallback()

	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer stop()

	go refreshLoop(ctx, logger, reg, cfg.RegistryRefresh)

	// One upstream client and session manager per token, bound into the pool
	// together with a per-token run manager. When SESSION_PERSIST is enabled
	// one shared store backs every session manager (fixed, runtime-added, and
	// bridge entries), so a restart resumes unexpired sessions.
	var store *session.Store
	if cfg.SessionPersist {
		// Log the absolute state-file path: a relative SESSION_STATE_FILE is
		// resolved against the working directory. The JSON file is
		// import-only now (sessions_persist is the sole truth): on first
		// use the store folds it into the dashboard DB and archives it to
		// .bak. The file is never written.
		stateFile := cfg.SessionStateFile
		if abs, err := filepath.Abs(stateFile); err == nil {
			stateFile = abs
		}
		if histStore != nil {
			// *history.Store satisfies session.SessionBackend
			// structurally (opaque blobs, SHA-256 keys); the session
			// package never imports the store package (archtest leaf).
			store = session.NewStoreWithBackend(stateFile, histStore)
			logger.Info("session state persistence enabled (dashboard store)", "file", stateFile)
		} else {
			// DB unavailable: memory-only for this run. The legacy file
			// still seeds memory on first use, never written.
			store = session.NewStore(stateFile)
			logger.Warn("dashboard store unavailable; session persistence is memory-only for this run", "file", stateFile)
		}

		// Same cwd-vs-exe trap as .env: on Windows launchers (Task
		// Scheduler, shortcuts, services) the working directory is often not
		// the executable's directory, so warn when a state file next to the
		// executable is silently ignored for the same reason.
		if !filepath.IsAbs(cfg.SessionStateFile) {
			if cwd, err := os.Getwd(); err == nil {
				exe, exeErr := os.Executable()
				if exeErr == nil {
					exeDir := filepath.Dir(exe)
					if filepath.Clean(cwd) != exeDir {
						if _, statErr := os.Stat(filepath.Join(exeDir, cfg.SessionStateFile)); statErr == nil {
							logger.Warn("found session state file next to the executable, but SESSION_STATE_FILE is read from the working directory — that file is NOT used",
								"cwd", cwd, "exe_dir", exeDir, "state_file", stateFile)
						}
					}
				}
			}
		}
	}
	// Dashboard persistence (ADR-0016, overlay ADR-0019): the store opened
	// before Load (boot overlay) — the legacy imports and history carry run
	// against the same handle below. DB_PATH wins, default
	// ./data/freebuff.db (the compose db_data volume mirrors it at
	// /app/data/freebuff.db). A nil store is the live-only degrade path; the
	// session managers above run memory-only (legacy file seeds, never written).
	if histStore != nil {
		st := histStore
		{
			dbPath := history.DBPathFromEnv()
			logger.Info("dashboard history enabled", "file", dbPath)
			// One-time migration: fold every legacy JSON session file into
			// sessions_persist, then archive each to .bak (never delete).
			// Gated on SESSION_PERSIST; a failure only warns — the session
			// managers above already run DB-backed (or memory-only without
			// a DB) and the store-level import is idempotent. SaveSession upserts by token
			// hash, so importing from several split-brain locations is
			// last-wins; each overwrite of a previously imported hash with
			// different content warns (identical re-imports stay silent).
			if cfg.SessionPersist {
				onCollision := func(file string) func(string) {
					return func(hash string) {
						logger.Warn("legacy session import overwrote a previously imported session with different content; later file wins", "file", file, "token_hash", hash)
					}
				}
				for _, legacy := range history.LegacySessionCandidates(cfg.SessionStateFile) {
					if n, err := history.ImportLegacySessionFileWithCollisions(st, legacy, onCollision(legacy)); err != nil {
						logger.Warn("legacy session import skipped; legacy file left in place (DB stays authoritative)", "file", legacy, "err", err)
					} else if n > 0 {
						logger.Info("imported legacy session state into dashboard store", "file", legacy, "sessions", n)
					}
				}
				// .bak re-consult: each import above archives its source to
				// .bak, so when sessions_persist still holds zero rows AND
				// the live JSON path is missing, the archive is the only
				// copy left (fresh DB path over a previous install) —
				// import it WITHOUT re-archiving (the path already is the
				// archive). Warn-only like every other carry step.
				if empty, err := st.SessionsEmpty(); err != nil {
					logger.Warn("legacy session backup re-consult skipped", "err", err)
				} else if empty {
					if _, statErr := os.Stat(cfg.SessionStateFile); errors.Is(statErr, os.ErrNotExist) {
						bak := cfg.SessionStateFile + ".bak"
						if fi, bakErr := os.Stat(bak); bakErr == nil && fi.Mode().IsRegular() {
							if n, err := history.ImportLegacySessionBackup(st, bak, onCollision(bak)); err != nil {
								logger.Warn("legacy session backup import skipped", "file", bak, "err", err)
							} else if n > 0 {
								logger.Info("imported legacy session backup into dashboard store", "file", bak, "sessions", n)
							}
						}
					}
				}
			}
			// One-time display-history carry: every legacy dashboard DB still
			// on disk folds into the new DB while the new history tables are
			// empty (old bind mounts, pre-unified files). An empty candidate
			// (0, nil) does NOT stop the scan — a later file may hold the
			// rows. ImportLegacyHistoryDB fills from ONE file only (never
			// merges — INTEGER rowid PKs would collide across files), so
			// the scan keeps walking after a carry: a later file holding
			// rows is a skipped era, inspected (staged copy, COUNT(*) per
			// table, never imported) and reported at WARN with its counts.
			// Steady-state boots just walk cheap COUNT(*) no-ops. Warn-only,
			// legacy files stay.
			carriedFrom := ""
			for _, legacyHist := range history.LegacyHistoryCandidates(history.DBPathFromEnv(), cfg.SessionStateFile) {
				n, err := history.ImportLegacyHistoryDB(st, history.DBPathFromEnv(), legacyHist)
				if err != nil {
					logger.Warn("legacy history carry skipped", "file", legacyHist, "err", err)
					continue
				}
				if n > 0 {
					logger.Info("carried legacy history into dashboard store", "file", legacyHist, "rows", n)
					if carriedFrom == "" {
						carriedFrom = legacyHist
					}
					continue
				}
				if carriedFrom == "" {
					continue
				}
				counts, cerr := history.CountLegacyHistoryRows(st, legacyHist)
				if cerr != nil {
					logger.Warn("legacy history carry skipped; later era left in place unreadable (no cross-file merge: rowid collisions)", "file", legacyHist, "carried_from", carriedFrom, "err", cerr)
					continue
				}
				var skipped int64
				for _, c := range counts {
					skipped += c
				}
				if skipped > 0 {
					logger.Warn("legacy history skipped: store already carries an earlier era; later file left in place (no cross-file merge: rowid collisions)", "file", legacyHist, "carried_from", carriedFrom, "rows", skipped, "log_entries", counts["log_entries"], "quota_snapshots", counts["quota_snapshots"], "maturity_events", counts["maturity_events"], "request_records", counts["request_records"])
				}
			}
		}
	}
	clients := make([]*upstream.Client, 0, len(cfg.AuthTokens))
	sessions := make([]*session.Manager, 0, len(cfg.AuthTokens))
	for i, token := range cfg.AuthTokens {
		client, err := upstream.NewWithIndex(token, i, &cfg)
		if err != nil {
			logger.Error("failed to build upstream client", "err", err)
			holdForExitIfConsole()
			return 1
		}
		clients = append(clients, client)
		sessions = append(sessions, session.NewManagerWithStore(client, store))
	}
	if cfg.DiscoveredSource != "" {
		logger.Info("auto-discovered FreeBuff token from CLI login", "email", cfg.DiscoveredEmail, "file", cfg.DiscoveredSource)
	}
	p, err := pool.New(&cfg, clients, sessions, reg)
	if err != nil {
		logger.Error("failed to build pool", "err", err)
		holdForExitIfConsole()
		return 1
	}
	p.SetSessionStore(store)
	// Dashboard history (ADR-0016): pool maturity events persist through a
	// nil-safe adapter; without a store the pool stays persistence-free.
	p.SetHistorySink(&poolHistorySink{st: histStore})
	// Pool runtime persist (pool_state): the live per-token quota cache
	// survives restarts through the dashboard store (opaque blobs,
	// SHA-256 keys — raw tokens never reach the DB). The flush rides the
	// maintain tick + Shutdown and Start restores; a nil store stays
	// live-only. The concrete nil guard stays here: a nil *Store would
	// arrive as a non-nil interface.
	if histStore != nil {
		p.SetPoolPersist(histStore)
	}
	// Issue #48: best-effort webhook alerts (WEBHOOK_URL) for pool
	// exhaustion / token bans — fire-and-forget, throttled, never blocking.
	if cfg.WebhookURL != "" {
		p.SetNotifier(notify.New(cfg.WebhookURL, nil))
		logger.Info("webhook alerts enabled", "url", notify.RedactURL(cfg.WebhookURL))
	}

	// Issue #97: ADOPT_CLI_SESSION — seed every session manager with the
	// CLI-session adoption mode (owner file re-read per refresh; never
	// create a competing session while the CLI is alive).
	if cfg.AdoptCLISession {
		ownerFile, err := cliOwnerFilePath()
		if err != nil {
			logger.Error("ADOPT_CLI_SESSION: cannot resolve freebuff-instance-owner.json", "err", err)
			holdForExitIfConsole()
			return 1
		}
		for _, sess := range sessions {
			sess.SetCLIAdoption(session.CLIAdoption{Enabled: true, OwnerFile: ownerFile})
		}
		logger.Info("ADOPT_CLI_SESSION: adopting the official CLI session (single-session friendly)", "owner_file", ownerFile)
	}

	// Prewarm + the 60s maintain loop run until ctx is canceled (shutdown).
	p.Start(ctx)

	// Egress probing is NOT a risk-engine feed (#123): nothing in the request
	// path consults the probe's IP/country. It IS wired into startup now, with
	// exactly one consumer — the session-locality rule: the gateway declares
	// an IANA zone on every session call (x-fb-timezone) and the upstream
	// server derives the account's daily reset zone from it. The detected
	// egress region supplies that zone ONLY when the host zone carries no
	// locality (UTC/Local — egress.BoringZone); an explicit SESSION_TIMEZONE,
	// or any real host zone, always wins. Started here, never in a server
	// constructor, so tests stay hermetic; it stops with the same shutdown
	// context as the pool.
	//
	// #123 had two reasons and only the first is answered by that consumer:
	// the probe now has one, but the gateway does gain a recurring outbound
	// request (one cdn-cgi/trace GET every egress.DefaultTTL, fixed cadence,
	// SESSION_TIMEZONE does not disable it) that the official CLI never makes.
	// It carries no credentials and never touches upstream visibility — only
	// Cloudflare learns the egress IP — and docs/decisions/locality-timezone.md
	// records the reversal.
	egressTracker := egress.NewTracker(egress.NewCache(), egress.Path{
		Key:    "direct",
		Dialer: egress.DirectDialer(egress.ProbeTimeout),
	}, egress.DefaultTTL)

	// Issue #62: the dashboard login wizard drives the same headless OAuth
	// flow as the CLI against the proxy's own transport/stealth wiring; the
	// token it yields is added to the pool + .env (nil disables the wizard).
	loginClient, err := upstream.NewForAuth(&cfg)
	if err != nil {
		slog.Warn("dashboard login client unavailable (login wizard disabled)", "err", err)
	}
	serverOpts := []server.Option{server.WithLoginClient(loginClient)}
	// Dashboard history (ADR-0016): nil-safe, live-only views when unset.
	serverOpts = append(serverOpts, server.WithHistory(histStore))
	// Issue #50b: release update indicator — the dashboard badge compares
	// the running version against the latest GitHub release (6h cache).
	serverOpts = append(serverOpts, server.WithVersion(version, updatecheck.New(updatecheck.DefaultRepo, nil)))

	srv := server.New(&cfg, p, reg, logger, logringHandler, configPath, serverOpts...)
	// Session locality: install the resolver on the pooled clients and wire
	// the tracker, then start the probe loop (immediate first probe, then
	// every interval) on the process context. /healthz reports the live
	// zone/source/region (session_timezone, session_timezone_source,
	// egress_region).
	srv.SetEgressTracker(egressTracker)
	go egressTracker.Start(ctx)
	logger.Info("session locality enabled: session calls declare x-fb-timezone (SESSION_TIMEZONE > real host zone > detected egress region > UTC; see /healthz session_timezone)",
		"probe_interval", egress.DefaultTTL.String())
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       cfg.HTTPReadTimeout,
		// IdleTimeout closes keep-alive connections that have been idle for
		// two minutes, bounding goroutines parked on dead clients.
		IdleTimeout: 120 * time.Second,
		// WriteTimeout is deliberately unset (0): /v1/chat/completions
		// streams SSE responses that can legitimately outlive any fixed
		// write budget.
	}

	// Startup summary -- token values are never logged, only counts.
	logger.Info("freebucks-proxy starting",
		"version", version,
		"listen_addr", cfg.ListenAddr,
		"upstream", cfg.UpstreamBaseURL,
		"auth_tokens", len(cfg.AuthTokens),
		"bridge_mode", len(cfg.AuthTokens) == 0,
		"api_keys", len(cfg.APIKeys),
		"cost_mode", cfg.CostMode,
		"rotation_interval", cfg.RotationInterval.String(),
		"registry_refresh", cfg.RegistryRefresh.String(),
		"registry_agents", len(reg.AgentIDs()),
		"registry_models", reg.ModelCount(),
		"log_level", logLevelDisplay(level),
		"verbose", verbose,
		"dashboard_enabled", cfg.DashboardEnabled,
	)
	if cfg.ActingUserID != "" {
		// #126: the header is only safe with the token's OWN account id (the
		// CLI derives it from /api/v1/me; the server honors it only for the
		// FreeBuff Web service account) — any other value impersonates a
		// foreign user and can flag the account.
		logger.Info("acting user id set — x-freebuff-acting-user-id will be sent on chat calls (only safe with the token's own account id; any other value impersonates another user)", "acting_user_id", cfg.ActingUserID)
	}
	// Warn loudly if the dashboard is running with the factory default password ("123456").
	if cfg.DashboardEnabled && cfg.IsDefaultAdminToken() {
		logger.Warn("ADMIN_TOKEN is using default password ('123456') — change it immediately in dashboard settings or .env to secure this instance")
	}
	if w := adminTokenCleartextWarning(cfg.AdminToken, cfg.ListenAddr); w != "" {
		logger.Warn(w)
	}
	if w := openAPIWarning(cfg.AuthTokens, cfg.APIKeys, cfg.ListenAddr); w != "" {
		logger.Warn(w)
	}
	logger.Info("listening", "addr", cfg.ListenAddr)

	// Human-readable startup banner for interactive terminals. Suppressed
	// when stderr is piped (containers, log files, systemd) -- detected by
	// checking if the output is a character device (terminal).
	if stderrIsCharDevice() {
		mode := fmt.Sprintf("pooled (%d tokens)", len(cfg.AuthTokens))
		switch {
		case cfg.BridgeMode():
			mode = "bridge (clients send their own token)"
		case cfg.HybridBridgeMode():
			mode = fmt.Sprintf("hybrid (%d pooled tokens + bridge relay)", len(cfg.AuthTokens))
		}
		fmt.Fprintf(os.Stderr, "\n"+
			"  freebucks-proxy %s is running!\n"+
			"\n"+
			"  API endpoint:  http://%s/v1\n"+
			"  Health check:  http://%s/healthz\n"+
			"  Models:        http://%s/v1/models\n"+
			"  Mode:          %s\n"+
			"\n"+
			"  Quick test:\n"+
			"    curl http://%s/healthz\n"+
			"\n"+
			"  Press Ctrl+C to stop.\n\n",
			version, cfg.ListenAddr, cfg.ListenAddr, cfg.ListenAddr, mode, cfg.ListenAddr,
		)
	}

	// Serve until the server fails or a shutdown signal arrives.
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.ListenAndServe()
	}()

	// A bind failure (port already in use) is the most common startup
	// error and the one that looks like "cannot open" when the EXE is
	// double-clicked: print a prominent hint naming the offender before
	// draining. Any server failure exits non-zero so scripts/health checks
	// can tell the process did not come up.
	exitCode := 0
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			if port.IsPortInUse(err) {
				printPortInUseHint(cfg.ListenAddr, err)
			} else {
				logger.Error("http server failed", "err", err)
			}
			exitCode = 1
			stop() // cancel ctx: stop the pool jobs, then drain
		}
	case <-ctx.Done():
	}

	// Graceful drain: stop accepting new requests first, then finish
	// runs/sessions. HTTP gets a 10s force deadline; the pool then gets its
	// OWN fresh budget — a slow-draining SSE stream can consume the whole
	// HTTP budget, and the pool drain must not be starved by it.
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http server shutdown incomplete", "err", err)
	}
	poolCtx, poolCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer poolCancel()
	p.Shutdown(poolCtx)
	// Flush the history spill consumer and release the SQLite handle.
	if err := srv.Close(); err != nil {
		logger.Warn("history store close failed", "err", err)
	}
	logger.Info("shutdown complete")
	return exitCode
}
