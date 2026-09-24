// Chat request plumbing for the wire client: the request builder
// (newRequest), the retry loop (do) with its transient-failure detection,
// pinned TLS-fingerprint rotation and jittered backoff, the SSE stream
// plumbing (transparent decompression and cancel-aware response bodies),
// and the SDK-faithful client_id generator.
package upstream

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	cryptoRand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"freebucks-proxy/backend/internal/stealth"
	"freebucks-proxy/backend/internal/telemetry"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (c *Client) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("upstream: build %s %s: %w", method, path, err)
	}
	// A bodyless POST/PUT/PATCH is trivially replayable on a transient
	// retry: give it a NoBody GetBody so do()'s TRANSIENT_RETRIES replay
	// works (a nil GetBody silently disables retries, which after #120
	// would break the bodyless session POST's transport-level retry). GETs
	// and DELETEs stay nil-GetBody (never retried — idempotent reads fail
	// fast and the poll loop's own backoff owns them).
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		if body == nil {
			req.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
		}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if c.authOnly {
		// Token-less login-flow client (#62/#66): never send an empty
		// credential pair — the /api/auth/cli/* endpoints take the login
		// User-Agent instead (see authLoginRequest).
		req.Header.Del("Authorization")
	}
	// Content-Type only when a body is present (#120): the CLI sets it iff
	// body !== undefined (upstream/freebuff codebuff-api.ts:344-346), so a
	// bodyless session POST must not carry it. Chat always has a body.
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// UA scoping (newest-CLI wire behavior): the real CLI sends
	// the pinned llm-providers ai-sdk UA ONLY on chat; every other call
	// goes through plain Bun fetch, whose default UA is Bun/<version>
	// (.bun-version pins 1.3.14). newRequest therefore defaults to
	// bunUserAgent for all session/agent-runs/streak calls, and
	// ChatCompletions overrides with cliUserAgent. No browser headers on
	// any API path (#108/#109 fix option (a)): the utls ClientHello
	// impersonation stays, the browser header persona does not.
	// Agent-runs START/FINISH carry Authorization plus the optional
	// x-freebuff-acting-user-id, set by StartRun/FinishRun via
	// stampActingUser after newRequest (CLI parity: sdk/src/impl/
	// database.ts startAgentRun/finishAgentRun send Bearer plus the optional
	// acting-user header and no x-codebuff-api-key — that dual-auth pair
	// lives only on the agent-runtime's web-search/docs/gravity/token-count
	// POSTs, codebuff-web-api.ts callCodebuffV1/callTokenCountAPI).
	// newRequest itself never sets either extra header: the chat surface
	// stays Bearer-only (the pinned ai-sdk openai-compatible client's
	// Authorization is caller-supplied).
	req.Header.Set("User-Agent", bunUserAgent)
	// Proxy-identifying headers (X-Forwarded-*, Via, cloud headers, ...) are
	// stripped UNCONDITIONALLY — a relayed downstream header must never reach
	// upstream even on the plain Go transport with stealth off. The strip only
	// ever removes proxy signals; CLI-faithful headers set above are untouched.
	stealth.SanitizeHeaders(req.Header)
	ctx = req.Context()
	if profile := c.currentStealthProfile(); profile != nil {
		// Resolve the concrete profile ONCE per request and stash it: the
		// dialer reads the stash for the ClientHello, so the TLS fingerprint
		// matches the profile. Pinned profiles resolve to themselves;
		// auto/random get one concrete draw. The profile's browser headers
		// are deliberately NOT applied to upstream API calls (proxy-header
		// stripping already ran unconditionally above).
		connProf := stealth.GetProfileForConnection(profile)
		ctx = withStealthProfile(ctx, connProf)
	}
	if ctx != req.Context() {
		req = req.WithContext(ctx)
	}
	return req, nil
}

// do executes req, enforcing the given timeout unless ctx already carries an
// earlier deadline. The returned cancel must be released once the caller is
// done with the response BODY: canceling the request context aborts in-flight
// body reads, so it must outlive body streaming. cancel is nil when no
// timeout was applied. Failures are wrapped so errors.Is works both ways.
//
// When TRANSIENT_RETRIES > 0, transport-level failures (dial/TLS handshake/
// reset/EOF) are retried up to that many additional attempts: the body is
// replayed from GetBody on a fresh connection (req.Close), the pinned TLS
// fingerprint is rotated, and an exponential 1s*2^attempt +0-30% jitter
// backoff (cap 10s, the CLI control-path shape) precedes each
// retry. Classified upstream errors (429/403/401, session/run invalids,
// waiting room), any HTTP status >= 400, context cancellation, and requests
// whose body cannot be replayed are NEVER retried.
func (c *Client) do(req *http.Request, timeout time.Duration) (*http.Response, context.CancelFunc, error) {
	ctx := req.Context()
	start := time.Now()
	var cancel context.CancelFunc
	if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
		// The caller already bound the request. The control-call timeout is
		// still honored as an upper bound when it is the TIGHTER of the two:
		// a long caller deadline (e.g. a 15m request timeout) must not
		// silently defeat SessionCallTimeout on session/run control calls.
		if timeout > 0 {
			if remaining := time.Until(deadline); timeout < remaining {
				ctx, cancel = context.WithTimeout(ctx, timeout)
				req = req.WithContext(ctx)
			}
		}
	} else if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		req = req.WithContext(ctx)
	}

	// Capture the body so a transient failure can replay an identical
	// request. nil bodies (GETs) and non-replayable bodies never retry.
	var replayBody func() (io.ReadCloser, error)
	if req.GetBody != nil {
		replayBody = req.GetBody
	}
	// Wire correlation at debug: every upstream attempt below logs its
	// response ("upstream response"/"upstream ok"/"upstream error"), but
	// without a start line a death that never answers (hang until the
	// caller gives up) leaves no trace of which call was in flight.
	// Headers stay out — the token must never reach the logs.
	// req_id rides every do() line (""/absent only when the call was not
	// made through ChatCompletions with opts.RequestID, e.g. session/run
	// management): grep stays uniform across chat and control calls.
	// req.URL.Path (never URL.String) keeps query/secret material out.
	slog.Debug("upstream request", "method", req.Method, "path", req.URL.Path, "req_id", ReqID(ctx))

	for attempt := 1; ; attempt++ {
		resp, err := c.http.Do(req)
		if err == nil {
			if werr := wrapDecompress(resp); werr != nil {
				_ = resp.Body.Close()
				slog.Debug("upstream error", "method", req.Method, "path", req.URL.Path,
					"ms", time.Since(start).Milliseconds(), "class", errClassName(werr),
					"err", werr, "req_id", ReqID(ctx))
				if cancel != nil {
					cancel()
				}
				return nil, nil, fmt.Errorf("upstream: %s %s: %w", req.Method, req.URL.Path, werr)
			}
			if resp.StatusCode >= 400 {
				// Wire transparency: error responses are read (2KB cap),
				// logged as `upstream response` (redacted, ≤500 runes), and
				// classified ONCE here — the wrapper records the 428
				// waiting-room flag and the rate-limit ledger — then the typed
				// error is carried forward so callers never re-classify the
				// same body (issue #305).
				bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyRead))
				_ = resp.Body.Close()
				bodyText := telemetry.RedactSecrets(string(bodyBytes))
				classErr := c.classifyWithReqID(resp.StatusCode, bodyText, resp.Header, ReqID(ctx))
				class := errClassName(classErr)
				attrs := []any{
					"method", req.Method, "path", req.URL.Path,
					"status", resp.StatusCode, "ms", time.Since(start).Milliseconds(),
					"class", class,
					"body", truncateRunes(bodyText, 500),
					"req_id", ReqID(ctx),
				}
				slog.Debug("upstream response", attrs...)
				resp.Body = io.NopCloser(strings.NewReader(bodyText))
				// resp != nil with a non-nil classErr marks a classified
				// >=400 response (vs a transport failure, where resp is nil).
				return resp, cancel, classErr
			}
			slog.Debug("upstream ok", "method", req.Method, "path", req.URL.Path,
				"status", resp.StatusCode, "ms", time.Since(start).Milliseconds(),
				"req_id", ReqID(ctx))
			return resp, cancel, nil
		}

		// Transient transport failure with attempts remaining: rotate the
		// pinned fingerprint, replay the body on a fresh connection, and
		// retry after a jittered backoff.
		if c.transientRetriesLimit > 0 && attempt <= c.transientRetriesLimit &&
			ctx.Err() == nil && replayBody != nil && isTransient(err) {
			c.rotateStealthProfileForRetry(req)
			body, bodyErr := replayBody()
			if bodyErr != nil {
				slog.Debug("upstream retry aborted: body replay failed",
					"method", req.Method, "path", req.URL.Path,
					"token", c.tokenIndex+1, "attempt", attempt, "err", bodyErr,
					"req_id", ReqID(ctx))
			} else {
				// Count the retry only once the replay succeeded: the counter
				// reflects retries that actually fired, not aborted ones.
				c.transientRetries.Add(1)
				req.Body = body
				req.Close = true // fresh connection for the retry
				slog.Debug("upstream transient failure, retrying",
					"method", req.Method, "path", req.URL.Path,
					"token", c.tokenIndex+1, "attempt", attempt, "reason", err.Error(),
					"ms", time.Since(start).Milliseconds(), "req_id", ReqID(ctx))
				timer := time.NewTimer(c.retryDelay(attempt))
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
				}
				if ctx.Err() == nil {
					continue
				}
				// Context died during the backoff: a retry would fail
				// instantly, surface the context error instead.
				err = ctx.Err()
			}
		}

		// Transport failure (no response read, so no status): class names
		// the failure shape (generic UpstreamError for raw transport
		// errors); classified >=400s return via `upstream response` above.
		slog.Debug("upstream error", "method", req.Method, "path", req.URL.Path,
			"ms", time.Since(start).Milliseconds(), "class", errClassName(err),
			"err", err, "req_id", ReqID(ctx))
		if cancel != nil {
			cancel()
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, nil, context.Canceled
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, nil, fmt.Errorf("%w: %s %s", context.DeadlineExceeded, req.Method, req.URL.Path)
		}
		return nil, nil, fmt.Errorf("upstream: %s %s: %w", req.Method, req.URL.Path, err)
	}
}

// transientMarkers are transport-level failure signatures that are safe to
// retry: the request never reached the application layer, so no upstream
// quota/credits were burned and nothing was processed. Classified upstream
// errors (429/403/401, session/run invalids, waiting room) and any HTTP
// status >= 400 are handled at the response layer and never enter this path.
// Markers are lowercase: isTransient lowercases the wrapped error messages
// before matching. "tls: handshake failure" is Go's own alert string;
// "tls handshake failed" appears in wrapper libraries (e.g. stealth/uTLS).
var transientMarkers = []string{
	"tls handshake failed",
	"tls: handshake failure",
	"tls: internal error",
	"connection refused",
	"connection reset",
	"unexpected eof",
	"network is unreachable",
	"no route to host",
	"i/o timeout", // dial timeout
}

// isTransient reports whether err is a transient transport failure safe to
// retry. It walks the wrapped error chain and matches message fragments, so
// stealth-wrapped dial errors ("stealth: tcp dial failed: ...: connection
// refused") classify the same as the bare dial error.
//
// Bare "EOF" is matched on exact whole-message equality only: a substring
// match on "eof" would over-retry unrelated errors that merely mention the
// letters ("... eof marker ...").
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	for cur := err; cur != nil; cur = errors.Unwrap(cur) {
		msg := strings.ToLower(cur.Error())
		for _, marker := range transientMarkers {
			if strings.Contains(msg, marker) {
				return true
			}
		}
		if msg == "eof" {
			return true
		}
	}
	return false
}

// retryProfileRotation is the pinned-profile rotation order for transient
// retries: one entry per browser FAMILY, so a retry presents a genuinely
// different JA3 (chromium HelloChrome_120/133 -> safari custom -> firefox
// HelloFirefox_120). Profiles sharing one hello (chrome126/edge126,
// firefox120/firefox128) sit in the same entry. ProfileRandom/ProfileAuto
// are excluded: they already resolve a fresh fingerprint per connection.
var retryProfileRotation = []struct {
	ids  []stealth.ProfileID
	next *stealth.Profile
}{
	{ids: []stealth.ProfileID{stealth.ProfileIDChrome120, stealth.ProfileIDChrome126, stealth.ProfileIDEdge126}, next: stealth.ProfileSafari18},
	{ids: []stealth.ProfileID{stealth.ProfileIDSafari17, stealth.ProfileIDSafari18}, next: stealth.ProfileFirefox128},
	{ids: []stealth.ProfileID{stealth.ProfileIDFirefox120, stealth.ProfileIDFirefox128}, next: stealth.ProfileChrome126},
}

// rotateStealthProfileForRetry swaps the pinned TLS fingerprint to a
// different profile before a retry so the retried connection does not repeat
// the fingerprint that just failed. The request keeps its CLI headers —
// only proxy-identifying headers are re-stripped; no browser persona is
// applied on API paths (#109). random/auto already rotate per connection and
// are left alone. No-op when retries are disabled or no fingerprint is
// pinned.
//
// The rotation is deliberately cross-family (chromium -> safari -> firefox):
// a same-stack retry would be more CLI-faithful (Bun retries on one stack),
// but repeating the exact JA3 that just failed is the worse bet against a
// WAF that flagged it, so the retry presents a genuinely different hello.
// No byte-exact Bun/BoringSSL emulation is attempted: the CLI's exact TLS
// bytes are not derivable from its source (BoringSSL build, GREASE seeds,
// extension order), so utls presets approximate the family, honestly labeled
// in stealth/profiles.go.
func (c *Client) rotateStealthProfileForRetry(req *http.Request) {
	c.profileMu.Lock()
	defer c.profileMu.Unlock()
	if c.transientRetriesLimit <= 0 || c.stealthProfile == nil {
		return
	}
	id := c.stealthProfile.ID
	if id == stealth.ProfileIDRandom || id == stealth.ProfileIDAuto {
		return
	}
	next := nextStealthProfile(c.stealthProfile)
	if next.ID == id {
		return
	}
	c.stealthProfile = next
	c.fingerprintRotations.Add(1)
	stealth.SanitizeHeaders(req.Header)
}

// nextStealthProfile returns the profile to rotate to after cur: the next
// entry in the fixed rotation order whose ClientHelloID differs from cur's.
func nextStealthProfile(cur *stealth.Profile) *stealth.Profile {
	for _, entry := range retryProfileRotation {
		for _, id := range entry.ids {
			if id == cur.ID {
				return entry.next
			}
		}
	}
	return retryProfileRotation[0].next
}

// retryDelay returns the sleep before transient retry number attempt
// (1-based: the first retry is attempt 1): exponential backoff
// 1s*2^(attempt-1) plus 0-30% jitter, capped at 10s — the CLI's
// calculateBackoffDelay shape (codebuff-api.ts: initialDelayMs 1000,
// maxDelayMs 10000, jitter 0.3*exponential). Tests pin it via
// Client.retryBackoff, which overrides the computed delay wholesale.
func (c *Client) retryDelay(attempt int) time.Duration {
	if c.retryBackoff != nil {
		return c.retryBackoff()
	}
	idx := attempt - 1
	if idx < 0 {
		idx = 0
	}
	if idx > 10 {
		idx = 10
	}
	base := time.Second << uint(idx)
	var b [8]byte
	_, _ = cryptoRand.Read(b[:])
	u := binary.BigEndian.Uint64(b[:])
	jitter := time.Duration(u % uint64(int64(base)*3/10+1))
	d := base + jitter
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	return d
}

// wrapDecompress replaces resp.Body with a transparent decompressing reader
// when the upstream compresses the response (gzip/deflate only — stdlib).
// newRequest intentionally sends no browser Accept-Encoding (CLI fidelity),
// so live upstream wire is Go-default gzip handled here; an uninvited
// br/zstd/lz4 errors as unsupported Content-Encoding, same as before.
func wrapDecompress(resp *http.Response) error {
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if enc == "" || enc == "identity" {
		return nil
	}
	underlying := resp.Body
	switch enc {
	case "gzip":
		zr, err := gzip.NewReader(underlying)
		if err != nil {
			return fmt.Errorf("gzip: %w", err)
		}
		resp.Body = &decompressCloser{Reader: zr, underlying: underlying}
	case "deflate":
		// RFC 9110 §8.4.1.3 defines Content-Encoding: deflate as a
		// zlib-wrapped stream (RFC 1950), but some servers historically
		// send raw DEFLATE (RFC 1951). Sniff the zlib header (CMF/FLG:
		// CM=8, CINFO<=7, 16-bit header a multiple of 31) WITHOUT
		// consuming bytes — a consumed header would corrupt the raw
		// fallback — and decode accordingly. (the raw-only
		// reader broke mid-stream on conforming zlib responses.)
		br := bufio.NewReader(underlying)
		head, _ := br.Peek(2)
		if len(head) == 2 && head[0]&0x0f == 8 && head[0]>>4 <= 7 &&
			(uint16(head[0])<<8|uint16(head[1]))%31 == 0 {
			zr, err := zlib.NewReader(br)
			if err != nil {
				return fmt.Errorf("deflate: %w", err)
			}
			resp.Body = &decompressCloser{Reader: zr, underlying: underlying}
		} else {
			resp.Body = &decompressCloser{Reader: flate.NewReader(br), underlying: underlying}
		}
	default:
		return fmt.Errorf("unsupported Content-Encoding %q", enc)
	}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	return nil
}

// decompressCloser bridges a decompressing reader back to the underlying
// response body so Close always reaches the socket. The stdlib decoders
// (gzip/zlib/flate) need no per-response cleanup beyond that.
type decompressCloser struct {
	io.Reader
	underlying io.ReadCloser
}

func (d *decompressCloser) Close() error {
	return d.underlying.Close()
}

// releaseCancel cancels a do() timeout context unless it is nil.
func releaseCancel(cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}
}

// cancelBody closes the underlying body and then releases the request
// context, so a streamed response body lives exactly as long as its reader.
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	releaseCancel(b.cancel)
	return err
}

// NewClientID mints one SDK-faithful client id for a run. The CLI draws it
// once per PROMPT (run.ts:722 promptId, passed as clientSessionId at run.ts:822
// and reused by every LLM step of that run), so the run manager mints it with
// the run and every chat call of that run repeats it.
func NewClientID() string { return generateClientID() }

// generateClientID mints the SDK-faithful 13-char base36 client id
// (Math.random().toString(36).substring(2, 15)).
func generateClientID() string {
	var b [16]byte
	if _, err := cryptoRand.Read(b[:]); err != nil {
		// crypto/rand failure is unrecoverable in practice; fall back to a
		// time-seeded value rather than panicking mid-request. UnixNano in
		// base36 is only 12 digits today, so pad to the SDK's 13-char length
		// (the old [:13] slice panicked on short values).
		return padBase36(strconv.FormatInt(time.Now().UnixNano(), 36))
	}
	n := new(big.Int).SetBytes(b[:])
	mod := new(big.Int).Exp(big.NewInt(36), big.NewInt(13), nil)
	return padBase36(n.Mod(n, mod).Text(36))
}

// padBase36 left-pads a base36 string with '0' to the SDK-faithful 13-char
// client id length. Both the crypto/rand draw and the time-seeded fallback
// need it: the latter is 12 digits, which would otherwise come out shorter
// than the JS substring(2, 15) equivalent.
func padBase36(id string) string {
	for len(id) < 13 {
		id = "0" + id
	}
	return id
}
