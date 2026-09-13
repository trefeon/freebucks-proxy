package dashboard

// rate_tokens passthrough: the chat trace logs the acquire-time limited
// set verbatim (comma-joined 1-based); the dashboard parses it like token
// (string passthrough, omitempty) and only falls back to TOKEN — when both
// are absent.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTraceFromFieldsRateTokens(t *testing.T) {
	e := traceFromFields("2026-09-14T00:00:00Z", []string{
		"token=2", "rate_tokens=1,2", "model=m", "status=error", "ms=9", "error=rate_limited",
	})
	if e.Token != "2" {
		t.Errorf("token = %q, want 2", e.Token)
	}
	if e.RateTokens != "1,2" {
		t.Errorf("rate_tokens = %q, want 1,2", e.RateTokens)
	}
	raw, _ := json.Marshal(e)
	if !strings.Contains(string(raw), `"rate_tokens":"1,2"`) {
		t.Errorf("trace JSON omits rate_tokens: %s", raw)
	}

	// rate_tokens without token: no TOKEN — fallback — the limited set is
	// still the attribution signal.
	only := traceFromFields("2026-09-14T00:00:00Z", []string{"rate_tokens=1,3", "model=m"})
	if only.Token != "" || only.RateTokens != "1,3" {
		t.Errorf("rate-only row = token %q rate_tokens %q, want \"\"/\"1,3\"", only.Token, only.RateTokens)
	}

	// Neither present: the row keeps TOKEN — and omits the key.
	bare := traceFromFields("2026-09-14T00:00:00Z", []string{"model=m"})
	if bare.Token != "—" {
		t.Errorf("bare token = %q, want —", bare.Token)
	}
	raw, _ = json.Marshal(bare)
	if strings.Contains(string(raw), `"rate_tokens"`) {
		t.Errorf("bare trace JSON contains rate_tokens, want omitted: %s", raw)
	}
}
