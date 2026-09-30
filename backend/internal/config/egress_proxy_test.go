package config

import (
	"strings"
	"testing"
	"time"
)

func goodEgressBase() Config {
	return Config{
		ListenAddr:         ":3457",
		UpstreamBaseURL:    "https://www.codebuff.com",
		AuthTokens:         []string{"tok"},
		RotationInterval:   6 * time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 30 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		MaturityTargetDays: 7,
	}
}

// TestValidateUpstreamEgressProxy pins the knob's validation: empty (direct)
// passes, http/https/socks5 with optional userinfo pass, anything else fails.
func TestValidateUpstreamEgressProxy(t *testing.T) {
	accept := []string{
		"",
		"http://127.0.0.1:40000",
		"https://exit.example:8080",
		"socks5://127.0.0.1:40000",
		"socks5://user:pass@exit.example:1080",
	}
	for _, raw := range accept {
		c := goodEgressBase()
		c.UpstreamEgressProxy = raw
		if err := c.Validate(); err != nil {
			t.Errorf("Validate(UPSTREAM_EGRESS_PROXY=%q) = %v, want nil", raw, err)
		}
	}
	reject := []string{
		"ftp://exit.example:21",
		"http://",
		"socks5://",
		"://missing-scheme",
		"not a url",
		"exit.example:8080",
	}
	for _, raw := range reject {
		c := goodEgressBase()
		c.UpstreamEgressProxy = raw
		if err := c.Validate(); err == nil {
			t.Errorf("Validate(UPSTREAM_EGRESS_PROXY=%q) succeeded, want rejection", raw)
		}
	}
}

// TestEgressProxyURL pins the parsed accessor: nil/empty/host-less yield the
// direct path (nil); valid values round-trip scheme, host and userinfo.
func TestEgressProxyURL(t *testing.T) {
	var nilCfg *Config
	if got := nilCfg.EgressProxyURL(); got != nil {
		t.Errorf("nil Config EgressProxyURL() = %v, want nil", got)
	}
	if got := (&Config{}).EgressProxyURL(); got != nil {
		t.Errorf("empty knob EgressProxyURL() = %v, want nil (direct)", got)
	}
	if got := (&Config{UpstreamEgressProxy: "http://"}).EgressProxyURL(); got != nil {
		t.Errorf("host-less knob EgressProxyURL() = %v, want nil (direct fallback)", got)
	}
	got := (&Config{UpstreamEgressProxy: "socks5://user:pass@exit.example:1080"}).EgressProxyURL()
	if got == nil {
		t.Fatal("valid knob EgressProxyURL() = nil, want parsed URL")
	}
	if got.Scheme != "socks5" || got.Host != "exit.example:1080" {
		t.Errorf("EgressProxyURL() = %v, want socks5://exit.example:1080", got)
	}
	if got.User == nil || got.User.Username() != "user" {
		t.Errorf("EgressProxyURL() user = %v, want user", got.User)
	}
}

// TestUpstreamEgressProxyLoadsFromEnv pins the loader path: the env var (and
// hence a .env line) lands on the field and survives Validate.
func TestUpstreamEgressProxyLoadsFromEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("UPSTREAM_EGRESS_PROXY", "socks5://127.0.0.1:40000")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UpstreamEgressProxy != "socks5://127.0.0.1:40000" {
		t.Errorf("UpstreamEgressProxy = %q, want the env value", cfg.UpstreamEgressProxy)
	}
	if cfg.EgressProxyURL() == nil {
		t.Error("EgressProxyURL() = nil after env load, want parsed URL")
	}
}

// TestUpstreamEgressProxyRedacted pins the redaction contract: the dashboard
// and admin surfaces render presence only — a URL carrying user:pass must
// never leak host or credentials into a rendered value.
func TestUpstreamEgressProxyRedacted(t *testing.T) {
	const secret = "socks5://user:s3cret@exit.example:1080"
	val, isSecret := renderKey(&Config{UpstreamEgressProxy: secret}, "UPSTREAM_EGRESS_PROXY")
	if !isSecret {
		t.Error("renderKey not flagged secret, want masked in dashboard/admin surfaces")
	}
	if val != "set" {
		t.Errorf("renderKey = %q, want presence word %q", val, "set")
	}
	for _, leak := range []string{"s3cret", "user", "exit.example"} {
		if strings.Contains(val, leak) {
			t.Errorf("renderKey = %q, leaks %q", val, leak)
		}
	}
	if val, _ := renderKey(&Config{}, "UPSTREAM_EGRESS_PROXY"); val != "unset" {
		t.Errorf("unset renderKey = %q, want %q", val, "unset")
	}
}

// TestUpstreamEgressProxyOverlayRoundTrip pins the migration contract for a
// configured knob: the overlay export must carry the raw URL (never the
// masked presence word), so a DB-alone boot reproduces the exit exactly —
// credentials included.
func TestUpstreamEgressProxyOverlayRoundTrip(t *testing.T) {
	const raw = "socks5://user:s3cret@exit.example:1080"
	cfg := goodEgressBase()
	cfg.UpstreamEgressProxy = raw
	exported := EffectiveOverlayMap(cfg)
	if got := exported["UPSTREAM_EGRESS_PROXY"]; got != raw {
		t.Fatalf("overlay export = %q, want raw %q", got, raw)
	}
	rows := map[string]string{OverlayRowKey("UPSTREAM_EGRESS_PROXY"): exported["UPSTREAM_EGRESS_PROXY"]}
	ov := OverlayFromRows(rows)
	if ov["UPSTREAM_EGRESS_PROXY"] != raw {
		t.Fatalf("OverlayFromRows dropped the exit row: %v", ov)
	}
	clearEnv(t)
	t.Chdir(t.TempDir())
	reloaded, err := LoadOpts("", LoadOptions{Overlay: ov})
	if err != nil {
		t.Fatalf("LoadOpts from overlay: %v", err)
	}
	if reloaded.UpstreamEgressProxy != raw {
		t.Errorf("reloaded knob = %q, want %q", reloaded.UpstreamEgressProxy, raw)
	}
}
