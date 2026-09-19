package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Validate checks the resolved configuration. It must be called before use.
// Includes actionable fix suggestions for common misconfigurations (#16).
func (c Config) Validate() error {
	switch {
	case c.ListenAddr == "":
		return errors.New("LISTEN_ADDR cannot be empty")
	case !strings.Contains(c.ListenAddr, ":"):
		return fmt.Errorf("LISTEN_ADDR %q missing port separator ':' (did you mean '127.0.0.1:3457' or ':3457'?)", c.ListenAddr)
	case c.UpstreamBaseURL == "":
		return errors.New("UPSTREAM_BASE_URL cannot be empty")
	case c.RotationInterval <= 0:
		return errors.New("ROTATION_INTERVAL must be greater than zero")
	case c.RequestTimeout <= 0:
		return errors.New("REQUEST_TIMEOUT must be greater than zero")
	case c.SessionCallTimeout <= 0:
		return errors.New("SESSION_CALL_TIMEOUT must be greater than zero")
	case c.HTTPReadTimeout < 0:
		return errors.New("HTTP_READ_TIMEOUT cannot be negative (0 disables the read timeout)")
	case c.RegistryRefresh <= 0:
		return errors.New("REGISTRY_REFRESH must be greater than zero")
	case c.RequestJitter < 0:
		return errors.New("REQUEST_JITTER cannot be negative")
	case c.TransientRetries < 0:
		return errors.New("TRANSIENT_RETRIES cannot be negative")
	case c.RunFinishQueueSize < 0 || c.RunsDrainQueueCap < 0:
		return errors.New("RUN_FINISH_QUEUE_SIZE/RUNS_DRAIN_QUEUE_CAP cannot be negative (0 = default)")
	case c.SessionPersist && strings.TrimSpace(c.SessionStateFile) == "":
		return errors.New("SESSION_STATE_FILE cannot be empty when SESSION_PERSIST is enabled")
	case c.CostMode != "" && c.CostMode != "free":
		return errors.New(`COST_MODE must be "free" or unset -- any other value (e.g. a typo) routes requests as PAID and fresh free accounts get 402 "Out of credits"`)
	case math.IsNaN(c.RateLimitPerIP) || math.IsInf(c.RateLimitPerIP, 0):
		return errors.New("RATE_LIMIT_PER_IP must be a finite number (requests/second, 0 disables)")
	case c.RateLimitPerIP < 0:
		return errors.New("RATE_LIMIT_PER_IP cannot be negative")
	case c.RateLimitBurst < 0:
		return errors.New("RATE_LIMIT_BURST cannot be negative")
	case c.QueueDepth < 0:
		return errors.New("QUEUE_DEPTH cannot be negative (0 disables slot queueing)")
	case c.SlotsPerAccount < 0:
		return errors.New("SLOTS_PER_ACCOUNT cannot be negative (0 = unlimited)")
	case c.MaxSpillAccounts < 0:
		return errors.New("MAX_SPILL_ACCOUNTS cannot be negative (0 = unbounded)")
	case c.MaturityTargetDays < 1 || c.MaturityTargetDays > 28:
		return fmt.Errorf("MATURITY_TARGET_DAYS must be an integer in 1..28 (got %d)", c.MaturityTargetDays)
	}
	// PIN_MODEL cross-check: every pinned slot must address a configured
	// AUTH_TOKENS position, so a typo surfaces at load instead of silently
	// pinning nothing (an out-of-range pin would never match an Acquire).
	// Skipped when no pool is configured: pin syntax is still parsed at
	// Load, and the range only means something against real slots.
	for idx := range c.PinModel {
		if len(c.AuthTokens) > 0 && (idx < 0 || idx >= len(c.AuthTokens)) {
			return fmt.Errorf("PIN_MODEL slot %d out of range (only %d AUTH_TOKENS slot(s) configured)", idx, len(c.AuthTokens))
		}
	}
	// tolerated as unknown keys and ignored, so there is nothing to validate.

	if c.WebhookURL != "" {
		u, err := url.Parse(c.WebhookURL)
		if err != nil {
			return fmt.Errorf("WEBHOOK_URL %q is not a valid URL: %w", c.WebhookURL, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("WEBHOOK_URL %q must be an http(s) URL", c.WebhookURL)
		}
		if u.Host == "" {
			return fmt.Errorf("WEBHOOK_URL %q has no host", c.WebhookURL)
		}
		if err := validateWebhookHost(c.WebhookURL, u.Hostname()); err != nil {
			return err
		}
	}

	_, portStr, err := net.SplitHostPort(c.ListenAddr)
	if err != nil {
		return fmt.Errorf("LISTEN_ADDR %q is invalid: %w", c.ListenAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("LISTEN_ADDR %q has invalid port %q (must be an integer in 1-65535)", c.ListenAddr, portStr)
	}

	for i, tok := range c.AuthTokens {
		if strings.HasPrefix(strings.ToLower(tok), "bearer ") {
			return fmt.Errorf("AUTH_TOKENS token #%d starts with 'Bearer ' prefix -- remove 'Bearer ' (the proxy adds it upstream automatically)", i+1)
		}
		if tok == "cb_xxx" || tok == "cb_yyy" || tok == "YOUR_TOKEN_HERE" {
			return fmt.Errorf("AUTH_TOKENS token #%d is a placeholder %q -- replace with a real FreeBuff token (run: freebuff)", i+1, tok)
		}
	}

	if c.TLSFingerprint != "" {
		switch strings.ToLower(c.TLSFingerprint) {
		case "chrome120", "chrome126", "safari17", "safari18", "firefox120", "firefox128", "edge126", "random", "auto":
			// valid
		default:
			return fmt.Errorf("TLS_FINGERPRINT %q must be one of: chrome120, chrome126, safari17, safari18, firefox120, firefox128, edge126, random, auto", c.TLSFingerprint)
		}
	}

	if c.LogLevel != "" {
		if _, ok := ParseLevel(c.LogLevel); !ok {
			return fmt.Errorf("LOG_LEVEL %q must be one of: debug, info, warn, error, trace", c.LogLevel)
		}
	}
	switch c.LogFormat {
	case "", "text", "json":
		// "" never survives From (it defaults to "text"), accepted for
		// direct Config construction.
	default:
		return fmt.Errorf("LOG_FORMAT %q must be one of: text, json", c.LogFormat)
	}

	u, err := url.Parse(c.UpstreamBaseURL)
	if err != nil {
		return fmt.Errorf("UPSTREAM_BASE_URL %q is not a valid URL: %w", c.UpstreamBaseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("UPSTREAM_BASE_URL %q must be an http(s) URL", c.UpstreamBaseURL)
	}
	if u.Host == "" {
		return fmt.Errorf("UPSTREAM_BASE_URL %q has no host", c.UpstreamBaseURL)
	}
	return nil
}

// validateWebhookHost rejects webhook targets that resolve to a non-global
// host. IP literals are checked directly against the loopback, private
// (RFC1918/ULA), link-local (169.254.0.0/16 metadata, fe80::/10), multicast,
// and unspecified ranges. Hostnames are NOT resolved here — a resolution-time
// check would let a public hostname later rebind to a private address (DNS
// rebinding is out of scope; the operator owns WEBHOOK_URL, and the notify
// sender refuses to follow redirects), but the reserved loopback name
// "localhost" and any non-global literal are rejected so a pasted
// WEBHOOK_URL cannot silently target an internal service.
func validateWebhookHost(raw, host string) error {
	if host == "" {
		return fmt.Errorf("WEBHOOK_URL %q has no host", raw)
	}
	if strings.EqualFold(host, "localhost") {
		return fmt.Errorf("WEBHOOK_URL %q must not target loopback (localhost)", raw)
	}
	if ip := net.ParseIP(host); ip != nil {
		if isNonGlobalIP(ip) {
			return fmt.Errorf("WEBHOOK_URL %q must target a public host, got %q", raw, host)
		}
	}
	return nil
}

// isNonGlobalIP reports whether ip is not a routable public address: loopback,
// private (RFC1918 / IPv6 ULA), link-local unicast or multicast (the
// cloud-metadata endpoint 169.254.169.254 sits in the 169.254.0.0/16
// link-local range), unspecified, or multicast.
func isNonGlobalIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// normalizeUpstreamBaseURL trims a trailing slash, requires an http(s) URL,
// and rewrites the host codebuff.com to www.codebuff.com (the API only serves
// the www host; the bare host redirects).
func normalizeUpstreamBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(strings.TrimRight(raw, "/"))
	if raw == "" {
		return "", errors.New("UPSTREAM_BASE_URL cannot be empty")
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("UPSTREAM_BASE_URL %q is not a valid URL: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("UPSTREAM_BASE_URL %q must be an http(s) URL", raw)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("UPSTREAM_BASE_URL %q has no host", raw)
	}

	if strings.EqualFold(parsed.Host, "codebuff.com") {
		parsed.Host = "www.codebuff.com"
	}

	return strings.TrimRight(parsed.String(), "/"), nil
}
