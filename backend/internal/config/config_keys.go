package config

// config_keys.go - raw key declarations, split from config.go (Wave C revamp):
// the rawConfig key mirror, the lenient list/map JSON shapes, and the
// built-in defaults. The load precedence pipeline lives in config_load.go.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// rawConfig mirrors the JSON file / env keys as strings so that parsing and
// validation happen once, after all overrides are applied.
type rawConfig struct {
	ListenAddr      string   `json:"LISTEN_ADDR"`
	UpstreamBaseURL string   `json:"UPSTREAM_BASE_URL"`
	AuthTokens      []string `json:"AUTH_TOKENS"`
	// AuthTokensSet records that AUTH_TOKENS was explicitly provided (even
	// as an empty value) by the JSON file, .env, or the environment. An
	// explicitly-empty AUTH_TOKENS means the operator chose bridge mode, so
	// CLI auto-discovery must not refill it (runtime mode switch persists
	// "AUTH_TOKENS=" to .env and relies on this).
	AuthTokensSet      bool     `json:"-"`
	RotationInterval   string   `json:"ROTATION_INTERVAL"`
	RequestTimeout     string   `json:"REQUEST_TIMEOUT"`
	SessionCallTimeout string   `json:"SESSION_CALL_TIMEOUT"`
	HTTPReadTimeout    string   `json:"HTTP_READ_TIMEOUT"`
	APIKeys            []string `json:"API_KEYS"`
	AdminToken         string   `json:"ADMIN_TOKEN"`
	CostMode           string   `json:"COST_MODE"`
	ActingUserID       string   `json:"ACTING_USER_ID"`
	// LegacyActingUserID is the pre-rename JSON key (USER_ID) — merged at
	// the end of Load when no ACTING_USER_ID source set a value (#126).
	LegacyActingUserID string `json:"USER_ID"`
	TLSFingerprint     string `json:"TLS_FINGERPRINT"`
	RegistryRefresh    string `json:"REGISTRY_REFRESH"`
	DebugDump          bool   `json:"DEBUG_DUMP"`
	DevToolsEnabled    bool   `json:"DEVTOOLS_ENABLED"`
	LogFile            string `json:"LOG_FILE"`
	LogLevel           string `json:"LOG_LEVEL"`
	LogFormat          string `json:"LOG_FORMAT"`
	LogAccess          bool   `json:"LOG_ACCESS"`
	// BridgeEnabled records BRIDGE_ENABLED (default true via
	// defaultRawConfig): whether bridge-mode traffic is accepted alongside
	// the AUTH_TOKENS pool (hybrid mode).
	BridgeEnabled bool `json:"BRIDGE_ENABLED"`
	// BridgeIdleEvict is the sliding-TTL string for idle bridge-entry
	// eviction (BRIDGE_IDLE_EVICT; default "72h", zero-tolerant → 72h).
	BridgeIdleEvict          string          `json:"BRIDGE_IDLE_EVICT"`
	IdleRotationTimeout      string          `json:"IDLE_ROTATION_TIMEOUT"`
	SafeMode                 bool            `json:"SAFE_MODE"`
	ModelsHideUnavailable    bool            `json:"MODELS_HIDE_UNAVAILABLE"`
	ModelsAllow              modelsAllowList `json:"MODELS_ALLOW"`
	CORSAllowedOrigin        string          `json:"CORS_ALLOWED_ORIGIN"`
	RequestJitter            string          `json:"REQUEST_JITTER"`
	CLIVersion               string          `json:"CLI_VERSION"`
	TransientRetries         *int            `json:"TRANSIENT_RETRIES"`
	SessionPersist           bool            `json:"SESSION_PERSIST"`
	SessionStateFile         string          `json:"SESSION_STATE_FILE"`
	HTTP2Upstream            bool            `json:"HTTP2_UPSTREAM"`
	RunFinishQueueSize       *int            `json:"RUN_FINISH_QUEUE_SIZE"`
	RunFinishInlineTimeout   string          `json:"RUN_FINISH_INLINE_TIMEOUT"`
	RunsDrainQueueCap        *int            `json:"RUNS_DRAIN_QUEUE_CAP"`
	RunsDrainTTL             string          `json:"RUNS_DRAIN_TTL"`
	SessionReAdmitLead       string          `json:"SESSION_RE_ADMIT_LEAD"`
	SessionProbeCacheTTL     string          `json:"SESSION_PROBE_CACHE_TTL"`
	ModelUnavailableCacheTTL string          `json:"MODEL_UNAVAILABLE_CACHE_TTL"`
	WebhookURL               string          `json:"WEBHOOK_URL"`
	AdoptCLISession          bool            `json:"ADOPT_CLI_SESSION"`
	WaitingRoomChain         bool            `json:"WAITING_ROOM_CHAIN"`
	RateLimitPerIP           *float64        `json:"RATE_LIMIT_PER_IP"`
	RateLimitBurst           *int            `json:"RATE_LIMIT_BURST"`
	PinModel                 string          `json:"PIN_MODEL"`
	DashboardEnabled         bool            `json:"DASHBOARD_ENABLED"`
	DashboardRequireLogin    bool            `json:"DASHBOARD_REQUIRE_LOGIN"`
	CompressPrompt           string          `json:"COMPRESS_PROMPT"`
	CacheControlInjection    string          `json:"CACHE_CONTROL_INJECTION"`
	ReasoningInContent       string          `json:"REASONING_IN_CONTENT"`
	// SlotsPerAccount records SLOTS_PER_ACCOUNT (default 2, floor
	// 1): the per account-model live-turn cap.
	SlotsPerAccount *int `json:"SLOTS_PER_ACCOUNT"`
	// QueueWait records QUEUE_WAIT (default "30s"): the FIFO slot-queue
	// wait bound.
	QueueWait string `json:"QUEUE_WAIT"`
	// QueueDepth records QUEUE_DEPTH (default 16): the per-token FIFO
	// queue depth cap.
	QueueDepth *int `json:"QUEUE_DEPTH"`
	// MaxSpillAccounts records MAX_SPILL_ACCOUNTS (default 0): the spill
	// walk bound, 0 = unbounded.
	MaxSpillAccounts *int `json:"MAX_SPILL_ACCOUNTS"`
}

// modelsAllowList is the raw MODELS_ALLOW value. The README documents list
// values as comma-separated in env and arrays in JSON, but operators write
// JSON configs by hand — accepting a plain comma-separated string here too
// avoids a hard parse error for the most natural single-value form. Both
// shapes are normalized to a comma-separated string; Config.ModelsAllow
// parses it with splitList in Load.
type modelsAllowList string

func (m *modelsAllowList) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*m = modelsAllowList(s)
		return nil
	}
	var arr []string
	if err := json.Unmarshal(data, &arr); err != nil {
		return fmt.Errorf("MODELS_ALLOW must be a comma-separated string or an array of strings, got: %s", data)
	}
	*m = modelsAllowList(strings.Join(arr, ","))
	return nil
}

// defaultRawConfig returns the raw defaults every load source layers over.
func defaultRawConfig() rawConfig {
	return rawConfig{
		ListenAddr:             "127.0.0.1:3457",       // loopback by default (PRD §3); containers set LISTEN_ADDR=:3457
		UpstreamBaseURL:        "https://codebuff.com", // normalized to www.
		RotationInterval:       "6h",
		RequestTimeout:         "15m",
		HTTPReadTimeout:        "60s",
		SessionCallTimeout:     "30s",
		CostMode:               "free",
		RegistryRefresh:        "6h",
		IdleRotationTimeout:    "",    // "" = disabled (unset → SAFE_MODE preset may fill)
		BridgeEnabled:          true,  // hybrid by default: AUTH_TOKENS + bridge relay share one instance
		BridgeIdleEvict:        "72h", // sliding-TTL for idle bridge-entry eviction
		SafeMode:               true,  // anti-ban presets on by default; set SAFE_MODE=false to disable
		DashboardEnabled:       true,  // dashboard on by default; set DASHBOARD_ENABLED=false to disable
		DashboardRequireLogin:  true,  // require login on by default; set DASHBOARD_REQUIRE_LOGIN=false to disable
		LogAccess:              true,
		DevToolsEnabled:        false, // per-request access lines on by default; LOG_ACCESS=false disables them
		CORSAllowedOrigin:      "*",   // browser clients reach /v1/* cross-origin by default
		RequestJitter:          "",    // "" = disabled (unset → SAFE_MODE preset may fill)
		CLIVersion:             "0.10.7",
		TransientRetries:       nil,  // nil = 1 (one retry after a transient transport failure; 0 disables)
		SessionPersist:         true, // session persistence on by default: restart resumes unexpired sessions
		SessionStateFile:       ".freebuff-session-state.json",
		HTTP2Upstream:          true,       // h2 ALPN matches real browsers (reference proxy-freebuff USE_HTTP2 default '1'); HTTP2_UPSTREAM=false forces h1 (#51)
		RunFinishQueueSize:     ptrInt(64), // #90: bounded deferred-FINISH queue
		RunFinishInlineTimeout: "250ms",    // #90: inline FINISH fallback bound
		RunsDrainQueueCap:      ptrInt(64), // #55: draining-runs list cap
		RunsDrainTTL:           "10m",      // #55: draining-runs TTL eviction
		QueueWait:              "30s",      // FIFO slot-queue wait bound per parked Acquire
		QueueDepth:             ptrInt(16), // parked FIFO waiters per token (0 = fail over at once when full)
		SlotsPerAccount:        ptrInt(2),  // per account-model live turns (floor 1; bunker strictness is 1)
		MaxSpillAccounts:       ptrInt(0),  // spill walk bound (0 = unbounded index chain)
	}
}

// ptrInt returns a pointer to n for *int raw fields with a non-nil default.
func ptrInt(n int) *int { return &n }
