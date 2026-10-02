// Device-bound request signing (vendor cli/src/utils/freebuff-device-key.ts +
// common/src/util/freebuff-device-signing.ts + common/src/types/
// freebuff-model-catalog.ts freebuffDeviceSignaturePayload).
//
// The vendor generates one Ed25519 key pair per install, stores the private
// key owner-only on disk (device-key.json), registers the public key once per
// account via POST FREEBUFF_DEVICE_KEYS_PATH, and signs every catalog,
// session and completions request. A request without a signer goes out
// UNSIGNED (freebuffDeviceHeaders returns {} — a vendor-supported
// degradation), and a server error code naming an unknown device key makes
// the client forget the registration so the next request registers again.
//
// Custody choice: the proxy is a multi-account server holding bearer tokens,
// not an install with a stable home — so each keypair lives in process
// memory only, keyed per (token, upstream host) like the held catalog
// (handles rotate; keys are install-bound). A process restart mints a fresh
// keypair and re-registers it on the next catalog-mode call; the orphaned
// server-side key id from the previous boot is abandoned, exactly like a CLI
// reinstall. Nothing is ever written to disk.
//
// Registration IS automatic (no user gesture exists to invent): the vendor
// CLI itself registers with zero user interaction (register defaults true —
// only a client that has never seen the server speak the catalog holds back
// with register:false). The proxy mirrors that: the first catalog GET goes
// out unsigned, and every later catalog-mode call signs, registering first
// when needed. A refused or failed registration backs off (5m; an endpoint
// the server predates — 404/405 — backs off 1h), so a pre-catalog server
// sees no registration traffic at all.
package upstream

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DeviceKeysPath is the per-account device-key registration route
	// (vendor FREEBUFF_DEVICE_KEYS_PATH).
	DeviceKeysPath = "/api/v1/freebuff/device-keys"
	// DeviceKeyHeader names the registered key id (vendor
	// FREEBUFF_DEVICE_KEY_HEADER).
	DeviceKeyHeader = "x-freebuff-device-key"
	// DeviceTimestampHeader carries the signature timestamp in millis
	// (vendor FREEBUFF_DEVICE_TIMESTAMP_HEADER).
	DeviceTimestampHeader = "x-freebuff-device-ts"
	// DeviceSignatureHeader carries the base64url Ed25519 signature (vendor
	// FREEBUFF_DEVICE_SIGNATURE_HEADER).
	DeviceSignatureHeader = "x-freebuff-device-sig"
	// deviceClientName is the client name registration reports (vendor
	// FreebuffDeviceClient 'cli': the proxy speaks the CLI's shape).
	deviceClientName = "cli"
	// deviceSignatureDomain is the first line of every signed payload
	// (vendor freebuffDeviceSignaturePayload).
	deviceSignatureDomain = "freebuff-device-v1"
	// deviceRegisterTimeout bounds one registration POST (vendor
	// REGISTER_TIMEOUT_MS).
	deviceRegisterTimeout = 10 * time.Second
	// deviceFirstWait bounds how long a request waits for an in-flight
	// registration owned by another call (vendor DEFAULT_WAIT_MS: the work
	// carries on behind a request that stopped waiting, so the next one is
	// signed).
	deviceFirstWait = 3 * time.Second
	// deviceRegisterRetryWait gates registration retries after a refused or
	// failed attempt (vendor REGISTER_RETRY_MS).
	deviceRegisterRetryWait = 5 * time.Minute
	// deviceUnsupportedRetryWait gates registration retries when the server
	// has no device-key endpoint (vendor REGISTER_UNSUPPORTED_RETRY_MS).
	deviceUnsupportedRetryWait = time.Hour
)

// deviceSignaturePayload renders the exact signed string (vendor
// freebuffDeviceSignaturePayload): newline-joined domain, uppercased method,
// path without query, timestamp millis, lowercase hex SHA-256 of the exact
// body bytes as sent (of the empty string for a body-less request), and the
// fetch id (empty when none).
func deviceSignaturePayload(method, path string, timestampMs int64, bodySHA256Hex, fetchID string) string {
	return strings.Join([]string{
		deviceSignatureDomain,
		strings.ToUpper(method),
		path,
		strconv.FormatInt(timestampMs, 10),
		bodySHA256Hex,
		fetchID,
	}, "\n")
}

// deviceBodySHA256Hex is the lowercase hex SHA-256 of the body bytes as sent
// (vendor freebuffBodySha256; the empty string hashes as itself, so a nil
// body and an empty body sign identically).
func deviceBodySHA256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// deviceBase64URL encodes raw key/signature bytes (vendor base64UrlEncode:
// base64url, no padding).
func deviceBase64URL(raw []byte) string {
	return base64.RawURLEncoding.EncodeToString(raw)
}

// deviceRequestPath extracts the signed path (URL path without query) from
// an absolute request URL. It never fails open to a wrong path: an
// unparseable URL signs the full string, which the server will refuse rather
// than accept under a mismatched path.
func deviceRequestPath(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		return u.Path
	}
	if i := strings.IndexByte(rawURL, '?'); i >= 0 {
		return rawURL[:i]
	}
	return rawURL
}

var (
	deviceKeyCodeRe   = regexp.MustCompile(`(?i)device[_-]?key`)
	deviceUnknownCode = regexp.MustCompile(`(?i)unknown|not[_-]?found|invalid|unregistered`)
)

// isDeviceKeyUnknownError reports whether a server error code says the device
// key a request named is not one it knows (vendor
// isFreebuffDeviceKeyUnknownError, matched loosely on the code alone).
func isDeviceKeyUnknownError(code string) bool {
	return code != "" && deviceKeyCodeRe.MatchString(code) && deviceUnknownCode.MatchString(code)
}

// deviceErrorCodeOf extracts the {"error": "<code>"} string from a response
// body, "" when the body carries no string error code.
func deviceErrorCodeOf(body string) string {
	var wire struct {
		Error any `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		return ""
	}
	if code, ok := wire.Error.(string); ok {
		return code
	}
	return ""
}

// --- per-account signer ----------------------------------------------------

// deviceEntry is one account's in-memory device identity: the keypair plus
// the server's key id for the registration scope. Zero keyID means
// unsigned (never registered, forgotten after an unknown-key refusal, or
// backing off).
type deviceEntry struct {
	mu         sync.Mutex
	priv       ed25519.PrivateKey
	hasKey     bool
	keyID      string
	retryAfter time.Time
	inflight   *deviceInflight
}

// deviceInflight lets concurrent calls join one registration POST instead of
// each firing their own (vendor FreebuffDeviceSigner.registering).
type deviceInflight struct {
	done chan struct{}
}

var (
	deviceMu      sync.Mutex
	deviceEntries = map[string]*deviceEntry{}
)

// resetDevicesForTest drops all held device identities. Tests only.
func resetDevicesForTest() {
	deviceMu.Lock()
	defer deviceMu.Unlock()
	deviceEntries = map[string]*deviceEntry{}
}

// deviceEntryFor returns the entry for one account on one API host,
// creating it empty (no keypair yet — a client that has never seen the
// server speak the catalog writes no key). Keys are account names, not
// tokens: the map key hashes token+host exactly like the catalog holder.
func deviceEntryFor(token, baseURL string) *deviceEntry {
	key := catalogAccountKey(token, baseURL)
	deviceMu.Lock()
	defer deviceMu.Unlock()
	e, ok := deviceEntries[key]
	if !ok {
		e = &deviceEntry{}
		deviceEntries[key] = e
	}
	return e
}

// seedDeviceForTest installs a fixed keypair+registration for byte-exact
// tests. Tests only.
func seedDeviceForTest(token, baseURL string, priv ed25519.PrivateKey, keyID string) {
	e := deviceEntryFor(token, baseURL)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.priv = priv
	e.hasKey = priv != nil
	e.keyID = keyID
	e.retryAfter = time.Time{}
	e.inflight = nil
}

// signDeviceRequest renders the three device headers for one request,
// signed with priv (vendor signFreebuffDeviceRequest).
func signDeviceRequest(priv ed25519.PrivateKey, keyID, method, path string, body []byte, fetchID string, timestampMs int64) map[string]string {
	payload := deviceSignaturePayload(method, path, timestampMs, deviceBodySHA256Hex(body), fetchID)
	sig := ed25519.Sign(priv, []byte(payload))
	return map[string]string{
		DeviceKeyHeader:       keyID,
		DeviceTimestampHeader: strconv.FormatInt(timestampMs, 10),
		DeviceSignatureHeader: deviceBase64URL(sig),
	}
}

// deviceHeaders returns the device trio for one request, or nil (send it
// unsigned — the vendor-supported degradation). With register=false it
// signs only with an already-known key id and never generates or registers,
// so a server that predates the protocol sees no registration traffic at
// all.
func (c *Client) deviceHeaders(ctx context.Context, method, rawURL string, body []byte, fetchID string, register bool) map[string]string {
	if c.http == nil {
		return nil
	}
	e := deviceEntryFor(c.token, c.baseURL)

	path := deviceRequestPath(rawURL)
	now := time.Now()

	e.mu.Lock()
	if e.keyID != "" && e.hasKey {
		headers := signDeviceRequest(e.priv, e.keyID, method, path, body, fetchID, now.UnixMilli())
		e.mu.Unlock()
		return headers
	}
	if !register {
		e.mu.Unlock()
		return nil
	}
	if !e.hasKey {
		_, priv, err := ed25519.GenerateKey(nil)
		if err != nil || len(priv) == 0 {
			e.mu.Unlock()
			return nil
		}
		e.priv = priv
		e.hasKey = true
	}
	if now.Before(e.retryAfter) {
		e.mu.Unlock()
		return nil
	}
	if in := e.inflight; in != nil {
		e.mu.Unlock()
		return c.joinDeviceInflight(ctx, e, in, method, path, body, fetchID)
	}
	in := &deviceInflight{done: make(chan struct{})}
	e.inflight = in
	priv := e.priv
	e.mu.Unlock()

	keyID, unsupported := c.registerDeviceKey(ctx, deviceBase64URL(priv.Public().(ed25519.PublicKey)))

	e.mu.Lock()
	if keyID != "" {
		e.keyID = keyID
	} else if unsupported {
		e.retryAfter = time.Now().Add(deviceUnsupportedRetryWait)
	} else {
		e.retryAfter = time.Now().Add(deviceRegisterRetryWait)
	}
	e.inflight = nil
	close(in.done)
	if e.keyID == "" {
		e.mu.Unlock()
		return nil
	}
	headers := signDeviceRequest(e.priv, e.keyID, method, path, body, fetchID, time.Now().UnixMilli())
	e.mu.Unlock()
	return headers
}

// joinDeviceInflight waits (bounded, context-aware) for another call's
// registration, then signs with the outcome — or sends unsigned when the
// wait loses (vendor withTimeout: the work carries on behind).
func (c *Client) joinDeviceInflight(ctx context.Context, e *deviceEntry, in *deviceInflight, method, path string, body []byte, fetchID string) map[string]string {
	timer := time.NewTimer(deviceFirstWait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil
	case <-timer.C:
		return nil
	case <-in.done:
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.keyID == "" || !e.hasKey {
		return nil
	}
	return signDeviceRequest(e.priv, e.keyID, method, path, body, fetchID, time.Now().UnixMilli())
}

// registerDeviceKey POSTs the public key for this account and returns the
// server's key id. The second return reports an endpoint the server predates
// (404/405 → hour backoff); any other failure retries in minutes. A missing
// keyId in an ok answer is a failure, never a registration.
func (c *Client) registerDeviceKey(ctx context.Context, publicKey string) (keyID string, unsupported bool) {
	if c.http == nil {
		return "", false
	}
	regCtx, cancel := context.WithTimeout(ctx, deviceRegisterTimeout)
	defer cancel()
	payload, _ := json.Marshal(map[string]any{
		"publicKey": publicKey,
		"client":    deviceClientName,
	})
	req, err := c.newRequest(regCtx, http.MethodPost, DeviceKeysPath, payload)
	if err != nil {
		return "", false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 256))
		return "", true
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil || resp.StatusCode/100 != 2 {
		return "", false
	}
	// The answer shape is checked loosely (vendor: typeof body?.keyId
	// === 'string'): anything else is a failed registration.
	var answered struct {
		KeyID any `json:"keyId"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&answered); err != nil {
		return "", false
	}
	id, ok := answered.KeyID.(string)
	if !ok || id == "" {
		return "", false
	}
	return id, false
}

// noteDeviceKeyErrorFromBody forgets this account's registration when a
// response error code says the server does not know the device key the
// request named (vendor noteFreebuffDeviceKeyError): the keypair is kept and
// the next request registers it again. Anything else is a no-op.
func (c *Client) noteDeviceKeyErrorFromBody(body string) {
	if !isDeviceKeyUnknownError(deviceErrorCodeOf(body)) {
		return
	}
	key := catalogAccountKey(c.token, c.baseURL)
	deviceMu.Lock()
	e, ok := deviceEntries[key]
	deviceMu.Unlock()
	if !ok {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.keyID = ""
	e.retryAfter = time.Time{}
}
