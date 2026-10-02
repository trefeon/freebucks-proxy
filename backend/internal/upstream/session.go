package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"freebuff-proxy/backend/internal/modelcat"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Session wire vocabulary shared with the upstream CLI and desktop clients
// (vendor af898dc, common/src/constants/freebuff-models.ts): the dedicated
// admission/reuse routes and their headers. Dedicated routes fail closed on
// servers predating these guarantees — never fall back to the legacy path.
const (
	sessionInstanceIDHeader         = "x-freebuff-instance-id"
	sessionMultiSessionHeader       = "x-freebuff-multi-session"
	sessionPurchaseContinuityHeader = "x-freebuff-purchase-continuity"
	sessionDesktopAttemptIDHeader   = "x-freebuff-desktop-attempt-id"
	// SessionAdmissionPath is the dedicated session-create route.
	SessionAdmissionPath = "/api/v1/freebuff/session/admission"
	// AgentRunsPath is the agent-run lifecycle route (START/FINISH). Like
	// admission, its POSTs are never transport-retried: disposition is
	// unknown after a network failure, and a replayed START mints a
	// duplicate run (the free_mode_run_fanout signal) while a replayed
	// FINISH double-reports steps. The run lifecycle owns its own
	// recovery (fresh run + replay at the engine level).
	AgentRunsPath = "/api/v1/agent-runs"
	// SessionReusePath reuses an exact live single-session instance without
	// buying or taking over. No proxy caller needs it yet (the CLI does
	// not call it either); pinned so every client agrees on the string.
	SessionReusePath = "/api/v1/freebuff/session/reuse"
	// ReuseInstanceHeader names the exact live instance to reuse.
	ReuseInstanceHeader = "x-freebuff-reuse-instance-id"
	// Takeover (vendor tip 57943aa71,
	// common/src/constants/freebuff-models.ts:3481-3482): the vendor sends
	// x-freebuff-takeover-instance-id on the admission POST only when taking
	// over — Desktop's explicit "Use it here", naming the single-slot holder
	// the rejection just identified ("end that tab's session and give me the
	// slot"). The server honors it only when it still matches that holder, so
	// a stale click can never end an unseen tab. The proxy NEVER sends it:
	// there is no user-confirmed holder here, and the superseded path is
	// terminal by design (auto-takeover risks ping-pong — see classify.go's
	// 409 session_superseded handling): the cached row drops and the NEXT
	// request re-joins fresh without naming a holder. Wiring it into
	// automatic recovery would fabricate a user gesture the operator never
	// made.
	// WalletSpendLimitHeader carries the per-request wallet spend cap on
	// admission. The proxy holds no user-confirmed limit, so it always
	// sends the server default, exactly like a CLI POST with no explicit
	// model pick (callFreebuffSession sends String(walletSpendLimit ?? 0)).
	WalletSpendLimitHeader = "x-freebuff-wallet-spend-limit"
	// DefaultWalletSpendLimit is the unset spend limit the proxy sends.
	DefaultWalletSpendLimit = "0"
	// FreebucksTimezoneHeader carries the declared IANA timezone on every
	// session call — admission POST, poll GET, probe GET, and the DELETE/refund
	// (vendor 3420c99, common/src/util/freebucks-timezone.ts
	// FREEBUCKS_TIMEZONE_HEADER; vendor cli/src/utils/freebuff-session-api.ts
	// spreads freebucksTimeZoneHeaders() into all of them): the server picks
	// the account reset zone for daily.resetAt from it. The value comes from
	// SetLocalityResolver when installed (the session-locality rule), else the
	// host zone (localIANATimezone). A timezone is a scheduling preference,
	// never proof of country or access.
	FreebucksTimezoneHeader = "x-fb-timezone"
	// FirstTabDiscountHeader folds the first-tab offer into the quoted
	// prices (vendor 3420c99,
	// common/src/util/freebuff-first-tab-discount.ts
	// FIRST_TAB_DISCOUNT_HEADER). callFreebuffSession stamps it on EVERY
	// session call — POST, GET and DELETE alike, "1"|"0"
	// (cli/src/utils/freebuff-session-api.ts:163-167) — so sessionCall and
	// EndSession send the boring value "0": the proxy holds no first-tab
	// discount, exactly like a CLI call with firstTabDiscount unset.
	FirstTabDiscountHeader = "x-freebuff-first-tab-discount"
	// SessionUnsupportedMessage is the verbatim fail-closed copy for
	// servers predating the admission route
	// (FREEBUFF_SESSION_UNSUPPORTED_MESSAGE).
	SessionUnsupportedMessage = "This server cannot safely start or resume your session yet. Reload or update Freebuff and try again shortly. No purchase was made."
)

// sessionAttemptSuffix identifies CLI-minted attempt IDs. The suffix is the
// UUID sent as x-freebuff-desktop-attempt-id.
func sessionAttemptSuffix(instanceID string) (string, bool) {
	if !strings.HasPrefix(instanceID, cliInstanceIDPrefix) {
		return "", false
	}
	attemptID := instanceID[len(cliInstanceIDPrefix):]
	if attemptID == "" {
		return "", false
	}
	return attemptID, true
}

// admission route (suffix match: the client base URL may carry a prefix).
func isSessionAdmissionRequest(req *http.Request) bool {
	return req != nil && req.URL != nil && strings.HasSuffix(req.URL.Path, SessionAdmissionPath)
}

// isAgentRunsRequest reports a START/FINISH lifecycle POST (suffix match
// like admission): never transport-retried, same unknown-disposition rule.
func isAgentRunsRequest(req *http.Request) bool {
	return req != nil && req.URL != nil && req.Method == http.MethodPost &&
		strings.HasSuffix(req.URL.Path, AgentRunsPath)
}

// SessionRefundReceipt is the parsed session-DELETE receipt (vendor
// af898dc): the release confirmation plus the early-end refund. Refund
// carries the settled freebucksRefund (including zero); nil when the server
// sent none. Pending mirrors freebucksRefundPending: final usage is still
// outstanding and the DELETE must be replayed with the same instance id.
type SessionRefundReceipt struct {
	Status  string
	Refund  *float64
	Pending bool
}

// CreateSession POSTs the admission route with no body.
func (c *Client) CreateSession(ctx context.Context) (*SessionState, error) {
	return c.CreateSessionForModel(ctx, "")
}

// CreateSessionForModel POSTs the dedicated admission route with the
// requested model and wallet limit, minting a fresh attempt identity.
// It is CreateSessionForModelWithClaim with an empty claim; see it for the
// full wire contract.
func (c *Client) CreateSessionForModel(ctx context.Context, model string) (*SessionState, error) {
	return c.CreateSessionForModelWithClaim(ctx, model, "")
}

// CreateSessionForModelWithClaim POSTs the dedicated admission route with
// the requested model and wallet limit, carrying the caller-held claimID
// as x-freebuff-instance-id instead of minting a fresh cli:<uuid>. Catalog
// mode (catalog.go, vendor callFreebuffSession): the held per-account
// catalog maps the model id to its handle, the POST carries the protocol
// headers, and a stale handle refetches once with a same-claim retry. The
// session manager owns the persisted per-token claim (persist/rotate/
// retry); the wire only carries it. Ordinary models send the claim with
// the multi-session attempt headers when it is cli:-prefixed (its suffix
// as x-freebuff-desktop-attempt-id, mirroring freebuffSessionMetadata);
// a non-cli: claim (e.g. a legacy hour) rides as a bare instance id with
// no attempt headers. An empty claimID falls back to a fresh mint, and
// TierOffer models retain the upstream legacy single-session identity
// regardless of claim. The POST carries NO body and therefore no
// Content-Type. It is NEVER retried at the transport level (vendor tip
// 57943aa71, cli/src/utils/freebuff-session-api.ts:57-74): a POST network
// failure leaves disposition unknown, and it never carries
// x-freebuff-takeover-instance-id (see the Takeover note above — no
// user-confirmed holder, never auto-takeover).
func (c *Client) CreateSessionForModelWithClaim(ctx context.Context, model, claimID string) (*SessionState, error) {
	if c.mock != nil {
		return c.mock.CreateSession(c.token, model)
	}
	attemptMode := !modelcat.HasTier(model, modelcat.TierOffer)
	instanceID := strings.TrimSpace(claimID)
	attemptID := ""
	if attemptMode {
		if instanceID == "" {
			var err error
			instanceID, err = generateCliInstanceID()
			if err != nil {
				return nil, fmt.Errorf("generate Freebuff session attempt ID: %w", err)
			}
		}
		attemptID, _ = sessionAttemptSuffix(instanceID)
	}
	// Catalog mode (vendor callFreebuffSession): the held catalog maps the
	// requested id to its handle. Nil catalog is fallback — today's exact
	// shape (raw id, no protocol headers).
	held := c.ensureModelCatalog(ctx)
	post := func(cat *modelCatalog) (*SessionState, error) {
		req, err := c.newRequest(ctx, http.MethodPost, SessionAdmissionPath, nil)
		if err != nil {
			return nil, err
		}
		stampCatalogModel(req, model, cat)
		// The per-request wallet spend cap (vendor callFreebuffSession sends
		// String(walletSpendLimit ?? 0)): the operator-set standing consent,
		// headless default "0" (WALLET_SPEND_LIMIT).
		req.Header.Set(WalletSpendLimitHeader, c.walletSpend())
		if attemptMode && instanceID != "" {
			req.Header.Set(sessionInstanceIDHeader, instanceID)
			if attemptID != "" {
				req.Header.Set(sessionMultiSessionHeader, "1")
				req.Header.Set(sessionPurchaseContinuityHeader, "1")
				req.Header.Set(sessionDesktopAttemptIDHeader, attemptID)
			}
		}
		return c.sessionCall(req)
	}
	backfill := func(st *SessionState) {
		if attemptMode && st != nil && st.InstanceID == "" && st.Status == "active" {
			st.InstanceID = instanceID
		}
	}
	st, err := post(held)
	if !isCatalogStaleResult(st, err) {
		backfill(st)
		return st, err
	}
	// Stale loop (vendor callFreebuffSession + refreshAfterStale): refetch
	// once and retry the SAME claim with the new handle, exactly once. A
	// second refusal surfaces as-is — no infinite loop, and the claim is
	// NEVER rotated here (rotation stays on the DELETE/superseded paths).
	// Without a held catalog there is nothing to refetch, so a fallback
	// stale surfaces like the vendor's fallback mode does.
	if held == nil || ctx.Err() != nil {
		return nil, staleAdmissionError(st, err)
	}
	slog.Debug("upstream: catalog handle stale, refetching once and retrying same claim")
	fresh, ok := c.refreshModelCatalogAfterStale(ctx)
	if !ok {
		return nil, staleAdmissionError(st, err)
	}
	st2, err2 := post(fresh)
	if isCatalogStaleResult(st2, err2) {
		return nil, staleAdmissionError(st2, err2)
	}
	backfill(st2)
	return st2, err2
}

// staleAdmissionError normalizes a stale-handle refusal to the typed error,
// whichever shape the parse surfaced (classify error or status row).
func staleAdmissionError(st *SessionState, err error) error {
	if err != nil {
		return err
	}
	body := ""
	if st != nil {
		body = st.WireBody
	}
	return &CatalogStaleError{Status: http.StatusConflict, Body: truncate(body, 200)}
}

// GetSession polls /api/v1/freebuff/session for the given instance. A poll
// 404 maps to Status "ended" (the session vanished upstream; the session
// manager re-creates it). An admission-route create never maps 404 to
// "disabled" — it fails closed with ErrSessionAdmissionUnsupported.
func (c *Client) GetSession(ctx context.Context, instanceID string) (*SessionState, error) {
	return c.GetSessionWithOpts(ctx, instanceID, false)
}

// GetSessionWithOpts polls /api/v1/freebuff/session with an optional compact
// response header. CLI attempt IDs add multi-session, purchase-continuity,
// and heartbeat headers; noncompact attempt polls also request unused rate
// limits. Compact polls retain the compact header but omit that request.
// Legacy IDs keep the base route and do not send attempt-only headers. The
// locality header rides every session call (sessionCall), the poll included.
func (c *Client) GetSessionWithOpts(ctx context.Context, instanceID string, compact bool) (*SessionState, error) {
	if c.mock != nil {
		return c.mock.GetSession(c.token, "")
	}
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/freebuff/session", nil)
	if err != nil {
		return nil, err
	}
	if instanceID != "" {
		req.Header.Set(sessionInstanceIDHeader, instanceID)
	}
	if compact {
		req.Header.Set("x-freebuff-compact-session", "1")
	}
	if _, attemptMode := sessionAttemptSuffix(instanceID); attemptMode {
		req.Header.Set(sessionMultiSessionHeader, "1")
		req.Header.Set(sessionPurchaseContinuityHeader, "1")
		req.Header.Set("x-freebuff-heartbeat", "1")
		if !compact {
			req.Header.Set("x-freebuff-include-unused-rate-limits", "1")
		}
	}
	return c.sessionCall(req)
}

// pollActiveInterval is the GET cadence inside PollUntilActive: fast enough
// to confirm a fresh admission inside the budget, slow enough not to hammer
// the session route (the CLI's steady-state poll cadence is 30s;
// use-freebuff-session.ts POLL_INTERVAL_ACTIVE_MS — this is the short
// post-admission confirm, not the steady state).
const pollActiveInterval = time.Second

// defaultPollActiveBudget bounds PollUntilActive when the caller passes no
// (or a non-positive) budget: long enough for admission to settle, short
// enough to fail a wedged turn while the client is still listening.
const defaultPollActiveBudget = 30 * time.Second

// PollUntilActive GET-polls the session until it reads active or budget
// runs out (CLI parity: the vendor turn is admit → GET-poll-until-active →
// agent-runs START → N×chat on the same run_id → FINISH; the poll loop
// lives in use-freebuff-session.ts, terminal states stop it via
// nextDelayMs). It exists for the post-admission confirm before the first
// START: the server slice calls it after CreateSessionForModel and only
// starts the run on an active row.
//
// An active row returns immediately with a nil error. Any other settled
// (non-queued) status returns immediately too — a queued row is the only
// shape that can still become active without a fresh admission, so only
// queued (and unparsed noise that never decoded into a state) keeps
// polling. Context cancellation aborts at once; budget exhaustion returns
// the last observed state (nil when no poll ever answered) alongside an
// error wrapping context.DeadlineExceeded.
func (c *Client) PollUntilActive(ctx context.Context, instanceID string, budget time.Duration) (*SessionState, error) {
	if budget <= 0 {
		budget = defaultPollActiveBudget
	}
	deadline := time.Now().Add(budget)
	var last *SessionState
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			status := ""
			if last != nil {
				status = last.Status
			}
			return last, fmt.Errorf("upstream: session %s not active after %s (last status %q): %w", instanceID, budget, status, context.DeadlineExceeded)
		}
		pollCtx, cancel := context.WithTimeout(ctx, remaining)
		st, err := c.GetSession(pollCtx, instanceID)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// Unparsed noise (transport failure, 5xx without a session
			// body): a parsed state would have returned above with a nil
			// error, so there is nothing terminal to honor — keep polling
			// while budget remains.
		} else {
			last = st
			if st.Status == "active" {
				return st, nil
			}
			if st.Status != "queued" {
				return st, nil
			}
		}
		wait := pollActiveInterval
		if remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	}
}

// ProbeAccount validates the token with a zero-cost GET /api/v1/freebuff/session
// that carries NO x-freebuff-instance-id header, so unlike CreateSession it
// claims no session slot and burns none of the daily session allowance. The
// probe carries the CLI-parity read headers (x-fb-timezone with the declared
// locality zone — the resolver's when installed, else the host zone — and
// x-freebuff-first-tab-discount "0" claiming no discount, both stamped by
// sessionCall for every session call), and the response
// carries the live pre-join meter — Freebucks, Referral,
// RateLimitsByModel, Standing — plus the account/session state, which
// callers surface for token checks and doctor diagnostics.
//
// A valid token with no active session — a 200 with status "none"/"ended",
// or a probe 404 (no body, the CLI bare-none) — returns the decoded state
// ALONGSIDE ErrNoActiveSession, so the idle-with-balance meter still
// reaches the pool snapshot while old callers branching on
// errors.Is(err, ErrNoActiveSession) behave unchanged. The 404 maps to a
// "none" state with a nil meter. Terminal refusal statuses the upstream
// returns as session states (403 {"status":"banned"}/
// {"status":"country_blocked"}) are converted to the same typed errors the
// session manager surfaces (ErrBanned / ErrCountryBlocked), so probe
// callers can distinguish a dead account from a healthy idle one. All
// other classifications pass through unchanged: 401 → ErrAuthRejected,
// 429 → ErrRateLimited, transport failures as-is. A 200 with any other
// status (active/queued/disabled/…) returns the full *SessionState with a
// nil error.
//
// The probe NEVER POSTs admission and NEVER touches the streak endpoint:
// streak stays on its own GetStreak path.
func (c *Client) ProbeAccount(ctx context.Context) (*SessionState, error) {
	if c.mock != nil {
		return c.mock.Probe(c.token)
	}
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/freebuff/session", nil)
	if err != nil {
		return nil, err
	}
	// The locality + first-tab read headers ride sessionCall now
	// (x-fb-timezone from the installed resolver, else the host zone; the
	// first-tab flag folds the offer into prices). Still zero-cost: no
	// instance header is set, so no slot is claimed.
	state, err := c.sessionCall(req)
	if err != nil {
		return nil, err
	}
	switch state.Status {
	case "none", "ended":
		if state.HTTPStatus == http.StatusNotFound {
			// Probe 404 carries no body: normalize the parse-layer
			// "ended" to the CLI bare-none with a nil meter.
			state.Status = "none"
		}
		return state, ErrNoActiveSession
	case "banned":
		// Build through banFromBody so the typed error matches the
		// classification matrix (issue #306): Body is the truncated raw body,
		// ResumesAt parsed from resumes_at.
		return nil, banFromBody(state.WireBody)
	case "country_blocked":
		return nil, countryBlockFromBody(state.WireBody)
	}
	return state, nil
}

// HostZone reports the host's own IANA zone name for the session-locality
// declaration, or "" when the host carries no usable name: the Go "Local"
// placeholder means "whatever the host is set to" (on a server, usually UTC,
// never a decision), so callers must treat it as unset rather than declare the
// literal string "Local" upstream. It does NOT check that the name loads as an
// IANA zone — egress.ValidZone owns that judgement for the locality rule;
// localIANATimezone owns it for the no-resolver fallback.
func HostZone() string {
	name := time.Local.String()
	if name == "" || name == "Local" {
		return ""
	}
	return name
}

// localIANATimezone reports the host IANA timezone name for the probe
// timezone header (vendor freebucksTimeZoneHeaders: the Intl-resolved host
// zone, evaluated per request so travel needs no restart). A name that
// does not load as an IANA zone (including bare "Local") falls back to
// UTC — always valid, always boring.
func localIANATimezone() string {
	if name := HostZone(); name != "" {
		if _, err := time.LoadLocation(name); err == nil {
			return name
		}
	}
	return "UTC"
}

// StreakInfo is the upstream streak position (docs/maturity-plan.md PR1).
type StreakInfo struct {
	Streak        int    `json:"streak"`
	TodayUsed     bool   `json:"todayUsed"`
	LastUsageDate string `json:"lastUsageDate,omitempty"`
	TimeZone      string `json:"timeZone,omitempty"`
	// FreebucksDailyBonus is the Freebucks a 7+ day streak credits to this
	// account's wallet each Pacific day (vendor freebuff-streak.ts). Nil
	// means the account is not on the Freebucks meter, or the server
	// predates the field — the streak still pays sessions either way.
	FreebucksDailyBonus *float64  `json:"freebucksDailyBonus,omitempty"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

// GetStreak calls GET /api/v1/freebuff/streak with the caller's auth token.
// Returns the streak count, whether activity has been recorded today in the
// account's timezone, and the last usage date.
func (c *Client) GetStreak(ctx context.Context) (*StreakInfo, error) {
	if c.mock != nil {
		if sm, ok := c.mock.(interface {
			Streak(string) (*StreakInfo, error)
		}); ok {
			return sm.Streak(c.token)
		}
		return &StreakInfo{
			Streak:        7,
			TodayUsed:     true,
			LastUsageDate: time.Now().Format("2006-01-02"),
			TimeZone:      "UTC",
			UpdatedAt:     time.Now(),
		}, nil
	}
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/freebuff/streak", nil)
	if err != nil {
		return nil, err
	}
	resp, cancel, classErr := c.do(req, c.sessionCallTimeout)
	if classErr != nil && resp == nil {
		return nil, classErr
	}
	defer releaseCancel(cancel)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		if classErr != nil {
			return nil, classErr
		}
		return nil, fmt.Errorf("streak endpoint returned status %d", resp.StatusCode)
	}
	var raw struct {
		Streak              int      `json:"streak"`
		TodayUsed           bool     `json:"todayUsed"`
		LastUsageDate       string   `json:"lastUsageDate"`
		TimeZone            string   `json:"timeZone"`
		FreebucksDailyBonus *float64 `json:"freebucksDailyBonus"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	return &StreakInfo{
		Streak:              raw.Streak,
		TodayUsed:           raw.TodayUsed,
		LastUsageDate:       raw.LastUsageDate,
		TimeZone:            raw.TimeZone,
		FreebucksDailyBonus: raw.FreebucksDailyBonus,
		UpdatedAt:           time.Now(),
	}, nil
}

// EndSession DELETEs a session and parses the release receipt. A 404 is
// tolerated (nil receipt — the row is already gone, nothing to record).
// CLI attempt IDs use the attempt DELETE route and carry the multi-session,
// purchase-continuity, and desktop-attempt headers. Legacy IDs retain the
// base route and carry only the instance ID. The locality + first-tab headers
// are stamped here too, as for polls and probes.
func (c *Client) EndSession(ctx context.Context, instanceID string) (*SessionRefundReceipt, error) {
	if c.mock != nil {
		return c.mock.EndSession(c.token, instanceID)
	}
	attemptID, attemptMode := sessionAttemptSuffix(instanceID)
	path := "/api/v1/freebuff/session"
	if attemptMode {
		path = "/api/v1/freebuff/session/attempt"
	}
	req, err := c.newRequest(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return nil, err
	}
	if instanceID != "" {
		req.Header.Set(sessionInstanceIDHeader, instanceID)
	}
	if attemptMode {
		req.Header.Set(sessionMultiSessionHeader, "1")
		req.Header.Set(sessionPurchaseContinuityHeader, "1")
		req.Header.Set(sessionDesktopAttemptIDHeader, attemptID)
	}
	// Locality, first-tab, env, and (under a held catalog) protocol/fetch +
	// device signature — same stamper as every other session call.
	c.stampSessionCall(ctx, req)
	resp, cancel, classErr := c.do(req, c.sessionCallTimeout)
	if classErr != nil && resp == nil {
		return nil, classErr
	}
	defer releaseCancel(cancel)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == 404 {
		return nil, nil // nothing to end
	}
	body := drainBody(resp.Body)
	c.noteDeviceKeyErrorFromBody(body)
	// Same precedence as sessionCall: a structured body wins (the ended
	// receipt carries the refund), an unparseable one falls back to the
	// classified error do() already produced (issue #305).
	state, perr := c.parseSessionResponse(req, resp, body)
	if perr != nil {
		if classErr != nil {
			return nil, classErr
		}
		return nil, perr
	}
	return &SessionRefundReceipt{
		Status:  state.Status,
		Refund:  state.FreebucksRefund,
		Pending: state.FreebucksRefundPending,
	}, nil
}

// stampActingUser sets x-freebuff-acting-user-id when the client holds the
// token's OWN account id (ACTING_USER_ID), mirroring the CLI which sends
// the /api/v1/me-derived id on chat (sdk/src/impl/model-provider.ts:361-370,
// VERSION from packages/llm-providers/src/openai-compatible/version.ts:1-5)
// and on agent-runs START/FINISH (database.ts startAgentRun/finishAgentRun:
// Bearer plus the optional acting-user header). Omitted when unset. Only the
// token's own id is ever sent: any other value impersonates a foreign user
// (see the chat-path comment in ChatCompletions).
func (c *Client) stampActingUser(req *http.Request) {
	if c.userID != "" {
		req.Header.Set("x-freebuff-acting-user-id", c.userID)
	}
}

// StartRun POSTs /api/v1/agent-runs with action START and returns the run id.
func (c *Client) StartRun(ctx context.Context, agentID string) (string, error) {
	if c.mock != nil {
		return c.mock.StartRun(c.token, agentID)
	}
	payload, _ := json.Marshal(map[string]any{
		"action":         "START",
		"agentId":        agentID,
		"ancestorRunIds": []string{},
	})
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/agent-runs", payload)
	if err != nil {
		return "", err
	}
	// Never transport-retried (unknown disposition; a replayed START mints
	// a duplicate run): nil GetBody fails fast with exactly one upstream
	// hit. do() holds the same guard belt-and-braces.
	req.GetBody = nil
	// Bearer-only like the CLI (sdk/src/impl/database.ts startAgentRun sends
	// Authorization plus the optional acting-user header and no
	// x-codebuff-api-key — that dual-auth pair lives only on the
	// agent-runtime's web-search/docs/gravity/token-count POSTs
	// (codebuff-web-api.ts callCodebuffV1/callTokenCountAPI), never on
	// agent-runs). newRequest already set Authorization.
	c.stampActingUser(req)
	resp, cancel, classErr := c.do(req, c.sessionCallTimeout)
	if classErr != nil && resp == nil {
		return "", classErr
	}
	defer releaseCancel(cancel)
	defer func() { _ = resp.Body.Close() }()
	if classErr != nil {
		return "", classErr
	}
	body := drainBody(resp.Body)
	var parsed struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return "", fmt.Errorf("upstream: parse START response %q: %w", truncate(body, 200), err)
	}
	if parsed.RunID == "" {
		// Wire parity (packages/agent-runtime/src/run-agent-step.ts
		// @0b7d580c, issue #323): registration ending on the run's abort
		// signal is a cancel, not a failure — surface the context error
		// so callers drop it instead of classifying an upstream failure.
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("upstream: START response missing runId: %q", truncate(body, 200))
	}
	return parsed.RunID, nil
}

// RunStep is one agent-run step, batched in memory and sent WITH FINISH
// (issue #114, CLI parity: upstream/freebuff/sdk/src/impl/database.ts
// pendingAgentStepSchema — the CLI has NO /steps endpoint, so steps ride
// the FINISH payload). The proxy records one step per completed chat call.
type RunStep struct {
	// ID is a per-step UUID minted at record time.
	ID string `json:"id"`
	// StepNumber is the 1-based per-run step index (sequential 1,2,3…).
	StepNumber int `json:"stepNumber"`
	// Credits is always 0 for the proxy (the upstream account owns spend);
	// sent verbatim — the CLI emits "credits":0 on free-tier steps
	// (live-captured 2026-09-27), never elided.
	Credits int `json:"credits"`
	// ChildRunIDs is empty for proxy-recorded steps (child runs are
	// separate runs, not steps); sent as [] — the CLI emits
	// "childRunIds":[] verbatim, never null or elided.
	ChildRunIDs []string `json:"childRunIds"`
	// MessageID is the completed chat response id; null when the stream
	// never carried one (the CLI schema allows a null messageId).
	MessageID *string `json:"messageId"`
	// Status mirrors the CLI step lifecycle; proxy-recorded steps are
	// always "completed" (recorded only after a successful chat), sent
	// verbatim like the CLI.
	Status string `json:"status"`
	// StartTime is the step start instant, RFC3339Nano UTC.
	StartTime string `json:"startTime"`
}

// FinishRun POSTs /api/v1/agent-runs with action FINISH, reporting the
// run's honest terminal status and its completed steps (issue #114, CLI
// parity: upstream/freebuff/sdk/src/impl/database.ts finishAgentRun — the
// full payload is sent in ONE request; there is no /steps endpoint).
// totalSteps is the step count the manager reports (len(steps) preferred,
// falling back to the request count when no steps were recorded);
// errorMessage is omitted when empty and truncated to 5000 runes otherwise,
// exactly like the CLI's truncateString(errorMessage, 5000).
func (c *Client) FinishRun(ctx context.Context, runID, status string, totalSteps int, steps []RunStep, errorMessage string) error {
	if c.mock != nil {
		return c.mock.FinishRun(c.token)
	}
	if steps == nil {
		steps = []RunStep{}
	}
	payload := map[string]any{
		"action":        "FINISH",
		"runId":         runID,
		"status":        status,
		"totalSteps":    totalSteps,
		"directCredits": 0,
		"totalCredits":  0,
		"steps":         steps,
	}
	if errorMessage != "" {
		payload["errorMessage"] = truncateRunes(errorMessage, 5000)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/agent-runs", body)
	if err != nil {
		return err
	}
	// Never transport-retried (a replayed FINISH double-reports steps);
	// see StartRun. do() holds the same guard belt-and-braces.
	req.GetBody = nil
	// Bearer-only like the CLI (sdk/src/impl/database.ts finishAgentRun sends
	// Authorization plus the optional acting-user header and no
	// x-codebuff-api-key — see StartRun). newRequest already set
	// Authorization.
	c.stampActingUser(req)

	resp, cancel, classErr := c.do(req, c.sessionCallTimeout)
	if classErr != nil && resp == nil {
		return classErr
	}
	defer releaseCancel(cancel)
	defer func() { _ = resp.Body.Close() }()
	if classErr != nil {
		return classErr
	}
	return nil
}

// --- internals ---

// sessionCall performs a session control call: parse the JSON body into a
// SessionState; errors are classified through the standard matrix. Every
// session call funnels through here (admission POST, poll GET, probe GET),
// so the CLI-parity headers are stamped here (see stampSessionCall).
func (c *Client) sessionCall(req *http.Request) (*SessionState, error) {
	c.stampSessionCall(req.Context(), req)
	resp, cancel, classErr := c.do(req, c.sessionCallTimeout)
	if classErr != nil && resp == nil {
		return nil, classErr
	}
	defer releaseCancel(cancel)
	defer func() { _ = resp.Body.Close() }()
	body := drainBody(resp.Body)
	// A device-key-unknown refusal forgets this account's registration so
	// the next call registers again (vendor noteFreebuffDeviceKeyError);
	// any other body is untouched.
	c.noteDeviceKeyErrorFromBody(body)
	// A session control call always tries to parse the body first: a
	// structured 4xx/5xx carries the session status the callers switch on
	// (model_locked/model_unavailable/ip_capped/spend_limited/...), and a
	// 404 maps through parseSessionResponse (legacy create -> disabled,
	// poll/probe -> ended; admission-route POST -> fail closed). Only an
	// unparseable body falls back to the classified error do() already
	// produced (issue #305) — except the admission fail-closed signal,
	// which outranks do()'s already-classified 404: do() cannot know the
	// POST targeted the dedicated route.
	state, perr := c.parseSessionResponse(req, resp, body)
	if perr == nil {
		return state, nil
	}
	if errors.Is(perr, ErrSessionAdmissionUnsupported) {
		return nil, perr
	}
	if classErr != nil {
		return nil, classErr
	}
	return nil, perr
}

// stampSessionCall stamps the headers the vendor's callFreebuffSession
// spreads into EVERY session call (POST, GET and DELETE alike):
// Authorization rides newRequest; here the locality timezone (the server
// derives the account's daily reset zone from it at admission time), the
// first-tab offer flag (always the boring "0": the proxy holds no
// user-confirmed offer state, exactly like a CLI call with firstTabDiscount
// unset — a real offer change surfaces as 409 first_tab_discount_changed),
// and the static client-environment descriptor.
//
// Under a held catalog (catalog mode) the call also carries the protocol
// header, the fetch id, and this install's device signature over the
// body-less request (vendor requestFreebuffSession: session calls send no
// body, so the signature covers the empty one). Nil catalog is fallback —
// the pre-catalog shape, byte-identical to before. Registration runs when
// needed (register=true): the first catalog GET never registers, but every
// session call after a catalog is held does.
func (c *Client) stampSessionCall(ctx context.Context, req *http.Request) {
	req.Header.Set(FreebucksTimezoneHeader, c.sessionTimezone())
	req.Header.Set(FirstTabDiscountHeader, "0")
	stampClientEnv(req.Header)
	held := c.heldModelCatalog()
	if held == nil {
		return
	}
	req.Header.Set(CatalogProtocolHeader, CatalogProtocolVersion)
	if held.FetchID != "" {
		req.Header.Set(CatalogFetchHeader, held.FetchID)
	}
	for name, value := range c.deviceHeaders(ctx, req.Method, req.URL.String(), nil, held.FetchID, true) {
		req.Header.Set(name, value)
	}
}
