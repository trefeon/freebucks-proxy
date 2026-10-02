// Catalog key → friendly model id for display (genuine-CLI parity).
//
// A catalog-mode admission 200 answers the sent handle with the opaque
// catalog row key in `model` (e.g. m-096e75164d), and that key is what the
// session manager stores (session/session_admission.go) and the pool
// snapshot carries (pool/snapshot.go). The genuine CLI resolves the key to
// the friendly model id for display from its held catalog rows; without
// that the dashboard session block renders the raw m-* key.
//
// Resolution is exact, never guessed:
//  1. An already-friendly value (a served model id, e.g. from a
//     fallback-mode admission that sent the raw id) passes through.
//  2. Live held rows: the row whose Key equals the key names its model
//     either by exact key (same as 1) or by legacy digest —
//     row.LegacyDigests holds freebuffLegacyModelDigest(friendly id)
//     (vendor findFreebuffCatalogRowForLegacyId), so the digest is
//     reversed against the static catalog id universe.
//  3. Static fallback for wire-observed keys when no catalog is held
//     (cold boot before the first fetch).
//  4. Anything else passes through verbatim: never blank, never invented.
package upstream

import (
	"freebuff-proxy/backend/internal/modelcat"
)

// staticCatalogKeyToID pins catalog row keys observed on the wire to the
// friendly id the genuine CLI displayed for the same key. Each entry needs
// capture evidence, never inference: m-096e75164d admitted the turn whose
// SSE chunks carried model deepseek/deepseek-v4-flash
// (docs/CLI-PARITY-PORT.md §§1.2/4.1). Keys seen but never display-named in
// the visible log (m-22ff…, m-a273…, m-916b…) stay OUT — they resolve via
// live rows (rule 2) once a catalog is held, else passthrough (rule 4).
var staticCatalogKeyToID = map[string]string{
	"m-096e75164d": modelcat.DeepSeekV4FlashModelID,
}

// friendlyIDForCatalogRow returns the friendly model id a catalog row names,
// or "" when the row names nothing in the static id universe.
func friendlyIDForCatalogRow(row modelCatalogRow) string {
	if modelcat.IsServed(row.Key) {
		return row.Key
	}
	for i := range modelcat.Catalog {
		id := modelcat.Catalog[i].ID
		if id == "" {
			continue
		}
		digest := freebuffLegacyModelDigest(id)
		for _, d := range row.LegacyDigests {
			if d == digest {
				return id
			}
		}
	}
	return ""
}

// CatalogKeyToModelID resolves an admission-200 session model value (an
// opaque catalog row key in catalog mode, a friendly id in fallback mode)
// to the friendly model id for display. Unknown keys pass through verbatim
// and "" stays "".
func CatalogKeyToModelID(key string) string {
	if key == "" {
		return ""
	}
	if modelcat.IsServed(key) {
		return key
	}
	catalogMu.Lock()
	defer catalogMu.Unlock()
	for _, e := range catalogEntries {
		if e == nil || e.catalog == nil {
			continue
		}
		for _, row := range e.catalog.Rows {
			if row.Key != key {
				continue
			}
			if id := friendlyIDForCatalogRow(row); id != "" {
				return id
			}
		}
	}
	if id, ok := staticCatalogKeyToID[key]; ok {
		return id
	}
	return key
}
