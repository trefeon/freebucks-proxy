package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// validCatalogKinds is the fixed set of form-control kinds the dashboard
// renderer understands.
var validCatalogKinds = map[string]bool{
	"bool": true, "select": true, "int": true, "text": true, "secret": true, "list": true,
}

// dotenvKeys is the set of keys applyDotenv mirrors from a .env file onto
// rawConfig (config_load.go). It must stay in lock-step with the catalog:
// a key the loader parses but the catalog does not describe would be lost
// from the settings form, and a catalog key the loader cannot apply would
// be a phantom row.
var dotenvKeys = map[string]bool{
	"AUTH_TOKENS": true, "LISTEN_ADDR": true, "UPSTREAM_BASE_URL": true,
	"ROTATION_INTERVAL": true, "REQUEST_TIMEOUT": true, "SESSION_CALL_TIMEOUT": true,
	"API_KEYS": true, "ADMIN_TOKEN": true, "COST_MODE": true,
	"HTTP_READ_TIMEOUT": true,
	"ACTING_USER_ID":    true, "TLS_FINGERPRINT": true, "REGISTRY_REFRESH": true,
	"DEBUG_DUMP": true, "DEVTOOLS_ENABLED": true, "LOG_FILE": true,
	"LOG_LEVEL": true, "LOG_FORMAT": true, "LOG_ACCESS": true,
	"BRIDGE_ENABLED": true, "BRIDGE_IDLE_EVICT": true,
	"IDLE_ROTATION_TIMEOUT": true, "SAFE_MODE": true,
	"MODELS_HIDE_UNAVAILABLE": true, "MODELS_ALLOW": true, "CORS_ALLOWED_ORIGIN": true,
	"REQUEST_JITTER": true, "CLI_VERSION": true, "PIN_MODEL": true, "TRANSIENT_RETRIES": true,
	"SESSION_PERSIST": true, "SESSION_STATE_FILE": true,
	"HTTP2_UPSTREAM": true, "RUN_FINISH_QUEUE_SIZE": true,
	"RUN_FINISH_INLINE_TIMEOUT": true, "RUNS_DRAIN_QUEUE_CAP": true,
	"RUNS_DRAIN_TTL": true, "SESSION_RE_ADMIT_LEAD": true, "SESSION_PROBE_CACHE_TTL": true,
	"MODEL_UNAVAILABLE_CACHE_TTL": true, "QUEUE_DEPTH": true, "QUEUE_WAIT": true,
	"WEBHOOK_URL": true, "ADOPT_CLI_SESSION": true, "WAITING_ROOM_CHAIN": true,
	"RATE_LIMIT_PER_IP": true, "RATE_LIMIT_BURST": true, "SLOTS_PER_ACCOUNT": true, "MAX_SPILL_ACCOUNTS": true,
	"DASHBOARD_ENABLED": true, "DASHBOARD_REQUIRE_LOGIN": true,
	"COMPRESS_PROMPT": true, "CACHE_CONTROL_INJECTION": true, "REASONING_IN_CONTENT": true,
}

// catalogExtras are documented keys the catalog may hold beyond the
// applyDotenv set: knobs the loader does not apply to rawConfig because
// they are read straight from the environment at use time (AUTO_DISCOVER_TOKEN
// never reads .env; ADMIN_FORCE_SECURE_COOKIES additionally falls back to a
// .env line on every request in isSecureCookie).
var catalogExtras = map[string]bool{
	"AUTO_DISCOVER_TOKEN":        true,
	"ADMIN_FORCE_SECURE_COOKIES": true,
}

// secretKeys is the exact set of keys whose effective value the dashboard
// must mask: credentials by shape, or URLs that may carry credentials.
var secretKeys = map[string]bool{
	"ADMIN_TOKEN": true, "AUTH_TOKENS": true, "API_KEYS": true, "WEBHOOK_URL": true,
}

// TestCatalogSanity pins the catalog's own invariants: unique keys, known
// groups and kinds, enum presence exactly for selects, secrets flagged, and
// non-empty descriptions.
func TestCatalogSanity(t *testing.T) {
	catalog := Catalog()
	if len(catalog) == 0 {
		t.Fatal("catalog is empty")
	}
	seen := make(map[string]bool, len(catalog))
	validGroups := map[string]bool{
		GroupGeneral: true, GroupPool: true, GroupQuota: true, GroupUpstream: true, GroupSecurity: true,
	}
	for i, def := range catalog {
		if def.Key == "" {
			t.Errorf("entry %d: empty key", i)
		}
		if seen[def.Key] {
			t.Errorf("duplicate catalog key: %s", def.Key)
		}
		seen[def.Key] = true
		if !validGroups[def.Group] {
			t.Errorf("key %s: invalid group %q", def.Key, def.Group)
		}
		if !validCatalogKinds[def.Kind] {
			t.Errorf("key %s: invalid kind %q", def.Key, def.Kind)
		}
		if def.Kind == "select" {
			if len(def.Enum) == 0 {
				t.Errorf("key %s: select kind without enum", def.Key)
			}
			if def.Default != "" && !contains(def.Enum, def.Default) {
				t.Errorf("key %s: default %q not in enum %v", def.Key, def.Default, def.Enum)
			}
		} else if len(def.Enum) > 0 {
			t.Errorf("key %s: enum on non-select kind %q", def.Key, def.Kind)
		}
		if def.Description == "" {
			t.Errorf("key %s: missing description", def.Key)
		}
		if def.Kind == "secret" && !def.Secret {
			t.Errorf("key %s: secret kind without secret flag", def.Key)
		}
	}
}

// TestCatalogCoversApplyDotenvKeys pins the catalog against the loader: every
// key applyDotenv applies must be described, and no catalog key may be a
// phantom (not applied and not a documented env-only extra).
func TestCatalogCoversApplyDotenvKeys(t *testing.T) {
	catalog := Catalog()
	have := make(map[string]bool, len(catalog))
	for _, def := range catalog {
		have[def.Key] = true
	}
	for key := range dotenvKeys {
		if !have[key] {
			t.Errorf("loader parses %s but the catalog lacks it — add it to keyCatalog", key)
		}
	}
	for key := range have {
		if !dotenvKeys[key] && !catalogExtras[key] {
			t.Errorf("catalog documents %s which the loader can neither apply nor has as an env-only key", key)
		}
	}
}

// TestCatalogOrderedPins the emission order: groups in the fixed UI order
// (general, pool, quota, upstream, security), keys byte-ascending within
// each group. Consumers render by group order, so the array must be stable.
func TestCatalogOrdered(t *testing.T) {
	catalog := Catalog()
	groupIndex := make(map[string]int, len(catalogGroupOrder))
	for i, g := range catalogGroupOrder {
		groupIndex[g] = i
	}
	prevGroup := ""
	for i, def := range catalog {
		if groupIndex[def.Group] < groupIndex[prevGroup] && prevGroup != "" {
			t.Errorf("group %q out of order: appears before group %q", def.Group, prevGroup)
		}
		if i > 0 && catalog[i-1].Group == def.Group && catalog[i-1].Key >= def.Key {
			t.Errorf("key %s out of order within group %s (after %s)", def.Key, def.Group, catalog[i-1].Key)
		}
		prevGroup = def.Group
	}
	// Byte-ascending within group must match sort.Strings exactly.
	for _, g := range catalogGroupOrder {
		var keys []string
		var groupKeys []string
		for _, def := range catalog {
			if def.Group == g {
				keys = append(keys, def.Key)
				groupKeys = append(groupKeys, def.Key)
			}
		}
		sort.Strings(keys)
		for i := range keys {
			if keys[i] != groupKeys[i] {
				t.Errorf("group %s not sorted: %v", g, groupKeys)
				break
			}
		}
	}
}

// TestCatalogSecretFlags pins the exact secret set so a credential-bearing
// key added to the loader never reaches the dashboard unmasked, and a
// mistakenly masked key is caught early.
func TestCatalogSecretFlags(t *testing.T) {
	catalog := Catalog()
	flagged := make(map[string]bool)
	for _, def := range catalog {
		if def.Secret {
			flagged[def.Key] = true
		}
	}
	if len(flagged) != len(secretKeys) {
		t.Errorf("secret count = %d, want %d (%v)", len(flagged), len(secretKeys), flagged)
	}
	for key := range secretKeys {
		if !flagged[key] {
			t.Errorf("key %s must be flagged secret", key)
		}
	}
	for key := range flagged {
		if !secretKeys[key] {
			t.Errorf("key %s is flagged secret but is not in the expected secret set", key)
		}
	}
}

// configMetaFixtures are the frontend e2e mock packs that must each carry
// Catalog() exactly: the shared dashboard pack and the real-world pack
// (realworld.spec.ts / flows.spec.ts). A pack that drifts from the catalog
// renders dead rows the backend cannot apply and hides live knobs, while the
// spec consuming it stays green — which is why every pack is pinned here.
var configMetaFixtures = []struct{ name, path string }{
	{"fixtures", filepath.Join("..", "..", "..", "frontend", "e2e", "fixtures", "config-meta.json")},
	{"fixtures-realworld", filepath.Join("..", "..", "..", "frontend", "e2e", "fixtures-realworld", "config-meta.json")},
}

// TestConfigMetaFixtureParity asserts that every frontend e2e mock fixture in
// configMetaFixtures decodes to exactly Catalog(). Run with FP_REGEN_FIXTURE=1
// to regenerate the fixtures (writes the canonical Go-marshaled JSON; the
// real-world pack is prettier-formatted, so follow it with `npm run format`).
//
// Packs are compared by decoded value rather than byte-for-byte: a JSON
// formatter may reflow them (frontend/.prettierignore covers e2e/fixtures/
// only), and whitespace drift says nothing about whether the rows the
// dashboard renders still exist in the backend.
func TestConfigMetaFixtureParity(t *testing.T) {
	catalog := Catalog()
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')

	for _, fixture := range configMetaFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			if os.Getenv("FP_REGEN_FIXTURE") != "" {
				if err := os.WriteFile(fixture.path, data, 0o644); err != nil {
					t.Fatalf("write fixture: %v", err)
				}
				t.Logf("regenerated %s", fixture.path)
				return
			}

			raw, err := os.ReadFile(fixture.path)
			if err != nil {
				if os.IsNotExist(err) {
					t.Skip("fixture does not exist; skipping parity check")
				}
				t.Fatal(err)
			}
			var got []KeyDef
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&got); err != nil {
				t.Fatalf("fixture %s does not decode as a config catalog: %v", fixture.path, err)
			}
			if diff := catalogFixtureDiff(catalog, got); diff != "" {
				t.Errorf("frontend e2e fixture %s is out of date with Catalog():\n%s\nre-run with FP_REGEN_FIXTURE=1 (then `npm run format`) to regenerate", fixture.path, diff)
			}
		})
	}
}

// catalogFixtureDiff describes how a decoded fixture diverges from the
// catalog: keys the catalog dropped but the fixture still renders, live keys
// the fixture cannot render, order drift, and per-key field drift. It returns
// "" when the two are equal.
func catalogFixtureDiff(want, got []KeyDef) string {
	var b strings.Builder
	wantAt := make(map[string]int, len(want))
	gotAt := make(map[string]int, len(got))
	for i, def := range want {
		wantAt[def.Key] = i
	}
	for i, def := range got {
		gotAt[def.Key] = i
	}
	for _, def := range want {
		if _, ok := gotAt[def.Key]; !ok {
			fmt.Fprintf(&b, "  missing key %s\n", def.Key)
		}
	}
	for _, def := range got {
		if _, ok := wantAt[def.Key]; !ok {
			fmt.Fprintf(&b, "  dead key %s\n", def.Key)
		}
	}
	for i, def := range want {
		j, ok := gotAt[def.Key]
		if !ok {
			continue
		}
		if i != j {
			fmt.Fprintf(&b, "  key %s is at index %d, want %d\n", def.Key, j, i)
		}
		if reflect.DeepEqual(def, got[j]) {
			continue
		}
		wantJSON, _ := json.Marshal(def)
		gotJSON, _ := json.Marshal(got[j])
		fmt.Fprintf(&b, "  key %s differs:\n    want %s\n    got  %s\n", def.Key, wantJSON, gotJSON)
	}
	return b.String()
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
