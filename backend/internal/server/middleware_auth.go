package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"freebucks-proxy/backend/internal/config"
	"net/http"
	"strings"
)

// cfgSnapshotKey carries the per-request *config.Config snapshot through
// the request context. requireAuth (the outermost /v1 auth wrapper) loads
// the config ONCE per request and stamps it here; chatCore and authorized
// then decide pooled-vs-bridge routing from that same snapshot, so a
// config swap (e.g. /admin/reload) landing between the middleware's
// pass-through and the handler's routing cannot split one request's
// decision across two different config views.
type cfgSnapshotKey struct{}

// clientKeyHashKey carries the per-request client API-key identity through
// the request context, alongside the config snapshot. The value is
// hex(sha256(rawKey))[:16] — never the raw key — or "" for bridge/no-key
// requests. chatCore finalizes it after the pooled-vs-bridge decision;
// newUsageRecord and recordRequestOutcome read it back for usage tracking.
type clientKeyHashKey struct{}

func withCfgSnapshot(ctx context.Context, cfg *config.Config) context.Context {
	return context.WithValue(ctx, cfgSnapshotKey{}, cfg)
}

// cfgSnapshotFrom returns the config snapshot stamped by requireAuth, or
// nil when the request never passed through it (direct handler calls in
// tests) — callers fall back to their own load in that case.
func cfgSnapshotFrom(ctx context.Context) *config.Config {
	cfg, _ := ctx.Value(cfgSnapshotKey{}).(*config.Config)
	return cfg
}

func withClientKeyHash(ctx context.Context, hash string) context.Context {
	return context.WithValue(ctx, clientKeyHashKey{}, hash)
}

// clientKeyHashFrom returns the key identity stamped on the context, or
// ("", false) when no stamp is present (direct handler calls in tests) —
// callers fall back to deriving it from the request headers in that case.
// A stamped "" (bridge/no-key) reports ("", true).
func clientKeyHashFrom(ctx context.Context) (string, bool) {
	hash, ok := ctx.Value(clientKeyHashKey{}).(string)
	return hash, ok
}

// hashClientKey maps a raw client API key to its usage-tracking identity:
// hex(sha256(raw))[:16]. Empty input maps to "" (bridge/no-key requests
// carry no pooled identity). The raw key never leaves this function.
func hashClientKey(raw string) string {
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])[:16]
}

// requireAuth wraps a handler with client-auth enforcement. When no API keys
// are configured the handler passes through untouched; /healthz is always
// exempt (the caller wires it without requireAuth). Bridge mode (no
// AUTH_TOKENS) also passes through: the Authorization header IS the upstream
// token there, and API_KEYS is meaningless.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfg.Load()
		// Pin the snapshot this decision used: chatCore and authorized
		// below must route this same request from the same config view.
		ctx := withCfgSnapshot(r.Context(), cfg)
		// Stamp the caller's key identity best-effort: a credential
		// matching API_KEYS hashes to its tracking id; bridge tokens and
		// missing credentials stamp "". chatCore re-stamps authoritatively
		// after the pooled-vs-bridge decision.
		if ok, hash := s.authorizedWithIdentity(cfg, r); ok {
			ctx = withClientKeyHash(ctx, hash)
		} else {
			ctx = withClientKeyHash(ctx, "")
		}
		r = r.WithContext(ctx)
		// Hybrid mode (AUTH_TOKENS + BRIDGE_ENABLED) passes through too:
		// the per-request decision — pooled vs bridge — happens in
		// chatCore, where a credential matching API_KEYS uses the pool and
		// any other credential is relayed as a bridge token.
		if len(cfg.APIKeys) == 0 || cfg.BridgeMode() || cfg.HybridBridgeMode() {
			next(w, r)
			return
		}
		if !s.authorized(cfg, r) {
			s.writeJSONError(w, http.StatusUnauthorized,
				"Invalid API key", "invalid_request_error", "invalid_api_key", 0)
			return
		}
		next(w, r)
	}
}

// extractBearerToken extracts the token from an Authorization header if it has
// a case-insensitive "Bearer " prefix (per RFC 7235 / RFC 6750). Returns the
// trimmed token and true if the prefix matches, or ("", false) otherwise.
func extractBearerToken(authHeader string) (string, bool) {
	authHeader = strings.TrimSpace(authHeader)
	if len(authHeader) >= 7 && strings.EqualFold(authHeader[:7], "bearer ") {
		return strings.TrimSpace(authHeader[7:]), true
	}
	return "", false
}

// authorized reports whether the request carries a configured API key,
// either as "Authorization: Bearer <key>", "x-api-key: <key>", or
// "anthropic-api-key: <key>". Comparison is constant-time against every
// configured key. cfg is the caller's config snapshot (see cfgSnapshotKey):
// the check must use the same view that made the surrounding routing
// decision, never a fresh load.
func (s *Server) authorized(cfg *config.Config, r *http.Request) bool {
	ok, _ := s.authorizedWithIdentity(cfg, r)
	return ok
}

// authorizedWithIdentity is authorized plus the caller's key identity: on a
// match it returns (true, hashClientKey(provided)) — the hex(sha256)[:16]
// tracking id, never the raw key. No match (or no credential) returns
// (false, ""). Behavior matches authorized exactly; authorized delegates
// here so the two cannot diverge.
func (s *Server) authorizedWithIdentity(cfg *config.Config, r *http.Request) (bool, string) {
	provided := ""
	if tok, ok := extractBearerToken(r.Header.Get("Authorization")); ok {
		provided = tok
	} else if h := strings.TrimSpace(r.Header.Get("x-api-key")); h != "" {
		provided = h
	} else if h := strings.TrimSpace(r.Header.Get("anthropic-api-key")); h != "" {
		provided = h
	}
	if provided == "" {
		return false, ""
	}
	for _, key := range cfg.APIKeys {
		if subtle.ConstantTimeCompare([]byte(provided), []byte(key)) == 1 {
			return true, hashClientKey(provided)
		}
	}
	return false, ""
}

// requireAdminToken guards POST /admin/reload when ADMIN_TOKEN is set: the
// request must present it as "Authorization: Bearer <token>" (constant-time
// compare). When ADMIN_TOKEN is unset the handler passes through untouched —
// the loopback gate (adminSensitive, wired between requireAdminToken and
// requireAuth) and the legacy API_KEYS gate then apply, and main.go logs a
// startup warning for the open (default) case.
func (s *Server) requireAdminToken(next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfg.Load()
		if cfg.AdminToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		provided := ""
		if tok, ok := extractBearerToken(r.Header.Get("Authorization")); ok {
			provided = tok
		}
		if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(cfg.AdminToken)) != 1 {
			s.writeJSONError(w, http.StatusUnauthorized,
				"Invalid password", "invalid_request_error", "invalid_admin_token", 0)
			return
		}
		next.ServeHTTP(w, r)
	}
}

// clientToken returns the request's bearer token (Authorization: Bearer,
// x-api-key, or anthropic-api-key), trimmed. Empty when the request carries
// none. In bridge mode this token IS the client's FreeBuff token relayed
// upstream.
func clientToken(r *http.Request) string {
	provided := ""
	if tok, ok := extractBearerToken(r.Header.Get("Authorization")); ok {
		provided = tok
	} else if h := r.Header.Get("x-api-key"); h != "" {
		provided = h
	} else if h := r.Header.Get("anthropic-api-key"); h != "" {
		provided = h
	}
	return strings.TrimSpace(provided)
}

// bearerToken returns only the Authorization: Bearer token (the
// Authorization header value without the "Bearer " prefix). Returns "" if
// no Bearer token is present.
func bearerToken(r *http.Request) string {
	if tok, ok := extractBearerToken(r.Header.Get("Authorization")); ok {
		return tok
	}
	return ""
}
