package dashboard

import (
	"net/http"
)

// --- client setup ---

type setupData struct {
	BaseURL    string   `json:"base_url"`
	KeyHint    string   `json:"key_hint"`
	Model      string   `json:"model"`
	Models     []string `json:"models"`
	Mode       string   `json:"mode"`
	TokenCount int      `json:"token_count"`
	HasTokens  bool     `json:"has_tokens"`
}

func (d *Dashboard) setupData(r *http.Request) setupData {
	cfg := d.cfg()
	sd := setupData{
		BaseURL:    baseURLForRequest(cfg, r),
		Mode:       cfg.EffectiveMode(),
		TokenCount: d.pool.TokenCount(),
		Models:     servedModels(d.reg),
	}
	sd.HasTokens = sd.TokenCount > 0
	if len(sd.Models) > 0 {
		sd.Model = pickDefaultModel(sd.Models)
	}
	sd.KeyHint = "sk-any (pooled mode; the proxy picks from AUTH_TOKENS)"
	return sd
}
