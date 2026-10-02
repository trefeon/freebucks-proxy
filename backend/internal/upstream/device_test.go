// Device-signing regression tests (vendor cli/src/utils/freebuff-device-key.ts
// + common/src/util/freebuff-device-signing.ts): the proxy signs catalog-mode
// calls with a per-account Ed25519 key over the exact vendor payload shape,
// registers the public key once per account, re-registers after an
// unknown-key refusal, and sends pre-catalog servers no registration traffic
// at all. Mock-upstream only; never live traffic.
package upstream

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// spoofFake serves the catalog GET, device-keys POST, admission POST and chat
// POST from scripted sequences for the CLI-spoof wire tests. An empty
// catalog body answers 404 (a server predating catalogs); noDeviceKeys
// answers the registration 404 (a server predating device keys), in which
// case the account stays unsigned. Admission answers past the end of admitSeq
// repeat the last one.
type spoofFake struct {
	mu               sync.Mutex
	catalogSeq       []string
	noDeviceKeys     bool
	deviceKeysSeq    []spoofDeviceResp
	admitSeq         []catalogAdmitResp
	chatStatus       int
	chatBody         string
	catalogHits      int
	catalogHeaders   []http.Header
	deviceKeysHits   int
	deviceKeysBodies []string
	admitHeaders     []http.Header
	chatHeaders      []http.Header
	chatBodies       []string
	sessionHeaders   []http.Header
}

type spoofDeviceResp struct {
	status int
	body   string
}

func (f *spoofFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == ModelCatalogPath && r.Method == http.MethodGet:
		f.mu.Lock()
		f.catalogHits++
		f.catalogHeaders = append(f.catalogHeaders, r.Header.Clone())
		body := ""
		if len(f.catalogSeq) > 0 {
			body = f.catalogSeq[min(f.catalogHits-1, len(f.catalogSeq)-1)]
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if body == "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	case r.URL.Path == DeviceKeysPath && r.Method == http.MethodPost:
		raw := drainBody(r.Body)
		f.mu.Lock()
		f.deviceKeysHits++
		f.deviceKeysBodies = append(f.deviceKeysBodies, raw)
		n := f.deviceKeysHits
		resp := spoofDeviceResp{status: http.StatusOK, body: fmt.Sprintf(`{"keyId":"dk-%d"}`, n)}
		if len(f.deviceKeysSeq) > 0 {
			resp = f.deviceKeysSeq[min(n-1, len(f.deviceKeysSeq)-1)]
		}
		noKeys := f.noDeviceKeys
		f.mu.Unlock()
		if noKeys {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.body))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, SessionAdmissionPath):
		f.mu.Lock()
		f.admitHeaders = append(f.admitHeaders, r.Header.Clone())
		resp := catalogAdmitResp{status: http.StatusOK, body: `{"status":"active","instanceId":"inst-1","expiresAt":"2030-01-01T00:00:00Z"}`}
		if len(f.admitSeq) > 0 {
			resp = f.admitSeq[min(len(f.admitHeaders)-1, len(f.admitSeq)-1)]
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.body))
	case r.URL.Path == "/api/v1/chat/completions" && r.Method == http.MethodPost:
		sent := drainBody(r.Body)
		f.mu.Lock()
		f.chatHeaders = append(f.chatHeaders, r.Header.Clone())
		f.chatBodies = append(f.chatBodies, sent)
		status, body := f.chatStatus, f.chatBody
		f.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		if body == "" {
			body = "data: [DONE]\n\n"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	case r.URL.Path == "/api/v1/ads" && r.Method == http.MethodPost:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ads":[]}`))
	case strings.HasPrefix(r.URL.Path, "/api/v1/ads/") && r.Method == http.MethodPost:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	case r.URL.Path == "/api/v1/freebuff/session" && r.Method == http.MethodGet:
		f.mu.Lock()
		f.sessionHeaders = append(f.sessionHeaders, r.Header.Clone())
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"none"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}
}

func (f *spoofFake) deviceHits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deviceKeysHits
}

// fixedDeviceSeed is the deterministic Ed25519 seed for byte-exact tests —
// never a live key.
func fixedDeviceSeed() []byte {
	seed := make([]byte, ed25519.SeedSize)
	for i := range len(seed) {
		seed[i] = byte(i + 1)
	}
	return seed
}

// verifyDeviceHeaders checks the trio against the vendor payload shape with
// the given public key and returns the fetch id the signature covered.
func verifyDeviceHeaders(t *testing.T, h http.Header, pub ed25519.PublicKey, method, path, body string, wantKeyID string) {
	t.Helper()
	if got := h.Get(DeviceKeyHeader); got != wantKeyID {
		t.Fatalf("%s = %q, want %q", DeviceKeyHeader, got, wantKeyID)
	}
	ts := h.Get(DeviceTimestampHeader)
	if ts == "" {
		t.Fatalf("%s absent", DeviceTimestampHeader)
	}
	var tsMs int64
	if _, err := fmt.Sscan(ts, &tsMs); err != nil || tsMs <= 0 {
		t.Fatalf("%s = %q, want millis since epoch", DeviceTimestampHeader, ts)
	}
	if now := time.Now().UnixMilli(); tsMs > now+60_000 || tsMs < now-120_000 {
		t.Fatalf("%s = %q, want a current timestamp", DeviceTimestampHeader, ts)
	}
	sigB64 := h.Get(DeviceSignatureHeader)
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		t.Fatalf("%s = %q, want base64url %d-byte signature", DeviceSignatureHeader, sigB64, ed25519.SignatureSize)
	}
	fetchID := h.Get(CatalogFetchHeader)
	payload := deviceSignaturePayload(method, path, tsMs, deviceBodySHA256Hex([]byte(body)), fetchID)
	if !ed25519.Verify(pub, []byte(payload), sig) {
		t.Fatalf("signature does not verify over %q", payload)
	}
}

// The payload is the vendor freebuffDeviceSignaturePayload shape, byte for
// byte: domain, uppercased method, path, millis, body hex, fetch id.
func TestDeviceSignaturePayloadVector(t *testing.T) {
	const emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := deviceBodySHA256Hex(nil); got != emptySHA {
		t.Errorf("sha(nil) = %q, want empty-string SHA-256 %q", got, emptySHA)
	}
	if got := deviceBodySHA256Hex([]byte("hello")); got != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Errorf(`sha("hello") = %q, want the well-known digest`, got)
	}
	got := deviceSignaturePayload("post", "/api/v1/freebuff/session/admission", 1727745600000, emptySHA, "fetch-1")
	want := "freebuff-device-v1\nPOST\n/api/v1/freebuff/session/admission\n1727745600000\n" + emptySHA + "\nfetch-1"
	if got != want {
		t.Errorf("payload = %q, want %q", got, want)
	}
	// No fetch id signs the empty slot (catalog GET before any fetch).
	if got := deviceSignaturePayload("GET", "/api/v1/freebuff/models", 1, emptySHA, ""); !strings.HasSuffix(got, "\n"+emptySHA+"\n") {
		t.Errorf("fetch-less payload = %q, want trailing empty fetch slot", got)
	}
}

// The unknown-key matcher is the vendor isFreebuffDeviceKeyUnknownError
// pair: device_key + unknown/not-found/invalid/unregistered, loosely.
func TestIsDeviceKeyUnknownError(t *testing.T) {
	for code, want := range map[string]bool{
		"device_key_unknown":               true,
		"DEVICE-KEY-NOT-FOUND":             true,
		"device_key_invalid":               true,
		"freebuff_device_key_unregistered": true,
		"freebuff_catalog_stale":           false,
		"purchase_claim_released":          false,
		"device_key":                       false,
		"unknown":                          false,
		"":                                 false,
	} {
		if got := isDeviceKeyUnknownError(code); got != want {
			t.Errorf("isDeviceKeyUnknownError(%q) = %v, want %v", code, got, want)
		}
	}
	if got := deviceErrorCodeOf(`{"error":"device_key_unknown"}`); got != "device_key_unknown" {
		t.Errorf("deviceErrorCodeOf = %q, want the string code", got)
	}
	for _, body := range []string{`{"error":{"code":1}}`, `{"status":"x"}`, `not json`, ``} {
		if got := deviceErrorCodeOf(body); got != "" {
			t.Errorf("deviceErrorCodeOf(%q) = %q, want empty", body, got)
		}
	}
}

// First contact: the catalog GET goes out unsigned (register:false until a
// catalog is held), then the admission registers once and signs — the
// signature verifying over the vendor payload shape with the registered
// public key. A second admission reuses the key id with no re-registration.
func TestDeviceRegistrationAndSignedAdmission(t *testing.T) {
	fake := &spoofFake{
		catalogSeq: []string{catalogTestBody("deepseek/deepseek-v4-flash", "fbm1.dev-1", "fetch-1")},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Cleanup(resetModelCatalogsForTest)
	t.Cleanup(resetDevicesForTest)

	client, err := New("tok-device-a", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := client.CreateSessionForModelWithClaim(ctx, "deepseek/deepseek-v4-flash", "cli:123e4567-e89b-42d3-a456-426614174000"); err != nil {
		t.Fatal(err)
	}
	if fake.catalogHits != 1 {
		t.Fatalf("catalog hits = %d, want 1", fake.catalogHits)
	}
	for _, name := range deviceHeaderNames {
		if got := fake.catalogHeaders[0].Get(name); got != "" {
			t.Errorf("catalog GET %s = %q, want absent (register:false until a catalog is held)", name, got)
		}
	}
	if got := fake.deviceHits(); got != 1 {
		t.Fatalf("device-keys hits = %d, want exactly 1 registration", got)
	}
	// The registration reports the raw public key as base64url + client cli.
	var reg struct {
		PublicKey string `json:"publicKey"`
		Client    string `json:"client"`
	}
	if err := json.Unmarshal([]byte(fake.deviceKeysBodies[0]), &reg); err != nil {
		t.Fatalf("registration body: %v", err)
	}
	if reg.Client != "cli" {
		t.Errorf("registration client = %q, want cli", reg.Client)
	}
	pubRaw, err := base64.RawURLEncoding.DecodeString(reg.PublicKey)
	if err != nil || len(pubRaw) != 32 {
		t.Fatalf("registration publicKey = %q, want base64url 32-byte raw key", reg.PublicKey)
	}
	verifyDeviceHeaders(t, fake.admitHeaders[0], ed25519.PublicKey(pubRaw),
		http.MethodPost, SessionAdmissionPath, "", "dk-1")

	// Second admission: same key id, no second registration.
	if _, err := client.CreateSessionForModelWithClaim(ctx, "deepseek/deepseek-v4-flash", "cli:123e4567-e89b-42d3-a456-426614174000"); err != nil {
		t.Fatal(err)
	}
	if got := fake.deviceHits(); got != 1 {
		t.Errorf("device-keys hits = %d after two admissions, want 1 (key id reused)", got)
	}
	verifyDeviceHeaders(t, fake.admitHeaders[1], ed25519.PublicKey(pubRaw),
		http.MethodPost, SessionAdmissionPath, "", "dk-1")
}

// A server predating device keys (404) leaves the account unsigned with a
// single registration attempt — the hour backoff means the retry does not
// re-attempt.
func TestDeviceUnsupportedServerStaysUnsigned(t *testing.T) {
	fake := &spoofFake{
		catalogSeq:   []string{catalogTestBody("deepseek/deepseek-v4-flash", "fbm1.dev-2", "fetch-1")},
		noDeviceKeys: true,
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Cleanup(resetModelCatalogsForTest)
	t.Cleanup(resetDevicesForTest)

	client, err := New("tok-device-b", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for range 2 {
		if _, err := client.CreateSessionForModelWithClaim(ctx, "deepseek/deepseek-v4-flash", "cli:123e4567-e89b-42d3-a456-426614174000"); err != nil {
			t.Fatal(err)
		}
	}
	if got := fake.deviceHits(); got != 1 {
		t.Errorf("device-keys hits = %d, want 1 (unsupported → hour backoff, no retry)", got)
	}
	for i, h := range fake.admitHeaders {
		for _, name := range deviceHeaderNames {
			if got := h.Get(name); got != "" {
				t.Errorf("admission %d %s = %q, want absent (registration unsupported → unsigned)", i, name, got)
			}
		}
	}
}

// Fallback mode (no catalog) sends no registration traffic at all.
func TestDeviceFallbackNeverRegisters(t *testing.T) {
	fake := &spoofFake{} // no catalogSeq: models path 404s → unsupported
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Cleanup(resetModelCatalogsForTest)
	t.Cleanup(resetDevicesForTest)

	client, err := New("tok-device-c", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateSessionForModelWithClaim(context.Background(), "deepseek/deepseek-v4-flash", "cli:123e4567-e89b-42d3-a456-426614174000"); err != nil {
		t.Fatal(err)
	}
	if got := fake.deviceHits(); got != 0 {
		t.Errorf("device-keys hits = %d in fallback mode, want 0 (no catalog → no key, no registration)", got)
	}
}

// An unknown-key refusal forgets the registration, so the next call
// registers again; any other error code keeps the registration.
func TestDeviceUnknownKeyRefusalReregisters(t *testing.T) {
	fake := &spoofFake{
		catalogSeq: []string{catalogTestBody("deepseek/deepseek-v4-flash", "fbm1.dev-3", "fetch-1")},
		admitSeq: []catalogAdmitResp{
			{status: http.StatusOK, body: `{"status":"active","instanceId":"inst-1","expiresAt":"2030-01-01T00:00:00Z"}`},
			{status: http.StatusForbidden, body: `{"error":"device_key_unknown"}`},
			{status: http.StatusOK, body: `{"status":"active","instanceId":"inst-1","expiresAt":"2030-01-01T00:00:00Z"}`},
		},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Cleanup(resetModelCatalogsForTest)
	t.Cleanup(resetDevicesForTest)

	client, err := New("tok-device-d", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	claim := "cli:123e4567-e89b-42d3-a456-426614174000"
	if _, err := client.CreateSessionForModelWithClaim(ctx, "deepseek/deepseek-v4-flash", claim); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateSessionForModelWithClaim(ctx, "deepseek/deepseek-v4-flash", claim); err == nil {
		t.Fatal("second admission: want the device_key_unknown refusal, got nil")
	}
	if _, err := client.CreateSessionForModelWithClaim(ctx, "deepseek/deepseek-v4-flash", claim); err != nil {
		t.Fatal(err)
	}
	if got := fake.deviceHits(); got != 2 {
		t.Fatalf("device-keys hits = %d, want 2 (forget + re-register)", got)
	}
	if got, want := fake.admitHeaders[0].Get(DeviceKeyHeader), "dk-1"; got != want {
		t.Errorf("first admission key = %q, want %q", got, want)
	}
	if got, want := fake.admitHeaders[2].Get(DeviceKeyHeader), "dk-2"; got != want {
		t.Errorf("post-refusal admission key = %q, want %q (re-registered)", got, want)
	}

	// A non-device error code never forgets.
	e := deviceEntryFor("tok-device-d", srv.URL)
	e.mu.Lock()
	keyBefore := e.keyID
	e.mu.Unlock()
	client.noteDeviceKeyErrorFromBody(`{"error":"purchase_claim_released"}`)
	e.mu.Lock()
	keyAfter := e.keyID
	e.mu.Unlock()
	if keyBefore == "" || keyAfter != keyBefore {
		t.Errorf("key id %q → %q on a foreign error code, want unchanged", keyBefore, keyAfter)
	}
}

// A fixed key signs deterministically: the same inputs always yield the same
// signature, and it verifies with the public key (the scheme is real
// Ed25519, not a header stamp).
func TestDeviceSeededSigningDeterministic(t *testing.T) {
	priv := ed25519.NewKeyFromSeed(fixedDeviceSeed())
	pub := priv.Public().(ed25519.PublicKey)
	first := signDeviceRequest(priv, "dk-9", "POST", "/api/v1/chat/completions", []byte(`{"model":"m"}`), "fetch-9", 1727745600000)
	second := signDeviceRequest(priv, "dk-9", "POST", "/api/v1/chat/completions", []byte(`{"model":"m"}`), "fetch-9", 1727745600000)
	if first[DeviceSignatureHeader] != second[DeviceSignatureHeader] {
		t.Fatal("same inputs signed differently")
	}
	payload := deviceSignaturePayload("POST", "/api/v1/chat/completions", 1727745600000, deviceBodySHA256Hex([]byte(`{"model":"m"}`)), "fetch-9")
	sig, err := base64.RawURLEncoding.DecodeString(first[DeviceSignatureHeader])
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(pub, []byte(payload), sig) {
		t.Fatalf("seeded signature does not verify over %q", payload)
	}
	if first[DeviceKeyHeader] != "dk-9" || first[DeviceTimestampHeader] != "1727745600000" {
		t.Errorf("headers = %v, want key id + timestamp", first)
	}
}
