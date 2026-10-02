// Catalog-mode admission (vendor cli/src/utils/freebuff-session-api.ts
// callFreebuffSession/requestFreebuffSession + cli/src/utils/
// freebuff-model-catalog.ts fetch controller + common/src/types/
// freebuff-model-catalog.ts protocol).
//
// The server-driven model catalog maps the proxy's legacy model ids to
// per-account handles (fbm1.…): a catalog-mode admission sends the HANDLE
// as x-freebuff-model plus the protocol/fetch headers, and answers a 409
// freebuff_catalog_stale by refetching once and retrying the SAME claim
// with the new handle. Nil catalog = fallback mode (today's behavior:
// the raw model id, no protocol headers).
//
// In-memory only, per (token, upstream host), never persisted: handles are
// minted per account and rotate, so none of this is ever written to disk
// (vendor freebuff-catalog-store.ts).
//
// Device signing is deliberately ABSENT (see the residual-gap note on
// deviceHeadersAbsent): the proxy holds bearer tokens, not an install-bound
// Ed25519 device key, and minting/registering one per proxied account would
// be an account-mutating side effect with no user gesture. The vendor
// tolerates unsigned requests (freebuffDeviceHeaders returns {} with no
// signer), so handle+protocol without device headers is a supported
// degradation, not a second convention.
package upstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// CatalogProtocolHeader tells the session endpoints to answer with
	// catalog keys instead of model ids (vendor
	// FREEBUFF_CATALOG_PROTOCOL_HEADER). Sent on every catalog-mode
	// session call.
	CatalogProtocolHeader = "x-freebuff-catalog-protocol"
	// CatalogProtocolVersion is the only protocol this client speaks
	// (vendor FREEBUFF_CATALOG_PROTOCOL_VERSION).
	CatalogProtocolVersion = "1"
	// CatalogFetchHeader ties a request to the fetch (account, install,
	// time) that produced its handle (vendor FREEBUFF_CATALOG_FETCH_HEADER).
	CatalogFetchHeader = "x-freebuff-catalog-fetch"
	// CatalogClientHeader names the app fetching the catalog so the server
	// lists the CLI's rows (vendor FREEBUFF_CATALOG_CLIENT_CLI).
	CatalogClientHeader = "x-freebuff-client"
	// CatalogClientCLI is the client name the proxy fetches as: it speaks
	// the CLI's admission shape, so it asks for the CLI's rows.
	CatalogClientCLI = "cli"
	// ModelCatalogPath is the catalog endpoint (vendor
	// FREEBUFF_MODEL_CATALOG_PATH).
	ModelCatalogPath = "/api/v1/freebuff/models"
	// CatalogStaleCode is the error code the server answers a stale,
	// expired or unknown handle with (vendor FREEBUFF_CATALOG_STALE_ERROR).
	// A client that sees it refetches the catalog once and retries with
	// the new handle for the same key.
	CatalogStaleCode = "freebuff_catalog_stale"
	// ModelHandlePrefix is the prefix every handle carries, telling a
	// handle from a legacy model id without parsing it (vendor
	// FREEBUFF_MODEL_HANDLE_PREFIX).
	ModelHandlePrefix = "fbm1."
	// FreebuffModelHeader names the admission model slot (vendor
	// FREEBUFF_MODEL_HEADER): the handle in catalog mode, the raw id in
	// fallback.
	FreebuffModelHeader = "x-freebuff-model"
)

// Catalog fetch cadence, mirroring the vendor controller
// (cli/src/utils/freebuff-model-catalog.ts): fetch timeout 10s, the first
// admission waits at most 4s for the first catalog (past it the session
// starts in fallback mode), refreshAt delays clamp to [30s, 6h], a second
// stale answer within 5s of a refetch means the fresh handle is stale too
// (never loop). Two proxy-side choices where the vendor has UI signals the
// proxy lacks: an unsupported catalog (server predates catalogs) rechecks
// after 1h (the vendor never re-polls until a tier/viewer change, which the
// proxy cannot observe), and a failed fetch keeps the current mode and
// retries after 60s (vendor RETRY_BASE_DELAY_MS).
const (
	catalogFetchTimeout    = 10 * time.Second
	catalogInitialWait     = 4 * time.Second
	catalogMinRefreshDelay = 30 * time.Second
	catalogMaxRefreshDelay = 6 * time.Hour
	catalogStaleCooldown   = 5 * time.Second
	catalogUnsupportedWait = time.Hour
	catalogErrorRetryWait  = time.Minute
	// catalogBodyLimit bounds the catalog GET body: a catalog is rows of
	// small JSON, never megabytes.
	catalogBodyLimit = 2 << 20
)

// modelCatalogRow is the wire row the proxy reads: the key it maps from
// and the handle it sends. Every other row field (badges, pricing,
// availability) is picker UI the proxy never draws.
type modelCatalogRow struct {
	Key           string   `json:"key"`
	Handle        string   `json:"handle"`
	OpensAt       *int64   `json:"opensAt"`
	LegacyDigests []string `json:"legacyDigests"`
}

// modelCatalog is the held catalog: the rows plus the fetch id every
// catalog-mode request sends back and the refreshAt schedule.
type modelCatalog struct {
	RefreshAt int64             `json:"refreshAt"`
	IssuedAt  int64             `json:"issuedAt"`
	Rows      []modelCatalogRow `json:"rows"`
	FetchID   string            `json:"fetchId"`
}

// parseModelCatalog validates a catalog body: protocol 1 with at least one
// listable row, mirroring parseFreebuffModelCatalog (null when it is not
// one this client speaks) plus the fetch rule that a catalog with no rows
// it may list leaves fallback mode in place. ok=false is ALWAYS fallback,
// never an error.
func parseModelCatalog(body []byte, nowMs int64) (cat *modelCatalog, ok bool) {
	// The wire nests the protocol beside the catalog fields; decode both.
	var wire struct {
		Protocol  int               `json:"protocol"`
		RefreshAt int64             `json:"refreshAt"`
		IssuedAt  int64             `json:"issuedAt"`
		Rows      []modelCatalogRow `json:"rows"`
		FetchID   string            `json:"fetchId"`
	}
	if err := json.Unmarshal(body, &wire); err != nil || wire.Protocol != 1 {
		return nil, false
	}
	at := wire.IssuedAt
	if at == 0 {
		at = nowMs
	}
	rows := make([]modelCatalogRow, 0, len(wire.Rows))
	for _, row := range wire.Rows {
		if row.OpensAt != nil && *row.OpensAt > at {
			continue
		}
		if row.Key == "" || row.Handle == "" {
			continue
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, false
	}
	return &modelCatalog{
		RefreshAt: wire.RefreshAt,
		IssuedAt:  wire.IssuedAt,
		Rows:      rows,
		FetchID:   wire.FetchID,
	}, true
}

// freebuffLegacyModelDigest is the FNV-1a digest of a legacy (pre-catalog)
// model id, matched against a row's legacyDigests so the catalog need not
// list model ids (vendor freebuffLegacyModelDigest: over the namespaced
// string, one 32-bit lane per multiplier). Model ids are ASCII, so byte
// iteration matches the vendor's UTF-16 unit iteration exactly.
func freebuffLegacyModelDigest(modelID string) string {
	input := "freebuff-legacy-model:" + modelID
	h1 := uint32(0x811c9dc5)
	h2 := uint32(0x01000193 ^ 0x5bd1e995)
	for i := range len(input) {
		c := uint32(input[i])
		h1 = (h1 ^ c) * 0x01000193
		h2 = (h2 ^ c) * 0x5bd1e995
	}
	return fmt.Sprintf("%08x%08x", h1, h2)
}

// catalogHandleFor returns the handle a model id is sent as under the held
// catalog: the exact key match first, then the legacy-digest match (vendor
// freebuffCatalogHandleFor + findFreebuffCatalogRowForLegacyId). Empty when
// the catalog names no row for the id — the caller sends the raw id and the
// server answers stale, which refetches the catalog.
func catalogHandleFor(cat *modelCatalog, modelID string) string {
	if cat == nil || modelID == "" {
		return ""
	}
	for _, row := range cat.Rows {
		if row.Key == modelID {
			return row.Handle
		}
	}
	digest := freebuffLegacyModelDigest(modelID)
	for _, row := range cat.Rows {
		for _, d := range row.LegacyDigests {
			if d == digest {
				return row.Handle
			}
		}
	}
	return ""
}

// --- per-account holder ----------------------------------------------------

// catalogEntry is one account's held fetch: nil catalog IS fallback mode
// (vendor `catalog !== null` is catalog mode). nextFetchAt carries the
// refreshAt schedule (or the unsupported/error backoff); lastStaleRefresh
// is the stale-loop guard.
type catalogEntry struct {
	catalog          *modelCatalog
	fetched          bool
	nextFetchAt      time.Time
	lastStaleRefresh time.Time
	inflight         *catalogInflight
}

type catalogInflight struct {
	done chan struct{}
	// ok reports whether the flight fetched kind==ok. Set before done
	// closes, so joiners read it race-free after <-done.
	ok bool
}

var (
	catalogMu      sync.Mutex
	catalogEntries = map[string]*catalogEntry{}
)

// catalogAccountKey names one account on one API host (vendor
// freebuff-device-signing accountFor: one registration per account and API
// host). Hashed so the registry never retains a second copy of the token.
func catalogAccountKey(token, baseURL string) string {
	sum := sha256.Sum256([]byte(token + "\x00" + baseURL))
	return hex.EncodeToString(sum[:])
}

// resetModelCatalogsForTest drops all held fetches. Tests only.
func resetModelCatalogsForTest() {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	catalogEntries = map[string]*catalogEntry{}
}

// catalogFetchKind mirrors FreebuffCatalogFetchResult: ok holds a catalog,
// unsupported and error both mean fallback (with different recheck waits).
type catalogFetchKind int

const (
	catalogFetchOK catalogFetchKind = iota
	catalogFetchUnsupported
	catalogFetchError
)

// fetchModelCatalog GETs the catalog for one account: Authorization plus
// the protocol/client headers (vendor fetchFreebuffModelCatalog, minus the
// device signature — see the file note). Exactly one upstream hit, bounded
// by timeout, never retried here.
func (c *Client) fetchModelCatalog(ctx context.Context, timeout time.Duration) (*modelCatalog, catalogFetchKind) {
	if c.http == nil {
		return nil, catalogFetchError
	}
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := c.newRequest(fetchCtx, http.MethodGet, ModelCatalogPath, nil)
	if err != nil {
		return nil, catalogFetchError
	}
	req.Header.Set(CatalogProtocolHeader, CatalogProtocolVersion)
	req.Header.Set(CatalogClientHeader, CatalogClientCLI)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, catalogFetchError
	}
	defer func() { _ = resp.Body.Close() }()
	// Vendor: 408/429/5xx are errors; any other non-ok is unsupported
	// (a server predating catalogs).
	if resp.StatusCode == http.StatusRequestTimeout ||
		resp.StatusCode == http.StatusTooManyRequests ||
		resp.StatusCode >= 500 {
		return nil, catalogFetchError
	}
	if resp.StatusCode/100 != 2 {
		return nil, catalogFetchUnsupported
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, catalogBodyLimit))
	if err != nil {
		return nil, catalogFetchError
	}
	cat, ok := parseModelCatalog(raw, time.Now().UnixMilli())
	if !ok {
		return nil, catalogFetchUnsupported
	}
	return cat, catalogFetchOK
}

// storeCatalogFetch applies a fetch answer to the entry, returning the kind
// for the stale path (which retries only on ok-with-catalog). An error
// keeps the current mode (vendor: "keeping the current mode"); unsupported
// drops to fallback with a slow recheck.
func storeCatalogFetch(e *catalogEntry, cat *modelCatalog, kind catalogFetchKind, now time.Time) {
	switch kind {
	case catalogFetchOK:
		e.catalog = cat
		delay := time.Duration(cat.RefreshAt-now.UnixMilli()) * time.Millisecond
		if delay < catalogMinRefreshDelay {
			delay = catalogMinRefreshDelay
		}
		if delay > catalogMaxRefreshDelay {
			delay = catalogMaxRefreshDelay
		}
		e.nextFetchAt = now.Add(delay)
	case catalogFetchUnsupported:
		e.catalog = nil
		e.nextFetchAt = now.Add(catalogUnsupportedWait)
	default:
		if !e.fetched {
			e.catalog = nil
		}
		e.nextFetchAt = now.Add(catalogErrorRetryWait)
	}
	e.fetched = true
}

// joinCatalogInflight waits for the in-flight fetch owned by another
// admission. False when ctx dies first (the caller falls back to whatever
// is held — today: nil on first boot).
func joinCatalogInflight(ctx context.Context, in *catalogInflight) bool {
	select {
	case <-ctx.Done():
		return false
	case <-in.done:
		return true
	}
}

// ensureModelCatalog returns the catalog held for this client's account,
// fetching on first admission and on the refreshAt schedule. Concurrent
// admissions join one flight. Never an error: nil is fallback mode.
func (c *Client) ensureModelCatalog(ctx context.Context) *modelCatalog {
	key := catalogAccountKey(c.token, c.baseURL)
	catalogMu.Lock()
	e, ok := catalogEntries[key]
	if !ok {
		e = &catalogEntry{}
		catalogEntries[key] = e
	}
	if in := e.inflight; in != nil {
		catalogMu.Unlock()
		if !joinCatalogInflight(ctx, in) {
			return nil
		}
		catalogMu.Lock()
		cat := e.catalog
		catalogMu.Unlock()
		return cat
	}
	now := time.Now()
	if e.fetched && now.Before(e.nextFetchAt) {
		cat := e.catalog
		catalogMu.Unlock()
		return cat
	}
	in := &catalogInflight{done: make(chan struct{})}
	e.inflight = in
	first := !e.fetched
	catalogMu.Unlock()

	timeout := catalogFetchTimeout
	if first {
		// The first session request waits bounded for the first catalog;
		// past it the session starts in fallback mode (vendor
		// FREEBUFF_CATALOG_INITIAL_WAIT_MS).
		timeout = catalogInitialWait
	}
	cat, kind := c.fetchModelCatalog(ctx, timeout)

	catalogMu.Lock()
	storeCatalogFetch(e, cat, kind, time.Now())
	in.ok = kind == catalogFetchOK
	e.inflight = nil
	close(in.done)
	held := e.catalog
	catalogMu.Unlock()
	return held
}

// refreshModelCatalogAfterStale refetches after the server refused a handle
// as stale (vendor refreshAfterStale): at most one refetch per cooldown
// window, joined by concurrent admissions. It reports the fresh catalog and
// whether the fetch answered ok — a retry goes out ONLY on a fresh catalog
// (retrying the same stale handle would only loop).
func (c *Client) refreshModelCatalogAfterStale(ctx context.Context) (*modelCatalog, bool) {
	key := catalogAccountKey(c.token, c.baseURL)
	catalogMu.Lock()
	e, ok := catalogEntries[key]
	if !ok {
		e = &catalogEntry{}
		catalogEntries[key] = e
	}
	if in := e.inflight; in != nil {
		catalogMu.Unlock()
		if !joinCatalogInflight(ctx, in) {
			return nil, false
		}
		catalogMu.Lock()
		cat := e.catalog
		fresh := in.ok && cat != nil
		catalogMu.Unlock()
		return cat, fresh
	}
	now := time.Now()
	if now.Sub(e.lastStaleRefresh) < catalogStaleCooldown {
		catalogMu.Unlock()
		return nil, false
	}
	e.lastStaleRefresh = now
	in := &catalogInflight{done: make(chan struct{})}
	e.inflight = in
	catalogMu.Unlock()

	cat, kind := c.fetchModelCatalog(ctx, catalogFetchTimeout)

	catalogMu.Lock()
	storeCatalogFetch(e, cat, kind, time.Now())
	in.ok = kind == catalogFetchOK
	e.inflight = nil
	close(in.done)
	held := e.catalog
	fresh := kind == catalogFetchOK && held != nil
	catalogMu.Unlock()
	if !fresh {
		slog.Debug("upstream: catalog stale refresh did not yield a catalog; surfacing the refusal")
	}
	return held, fresh
}

// stampCatalogModel sets the admission model slot from the held catalog:
// the handle when the catalog names a row for the id, the raw id otherwise
// (the server answers that stale, which refetches — vendor
// requestFreebuffSession). The protocol headers ride every catalog-mode
// call, with the fetch id when held. Nil catalog sends today's exact shape.
func stampCatalogModel(req *http.Request, model string, cat *modelCatalog) {
	value := model
	if cat != nil {
		if h := catalogHandleFor(cat, model); h != "" {
			value = h
		}
		req.Header.Set(CatalogProtocolHeader, CatalogProtocolVersion)
		if cat.FetchID != "" {
			req.Header.Set(CatalogFetchHeader, cat.FetchID)
		}
	}
	if model != "" {
		req.Header.Set(FreebuffModelHeader, value)
	}
}

// isCatalogStaleResult reports the stale-handle refusal in either shape
// the parse can surface: the typed classify error for the {"error": …}
// body, or a status-carrying row when the server puts the code in status.
func isCatalogStaleResult(st *SessionState, err error) bool {
	if err != nil {
		return strings.Contains(strings.ToLower(err.Error()), CatalogStaleCode)
	}
	return st != nil && st.Status == CatalogStaleCode
}

// deviceHeadersAbsent documents the residual gap the proxy ships with: the
// vendor signs every catalog-mode session call with the install's Ed25519
// device key (x-freebuff-device-key / -ts / -sig over method, path,
// timestamp, empty-body SHA-256 and fetchId — vendor
// common/src/util/freebuff-device-signing.ts signFreebuffDeviceRequest +
// common/src/types/freebuff-model-catalog.ts freebuffDeviceSignaturePayload).
// The signing input IS portable to Go (crypto/ed25519 + sha256 stdlib), but
// the KEY is not: a client generates one pair per install, stores the
// private key owner-only on disk, and registers the public key once per
// account via POST /api/v1/freebuff/device-keys. The proxy is a
// multi-account server holding bearer tokens — it has no install identity
// to bind a key to, no stable home for the private key (in-memory-only
// rule would re-register every boot: key churn on live user accounts), and
// no user gesture authorizing an account-mutating registration. So
// catalog-mode admissions carry handle+protocol WITHOUT the device trio,
// exactly like a CLI whose signer is unavailable (freebuffDeviceHeaders
// returns {} with no signer — a vendor-supported degradation). If the
// server ever hard-requires device signatures, catalog admissions will
// fail loudly (not silently downgrade): watch for device_key error codes
// in admission refusals.
var deviceHeadersAbsent = [3]string{
	"x-freebuff-device-key",
	"x-freebuff-device-ts",
	"x-freebuff-device-sig",
}
