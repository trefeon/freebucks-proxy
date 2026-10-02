// Catalog-mode admission regression tests (vendor
// cli/src/utils/freebuff-session-api.ts callFreebuffSession +
// cli/src/utils/freebuff-model-catalog.ts controller): the proxy sends the
// catalog HANDLE (not the raw id) with the protocol/fetch headers, refetches
// once on 409 freebuff_catalog_stale and retries the SAME claim with the new
// handle exactly once, and passes the raw id through untouched when no
// catalog is held. Mock-upstream only; never live traffic.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// catalogAdmitFake serves the catalog GET and the admission POST from
// scripted sequences, recording every admission's headers. An empty catalog
// body answers 404 (a server predating catalogs); admission answers past
// the end of admitSeq repeat the last one.
type catalogAdmitFake struct {
	mu             sync.Mutex
	catalogSeq     []string
	admitSeq       []catalogAdmitResp
	catalogHits    int
	catalogHeaders []http.Header
	admitHeaders   []http.Header
	posts          int
}

type catalogAdmitResp struct {
	status int
	body   string
}

func (f *catalogAdmitFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, SessionAdmissionPath):
		f.mu.Lock()
		f.posts++
		f.admitHeaders = append(f.admitHeaders, r.Header.Clone())
		resp := catalogAdmitResp{status: http.StatusOK, body: `{"status":"active","instanceId":"inst-1","expiresAt":"2030-01-01T00:00:00Z"}`}
		if len(f.admitSeq) > 0 {
			resp = f.admitSeq[min(f.posts-1, len(f.admitSeq)-1)]
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.body))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}
}

func (f *catalogAdmitFake) counts() (catalogHits, posts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.catalogHits, f.posts
}

// catalogTestBody renders a protocol-1 catalog holding one row, listable
// now with a refreshAt an hour out (no proactive refetch mid-test).
func catalogTestBody(key, handle, fetchID string) string {
	now := time.Now().UnixMilli()
	return fmt.Sprintf(`{"protocol":1,"version":"v1","issuedAt":%d,"refreshAt":%d,`+
		`"rows":[{"key":%q,"handle":%q}],"plansUrl":"https://example.test/plans","fetchId":%q}`,
		now, now+3600_000, key, handle, fetchID)
}

var cliClaimShape = regexp.MustCompile(`(?i)^cli:[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// assertCatalogClaimShape pins the claim contract on EVERY admission POST:
// a cli:<uuid> (session_claim.go mint-once identity), never rotated by the
// catalog path.
func assertCatalogClaimShape(t *testing.T, h http.Header) {
	t.Helper()
	got := h.Get("x-freebuff-instance-id")
	if !cliClaimShape.MatchString(got) {
		t.Errorf("x-freebuff-instance-id = %q, want cli:<RFC4122-v4 UUID>", got)
	}
	if got == "" {
		t.Errorf("x-freebuff-desktop-attempt-id absent: claim suffix must ride the attempt header")
		return
	}
	uuid := strings.TrimPrefix(got, "cli:")
	if attempt := h.Get("x-freebuff-desktop-attempt-id"); attempt != uuid {
		t.Errorf("x-freebuff-desktop-attempt-id = %q, want claim suffix %q", attempt, uuid)
	}
}

// Case A: handle+protocol byte-exact. The held catalog maps the requested
// id to its handle; the POST carries the handle plus the protocol version
// and the fetch id. The device trio stays absent here because this fake has
// no device-keys endpoint (registration 404s → unsupported → unsigned).
func TestCatalogAdmissionHandleAndProtocol(t *testing.T) {
	fake := &catalogAdmitFake{
		catalogSeq: []string{catalogTestBody("deepseek/deepseek-v4-flash", "fbm1.test-handle-1", "fetch-1")},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Cleanup(resetModelCatalogsForTest)

	client, err := New("tok-catalog-a", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.CreateSessionForModelWithClaim(context.Background(), "deepseek/deepseek-v4-flash", "cli:123e4567-e89b-42d3-a456-426614174000")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "active" {
		t.Fatalf("status = %q, want active", st.Status)
	}
	hits, posts := fake.counts()
	if hits != 1 || posts != 1 {
		t.Fatalf("catalog hits = %d, admission posts = %d; want 1 and 1", hits, posts)
	}
	h := fake.admitHeaders[0]
	if got := h.Get("x-freebuff-model"); got != "fbm1.test-handle-1" {
		t.Errorf("x-freebuff-model = %q, want catalog handle fbm1.test-handle-1", got)
	}
	if got := h.Get("x-freebuff-catalog-protocol"); got != "1" {
		t.Errorf("x-freebuff-catalog-protocol = %q, want 1", got)
	}
	if got := h.Get("x-freebuff-catalog-fetch"); got != "fetch-1" {
		t.Errorf("x-freebuff-catalog-fetch = %q, want fetch-1", got)
	}
	// No device-keys endpoint on this fake: the registration is refused as
	// unsupported, so the admission goes out unsigned (vendor-supported
	// degradation); see deviceHeaderNames.
	for _, name := range deviceHeaderNames {
		if got := h.Get(name); got != "" {
			t.Errorf("%s = %q, want absent (no device-keys endpoint on this fake: registration unsupported → unsigned)", name, got)
		}
	}
	assertCatalogClaimShape(t, h)
	if got := h.Get("x-freebuff-instance-id"); got != "cli:123e4567-e89b-42d3-a456-426614174000" {
		t.Errorf("claim = %q, want the caller-held claim carried verbatim", got)
	}
}

// Case B: stale → refetch → same-claim retry once, then success. The first
// POST's handle is refused as stale; the retry carries the SAME claim under
// the fresh handle with the fresh fetch id.
func TestCatalogAdmissionStaleRefetchSameClaimRetry(t *testing.T) {
	fake := &catalogAdmitFake{
		catalogSeq: []string{
			catalogTestBody("deepseek/deepseek-v4-flash", "fbm1.old-handle", "fetch-A"),
			catalogTestBody("deepseek/deepseek-v4-flash", "fbm1.new-handle", "fetch-B"),
		},
		admitSeq: []catalogAdmitResp{
			{status: http.StatusConflict, body: `{"error":"freebuff_catalog_stale","message":"handle rotated"}`},
			{status: http.StatusOK, body: `{"status":"active","instanceId":"inst-1","expiresAt":"2030-01-01T00:00:00Z"}`},
		},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Cleanup(resetModelCatalogsForTest)

	client, err := New("tok-catalog-b", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.CreateSessionForModelWithClaim(context.Background(), "deepseek/deepseek-v4-flash", "cli:123e4567-e89b-42d3-a456-426614174000")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "active" {
		t.Fatalf("status = %q, want active after the same-claim retry", st.Status)
	}
	hits, posts := fake.counts()
	if hits != 2 || posts != 2 {
		t.Fatalf("catalog hits = %d, admission posts = %d; want exactly 2 and 2 (one refetch, one retry)", hits, posts)
	}
	first, second := fake.admitHeaders[0], fake.admitHeaders[1]
	if got := first.Get("x-freebuff-model"); got != "fbm1.old-handle" {
		t.Errorf("first x-freebuff-model = %q, want fbm1.old-handle", got)
	}
	if got := second.Get("x-freebuff-model"); got != "fbm1.new-handle" {
		t.Errorf("retry x-freebuff-model = %q, want fbm1.new-handle", got)
	}
	if got := second.Get("x-freebuff-catalog-fetch"); got != "fetch-B" {
		t.Errorf("retry x-freebuff-catalog-fetch = %q, want fetch-B", got)
	}
	// The claim is NOT rotated by the stale path: both POSTs rejoin on it.
	if first.Get("x-freebuff-instance-id") != "cli:123e4567-e89b-42d3-a456-426614174000" ||
		second.Get("x-freebuff-instance-id") != "cli:123e4567-e89b-42d3-a456-426614174000" {
		t.Errorf("claim rotated across the stale retry: %q vs %q",
			first.Get("x-freebuff-instance-id"), second.Get("x-freebuff-instance-id"))
	}
	assertCatalogClaimShape(t, first)
	assertCatalogClaimShape(t, second)
}

// Case B2: a second stale refusal surfaces (no infinite loop). The retry's
// handle is refused again — the error returns to the caller with exactly two
// POSTs and two fetches behind it.
func TestCatalogAdmissionStaleSecondRefusalSurfaces(t *testing.T) {
	fake := &catalogAdmitFake{
		catalogSeq: []string{
			catalogTestBody("deepseek/deepseek-v4-flash", "fbm1.old-handle", "fetch-A"),
			catalogTestBody("deepseek/deepseek-v4-flash", "fbm1.new-handle", "fetch-B"),
		},
		admitSeq: []catalogAdmitResp{
			{status: http.StatusConflict, body: `{"error":"freebuff_catalog_stale"}`},
			{status: http.StatusConflict, body: `{"error":"freebuff_catalog_stale"}`},
		},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Cleanup(resetModelCatalogsForTest)

	client, err := New("tok-catalog-b2", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CreateSessionForModelWithClaim(context.Background(), "deepseek/deepseek-v4-flash", "cli:123e4567-e89b-42d3-a456-426614174000")
	if !errors.Is(err, ErrCatalogStale) {
		t.Fatalf("err = %v, want ErrCatalogStale surfaced (no loop, no fallback error)", err)
	}
	hits, posts := fake.counts()
	if hits != 2 || posts != 2 {
		t.Fatalf("catalog hits = %d, admission posts = %d; want exactly 2 and 2", hits, posts)
	}
	for _, h := range fake.admitHeaders {
		assertCatalogClaimShape(t, h)
	}
}

// Case C: nil-catalog fallback passthrough. A server predating catalogs
// (models path 404) leaves today's wire shape byte-identical: raw id, no
// protocol/fetch/device headers.
func TestCatalogAdmissionFallbackPassthrough(t *testing.T) {
	fake := &catalogAdmitFake{} // no catalogSeq: models path 404s → unsupported
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Cleanup(resetModelCatalogsForTest)

	client, err := New("tok-catalog-c", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.CreateSessionForModelWithClaim(context.Background(), "deepseek/deepseek-v4-flash", "cli:123e4567-e89b-42d3-a456-426614174000")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "active" {
		t.Fatalf("status = %q, want active", st.Status)
	}
	h := fake.admitHeaders[0]
	if got := h.Get("x-freebuff-model"); got != "deepseek/deepseek-v4-flash" {
		t.Errorf("x-freebuff-model = %q, want raw id passthrough in fallback mode", got)
	}
	for _, name := range []string{
		"x-freebuff-catalog-protocol",
		"x-freebuff-catalog-fetch",
		"x-freebuff-device-key",
		"x-freebuff-device-ts",
		"x-freebuff-device-sig",
	} {
		if got := h.Get(name); got != "" {
			t.Errorf("%s = %q, want absent in fallback mode", name, got)
		}
	}
	assertCatalogClaimShape(t, h)
}

// Fallback-mode stale surfaces without a retry: with no held catalog there
// is nothing to refetch, mirroring the vendor's `if (!catalog) throw`.
func TestCatalogAdmissionFallbackStaleSurfaces(t *testing.T) {
	fake := &catalogAdmitFake{
		admitSeq: []catalogAdmitResp{
			{status: http.StatusConflict, body: `{"error":"freebuff_catalog_stale"}`},
		},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Cleanup(resetModelCatalogsForTest)

	client, err := New("tok-catalog-c2", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CreateSessionForModelWithClaim(context.Background(), "deepseek/deepseek-v4-flash", "cli:123e4567-e89b-42d3-a456-426614174000")
	if !errors.Is(err, ErrCatalogStale) {
		t.Fatalf("err = %v, want ErrCatalogStale", err)
	}
	if _, posts := fake.counts(); posts != 1 {
		t.Fatalf("admission posts = %d; want exactly 1 (no catalog to refetch, no retry)", posts)
	}
}

// Handle lookup prefers the exact key, then the legacy digest — so a row
// that names only digests still maps the proxy's legacy model id.
func TestCatalogHandleForLegacyDigest(t *testing.T) {
	const model = "deepseek/deepseek-v4-flash"
	cat := &modelCatalog{
		Rows: []modelCatalogRow{
			{Key: "other/row", Handle: "fbm1.other"},
			{Key: "deepseek/deepseek-v4-flash", Handle: "fbm1.key-match"},
			{Handle: "fbm1.digest-match", LegacyDigests: []string{freebuffLegacyModelDigest(model)}},
		},
	}
	if got := catalogHandleFor(cat, model); got != "fbm1.key-match" {
		t.Errorf("exact key = %q, want fbm1.key-match", got)
	}
	digestOnly := &modelCatalog{
		Rows: []modelCatalogRow{
			{Handle: "fbm1.digest-match", LegacyDigests: []string{freebuffLegacyModelDigest(model)}},
		},
	}
	if got := catalogHandleFor(digestOnly, model); got != "fbm1.digest-match" {
		t.Errorf("legacy digest = %q, want fbm1.digest-match", got)
	}
	if got := catalogHandleFor(digestOnly, "unknown/model"); got != "" {
		t.Errorf("unknown id = %q, want empty (caller sends the raw id)", got)
	}
	if got := catalogHandleFor(nil, model); got != "" {
		t.Errorf("nil catalog = %q, want empty (fallback passthrough)", got)
	}
}

// The legacy digest is a cross-runtime contract with the vendor's
// freebuffLegacyModelDigest (FNV-1a over "freebuff-legacy-model:<id>", two
// 32-bit lanes, lowercase hex). Pinned against the JS implementation's
// output so a drift breaks loudly instead of silently unmapping rows.
func TestFreebuffLegacyModelDigestVector(t *testing.T) {
	// Vectors produced by the vendor implementation itself (bun-evaled from
	// upstream/freebuff common/src/types/freebuff-model-catalog.ts).
	for model, want := range map[string]string{
		"deepseek/deepseek-v4-flash": "1e303ac563a6f9cc",
		"thudm/glm-5.2":              "34b28d37c464c21c",
		"mimo/mimo-v3-flash":         "c08a7e04acd9a22f",
	} {
		if got := freebuffLegacyModelDigest(model); got != want {
			t.Errorf("digest(%q) = %q, want vendor vector %q", model, got, want)
		}
	}
}
