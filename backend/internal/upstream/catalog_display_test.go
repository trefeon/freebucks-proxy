package upstream

import (
	"freebuff-proxy/backend/internal/modelcat"
	"testing"
)

// The live hour ships the opaque catalog row key as the session model; the
// dashboard must show the friendly id the genuine CLI displays.
func TestCatalogKeyToModelIDStaticFallback(t *testing.T) {
	resetModelCatalogsForTest()
	defer resetModelCatalogsForTest()
	if got := CatalogKeyToModelID("m-096e75164d"); got != modelcat.DeepSeekV4FlashModelID {
		t.Errorf("static m-096e75164d = %q, want %q", got, modelcat.DeepSeekV4FlashModelID)
	}
}

// Live held rows resolve by legacy digest without any static entry.
func TestCatalogKeyToModelIDLiveRow(t *testing.T) {
	resetModelCatalogsForTest()
	defer resetModelCatalogsForTest()
	const liveKey = "m-live0001"
	catalogMu.Lock()
	catalogEntries["test-account"] = &catalogEntry{catalog: &modelCatalog{Rows: []modelCatalogRow{
		{Key: liveKey, Handle: "fbm1.live", LegacyDigests: []string{freebuffLegacyModelDigest(modelcat.FallbackModelID)}},
	}}}
	catalogMu.Unlock()
	if got := CatalogKeyToModelID(liveKey); got != modelcat.FallbackModelID {
		t.Errorf("live %s = %q, want %q", liveKey, got, modelcat.FallbackModelID)
	}
	// The static key resolves identically while a (non-matching) catalog is held.
	if got := CatalogKeyToModelID("m-096e75164d"); got != modelcat.DeepSeekV4FlashModelID {
		t.Errorf("static m-096e75164d with catalog held = %q, want %q", got, modelcat.DeepSeekV4FlashModelID)
	}
}

// Unknown keys pass through verbatim; empty stays empty; friendly ids are
// untouched (fallback-mode admissions already store the friendly id).
func TestCatalogKeyToModelIDPassthrough(t *testing.T) {
	resetModelCatalogsForTest()
	defer resetModelCatalogsForTest()
	if got := CatalogKeyToModelID("m-xxx-unknown"); got != "m-xxx-unknown" {
		t.Errorf("unknown key = %q, want verbatim passthrough", got)
	}
	if got := CatalogKeyToModelID(""); got != "" {
		t.Errorf("empty = %q, want empty (never fabricate)", got)
	}
	if got := CatalogKeyToModelID(modelcat.DeepSeekV4FlashModelID); got != modelcat.DeepSeekV4FlashModelID {
		t.Errorf("friendly id = %q, want untouched", got)
	}
}
