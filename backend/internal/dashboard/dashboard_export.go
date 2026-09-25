package dashboard

import (
	"freebucks-proxy/backend/internal/config"
	"strings"
)

// renderEnvExport renders the live configuration as a dotenv document for
// GET /admin/api/config's env_content (unified store, Lane D).
//
// The export is rendered from the mem snapshot (the running config), never
// from the .env file: the file is boot seed only, so file bytes go stale
// the moment an overlay row diverges — the split-brain class that motivated
// the unified store. Rendering from mem keeps the dashboard's break-glass
// copy and its key-list merge base (Client API Keys) on live truth without
// a file read on the request path.
//
// Shape: KEY=value lines in catalog order (the same order as effective[]).
// Non-secret values reuse the canonical effective strings (durations,
// bools, joined lists — all re-parseable by the loader); the four secret
// keys render raw so an admin can copy a working seed. Exposure is
// unchanged: this endpoint is admin-authenticated and previously served the
// raw file, secrets in clear, to the same callers.
func renderEnvExport(cfg *config.Config) string {
	var b strings.Builder
	b.WriteString("# Live configuration export (break-glass copy).\n")
	b.WriteString("# Rendered from the running snapshot; the .env file is boot seed only.\n")
	for _, entry := range cfg.Data() {
		val := entry.Value
		switch entry.Key {
		case "AUTH_TOKENS":
			val = strings.Join(cfg.AuthTokens, ",")
		case "API_KEYS":
			val = strings.Join(cfg.APIKeys, ",")
		case "ADMIN_TOKEN":
			val = cfg.AdminToken
		case "WEBHOOK_URL":
			val = cfg.WebhookURL
		}
		b.WriteString(entry.Key + "=" + val + "\n")
	}
	return b.String()
}
