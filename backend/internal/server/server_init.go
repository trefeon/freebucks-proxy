// Package server exposes the OpenAI-compatible HTTP surface of the
// freebucks-proxy bridge: POST /v1/chat/completions (stream + non-stream),
// GET /v1/models, and GET /healthz. Stdlib only.
//
// Responsibilities (PRD §6 error matrix):
//   - optional client auth (Bearer / x-api-key exact match, constant-time)
//   - request sanitization via backend/internal/convert before the upstream call
//   - retry-once recovery for session-invalid / run-invalid chat errors
//   - 30-min token cooldown on upstream auth rejection
//   - error mapping to the OpenAI error shape, 503 + Retry-After for the
//     waiting room, 502 when every token is exhausted
//   - SSE relay (sanitized chunks + [DONE]) and non-streaming accumulation
//   - client-disconnect propagation to the upstream (request context)
package server

import (
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/convert"
	"freebucks-proxy/backend/internal/dashboard"
	"freebucks-proxy/backend/internal/egress"
	"freebucks-proxy/backend/internal/logring"
	"freebucks-proxy/backend/internal/pool"
	"freebucks-proxy/backend/internal/ratelimit"
	"freebucks-proxy/backend/internal/reasoningcache"
	"freebucks-proxy/backend/internal/registry"
	"freebucks-proxy/backend/internal/store"
	"freebucks-proxy/backend/internal/tokenestimate"
	"freebucks-proxy/backend/internal/updatecheck"
	"freebucks-proxy/backend/internal/upstream"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// maxRequestBody caps the inbound chat-completions body (32MB).
	maxRequestBody = 32 << 20
	// maxStreamLine caps one upstream SSE line the scanner will buffer.
	maxStreamLine = 16 << 20
)

// Server is the HTTP handler holder: routes are built by Handler(). cfg is an
// atomic pointer because /admin/reload swaps it while requests are in flight;
// every read site must Load() it once per request and use the local.
type Server struct {
	cfg     atomic.Pointer[config.Config]
	pool    *pool.Pool
	reg     *registry.Registry
	logger  *slog.Logger
	started time.Time

	// logs is the optional dashboard log viewer ring (nil = disabled); its
	// Counts feed freebucks_proxy_log_events_total on /metrics.
	logs *logring.Handler

	// dash is the embedded admin UI (Svelte SPA + vendored assets).
	dash *dashboard.Dashboard
	// hist is the dashboard history store (nil = live-only views). Threaded
	// into the dashboard at construction; closed via Close on shutdown.
	hist *store.Store
	// adminAuth guards the dashboard: a stateless HMAC-signed session cookie
	// issued against ADMIN_TOKEN, plus a per-IP login rate limiter.
	adminAuth *adminAuth
	// configPath is the -config JSON path ("" when none); reloads re-apply it
	// so JSON overrides survive dashboard saves and /admin/reload.
	configPath string

	// version is the running release tag (""/dev for dev builds); the
	// dashboard badge compares it against the latest GitHub release (#50b).
	// updates is the cached latest-release checker (nil = no badge).
	version string
	updates *updatecheck.Checker

	// authClient drives the headless OAuth login wizard (issue #62): a
	// token-less upstream client whose transport/stealth wiring matches the
	// pooled clients. nil disables the wizard endpoints with 503.
	authClient *upstream.Client
	// tokenEstimator counts tokens locally for /v1/messages/count_tokens
	// (nil only if the embedded codec failed to initialize at startup).
	tokenEstimator *tokenestimate.Estimator
	// loginFlows is the in-flight login-flow registry keyed by flow id
	// (fingerprint): start POSTs /api/auth/cli/code, status polls it until
	// the authToken lands (then AddToken + persist).
	loginFlows map[string]*loginFlow
	// reasoningCache caches reasoning content and signatures for tool calls across turns.
	reasoningCache *reasoningcache.Cache
	// rateLimiter caps client request rates per source IP (issue #137).
	rateLimiter *ratelimit.Limiter
	// rateLimitRejections tracks total client requests rejected by the
	// local rate limiter.
	rateLimitRejections atomic.Int64

	// egressTracker, when set, supplies the detected egress country for the
	// session-locality resolver and the /healthz region field. nil means no
	// region detection: the resolver falls back to the host zone. Set via
	// SetEgressTracker; the tracker itself is started by the CLI serve path,
	// never by a constructor.
	egressTracker atomic.Pointer[egress.Tracker]
	// hostZoneSource, when set, replaces upstream.HostZone in the
	// session-locality rule (a test seam, see SetHostZoneSource).
	hostZoneSource atomic.Pointer[func() string]
	// warnedTimezone is the last invalid SESSION_TIMEZONE value warned about,
	// so a dashboard save (or a reload with the same bad value) warns once.
	warnedTimezone atomic.Pointer[string]

	// admin owns the /admin surface (issue #250): the admin handlers are
	// methods on *adminHandlers, not *Server, so the API surface and the
	// admin surface do not share one mutable god struct.
	admin *adminHandlers

	// gates are the per-Server access-log quiescence gates (issue #252):
	// previously process-global, now owned per instance so two Servers do
	// not share one access gate.
	gates *accessGates
	// rateLimitDedupe gates identical (token, code, window) `request failed`
	// logs (D6): the first + every 50th occurrence fire; the counter always
	// increments so a silent burst stays countable, and the client response
	// is always written. Per-Server like the access gates (issue #252).
	rateLimitDedupe struct {
		mu sync.Mutex
		m  map[string]int64
	}
}

// convertOptions builds the per-request convert options from the live
// config (issue #277/#251): feature knobs resolved once (never per chunk)
// plus the per-Server reasoning lookup threaded through the call chain
// instead of a process-global hook.
func (s *Server) convertOptions() convert.Options {
	opts := convert.Options{
		MaxSchemaNodes:          convert.DefaultMaxSchemaNodes,
		CompressKeepLast:        convert.DefaultCompressKeepLast,
		CompressMaxContentBytes: convert.DefaultCompressMaxContentBytes,
		ReasoningLookup: func(toolID, content, toolCallsJSON string) (string, string, bool) {
			if s.reasoningCache == nil {
				return "", "", false
			}
			return s.reasoningCache.Get(toolID, content, toolCallsJSON)
		},
	}
	if cfg := s.cfg.Load(); cfg != nil {
		opts.CompressPrompt = cfg.CompressPrompt
		opts.CacheControlInjection = cfg.CacheControlInjection
		opts.ReasoningInContent = cfg.ReasoningInContent
	}
	return opts
}

// WithVersion wires the running release tag + update checker for the
// dashboard badge (issue #50b). A nil checker disables the badge.
func WithVersion(version string, updates *updatecheck.Checker) Option {
	return func(s *Server) {
		s.version = version
		s.updates = updates
	}
}

// WithLoginClient wires the token-less upstream client that drives the
// headless OAuth login wizard (issue #62). A nil client disables the
// wizard endpoints with 503.
func WithLoginClient(c *upstream.Client) Option {
	return func(s *Server) {
		s.authClient = c
	}
}

// WithHistory threads the dashboard history store into the embedded admin
// UI (ADR-0016). A nil store keeps every history view on live data.
func WithHistory(st *store.Store) Option {
	return func(s *Server) {
		s.hist = st
	}
}

// SetEgressTracker wires the background egress tracker that feeds the
// session-locality resolver and /healthz's region field. Callers start the
// tracker themselves (Tracker.Start) from the process boot path — never from
// here — so tests that build a Server stay hermetic (no probe traffic). A nil
// tracker clears region detection; the resolver then declares the host zone.
func (s *Server) SetEgressTracker(t *egress.Tracker) {
	if t == nil {
		s.egressTracker.Store(nil)
		return
	}
	s.egressTracker.Store(t)
	if s.pool != nil {
		s.pool.SetLocalityResolver(s.sessionTimezone)
	}
}

// applyConfig swaps in a reloaded configuration and reports a bad
// SESSION_TIMEZONE once: the loader accepts any text and the resolver silently
// falls back to auto, so the warn log is the only operator-visible signal.
func (s *Server) applyConfig(cfg *config.Config) {
	if cfg == nil {
		return
	}
	s.cfg.Store(cfg)
	s.warnInvalidSessionTimezone(cfg.SessionTimezone)
}

// warnInvalidSessionTimezone logs once per distinct invalid zone value. An
// unset or resolvable value is silent.
func (s *Server) warnInvalidSessionTimezone(zone string) {
	zone = strings.TrimSpace(zone)
	if zone == "" || egress.ValidZone(zone) {
		return
	}
	if prev := s.warnedTimezone.Load(); prev != nil && *prev == zone {
		return
	}
	s.warnedTimezone.Store(&zone)
	s.logger.Warn("SESSION_TIMEZONE is not a valid IANA zone; falling back to auto", "value", zone)
}

// sessionLocality resolves the zone the gateway declares on session reads,
// the rule that picked it (override|host|region|utc), and the detected egress
// region ("" when unknown), all from the LIVE config plus the tracker's
// cached country. It never probes: the tracker serves its last known region.
func (s *Server) sessionLocality() (zone, source, region string) {
	override := ""
	if cfg := s.cfg.Load(); cfg != nil {
		override = cfg.SessionTimezone
	}
	if t := s.egressTracker.Load(); t != nil {
		region = t.Country()
	}
	// upstream.HostZone, not time.Local.String(): the Go "Local" placeholder
	// must reach the rule as unset, never as the literal zone "Local".
	host := upstream.HostZone()
	if fn := s.hostZoneSource.Load(); fn != nil {
		host = (*fn)()
	}
	zone, source = egress.SessionTimezone(override, host, region)
	return zone, source, region
}

// SetHostZoneSource installs the source of the host's configured IANA zone for
// the session-locality rule ("" when the host carries none, as on a container
// whose clock reports the bare "Local" placeholder). nil restores
// upstream.HostZone, which reads time.Local. This is a test seam of the same
// kind as SetTransport: a test needs a deterministically boring or real host
// zone, and mutating the process-global time.Local races with the httptest
// server's own timestamp formatting under -race.
func (s *Server) SetHostZoneSource(fn func() string) {
	if fn == nil {
		s.hostZoneSource.Store(nil)
		return
	}
	s.hostZoneSource.Store(&fn)
}

// sessionTimezone is the resolver installed on every upstream client: the
// zone to declare as x-fb-timezone. Never blocks (see sessionLocality).
func (s *Server) sessionTimezone() string {
	zone, _, _ := s.sessionLocality()
	return zone
}

// Option configures optional server features (release-version badge).
type Option func(*Server)

// New builds the server over the configured pool and registry. A nil logger
// falls back to slog.Default(). The started timestamp pins /v1/models
// "created" and /healthz uptime. logs is the optional dashboard log viewer
// ring (nil disables the /admin/logs page data). configPath is the -config
// JSON path the process was started with ("" = none), used by reloads so a
// dashboard save or /admin/reload re-applies the JSON overrides. opts
// configure optional features (release-version badge, login wizard client).
func New(cfg *config.Config, p *pool.Pool, reg *registry.Registry, logger *slog.Logger, logs *logring.Handler, configPath string, opts ...Option) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{pool: p, reg: reg, logger: logger, started: time.Now(), configPath: configPath, loginFlows: make(map[string]*loginFlow), logs: logs, gates: newAccessGates()}
	s.applyConfig(cfg)
	s.rateLimiter = ratelimit.New(cfg.RateLimitPerIP, cfg.RateLimitBurst, 10000)
	// The token estimator shares one o200k_base codec process-wide, so
	// count_tokens requests never rebuild the vocabulary.
	est, err := tokenestimate.New()
	if err != nil {
		logger.Warn("token estimator unavailable; /v1/messages/count_tokens will fail", "err", err)
	}
	s.tokenEstimator = est
	for _, opt := range opts {
		opt(s)
	}
	if cfg.DashboardEnabled {
		dashOpts := []dashboard.Option{}
		if s.version != "" {
			dashOpts = append(dashOpts, dashboard.WithVersion(s.version, s.updates))
		}
		if s.hist != nil {
			dashOpts = append(dashOpts, dashboard.WithHistory(s.hist))
		}
		s.dash = dashboard.New(func() *config.Config { return s.cfg.Load() }, p, reg, logger, logs, dashOpts...)
	}
	s.adminAuth = newAdminAuth()
	s.admin = &adminHandlers{
		dash:           s.dash,
		logfunc:        func() *slog.Logger { return s.logger },
		pool:           p,
		reg:            reg,
		cfgLoad:        s.cfg.Load,
		cfgStore:       s.applyConfig,
		configPath:     configPath,
		settings:       s.hist,
		adminAuth:      s.adminAuth,
		loginFlows:     s.loginFlows,
		authClientFunc: func() *upstream.Client { return s.authClient },
		rateLimiter:    s.rateLimiter,
		handleChat:     s.handleChat,
	}
	s.reasoningCache = reasoningcache.New(10000, 2*time.Hour)
	return s
}

// Close flushes and releases server-owned resources: the dashboard history
// consumer and store. Safe to call on a server built without WithHistory.
func (s *Server) Close() error {
	if s.dash != nil {
		return s.dash.Close()
	}
	return nil
}
