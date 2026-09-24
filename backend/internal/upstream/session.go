package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Session wire vocabulary shared with the upstream CLI and desktop clients
// (vendor af898dc, common/src/constants/freebuff-models.ts): the dedicated
// admission/reuse routes and their headers. Dedicated routes fail closed on
// servers predating these guarantees — never fall back to the legacy path.
const (
	// SessionAdmissionPath is the dedicated session-create route.
	SessionAdmissionPath = "/api/v1/freebuff/session/admission"
	// SessionReusePath reuses an exact live single-session instance without
	// buying or taking over. No proxy caller needs it yet (the CLI does
	// not call it either); pinned so every client agrees on the string.
	SessionReusePath = "/api/v1/freebuff/session/reuse"
	// ReuseInstanceHeader names the exact live instance to reuse.
	ReuseInstanceHeader = "x-freebuff-reuse-instance-id"
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

// isSessionAdmissionRequest reports whether req targets the dedicated
// admission route (suffix match: the client base URL may carry a prefix).
func isSessionAdmissionRequest(req *http.Request) bool {
	return req != nil && req.URL != nil && strings.HasSuffix(req.URL.Path, SessionAdmissionPath)
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
// requested model header. The POST carries NO body and therefore no
// Content-Type (#120): the CLI's session POST is a bare fetch with
// Authorization + the optional x-freebuff-model header plus the wallet
// spend-limit header (upstream/freebuff freebuff-session-api.ts
// callFreebuffSession; codebuff-api.ts sets the same request shape), plus the
// locality header sessionCall stamps on every session call.
func (c *Client) CreateSessionForModel(ctx context.Context, model string) (*SessionState, error) {
	if c.mock != nil {
		return c.mock.CreateSession(c.token, model)
	}
	req, err := c.newRequest(ctx, http.MethodPost, SessionAdmissionPath, nil)
	if err != nil {
		return nil, err
	}
	if model != "" {
		req.Header.Set("x-freebuff-model", model)
	}
	req.Header.Set(WalletSpendLimitHeader, DefaultWalletSpendLimit)
	return c.sessionCall(req)
}

// GetSession polls /api/v1/freebuff/session for the given instance. A poll
// 404 maps to Status "ended" (the session vanished upstream; the session
// manager re-creates it). An admission-route create never maps 404 to
// "disabled" — it fails closed with ErrSessionAdmissionUnsupported.
func (c *Client) GetSession(ctx context.Context, instanceID string) (*SessionState, error) {
	return c.GetSessionWithOpts(ctx, instanceID, false)
}

// GetSessionWithOpts polls /api/v1/freebuff/session with an optional compact
// response header. There is deliberately NO heartbeat option: the CLI never
// sends x-freebuff-heartbeat (Desktop-only, upstream/freebuff
// freebuff-models.ts:1212-1215); liveness comes from the recurring compact
// GET itself. The locality header rides every session call (sessionCall),
// the poll included.
func (c *Client) GetSessionWithOpts(ctx context.Context, instanceID string, compact bool) (*SessionState, error) {
	if c.mock != nil {
		return c.mock.GetSession(c.token, "")
	}
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/freebuff/session", nil)
	if err != nil {
		return nil, err
	}
	if instanceID != "" {
		req.Header.Set("x-freebuff-instance-id", instanceID)
	}
	if compact {
		req.Header.Set("x-freebuff-compact-session", "1")
	}
	return c.sessionCall(req)
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

// EndSession DELETEs /api/v1/freebuff/session and parses the release
// receipt (vendor af898dc: {status:'ended', freebucksRefund?,
// freebucksRefundPending?}). A 404 is tolerated (nil receipt — the row is
// already gone, nothing to record). The DELETE carries
// x-freebuff-instance-id when the caller holds one (vendor parity:
// cli/src/utils/freebuff-session-api.ts callFreebuffSession sends the
// instance header on GET/DELETE when known; the session POST carries
// x-freebuff-model and never the instance id). An empty instanceID omits
// the header (the caller genuinely holds no slot).
func (c *Client) EndSession(ctx context.Context, instanceID string) (*SessionRefundReceipt, error) {
	if c.mock != nil {
		return c.mock.EndSession(c.token, instanceID)
	}
	req, err := c.newRequest(ctx, http.MethodDelete, "/api/v1/freebuff/session", nil)
	if err != nil {
		return nil, err
	}
	if instanceID != "" {
		req.Header.Set("x-freebuff-instance-id", instanceID)
	}
	// The DELETE does not route through sessionCall (it parses a release
	// receipt, not a SessionState), so the locality + first-tab headers are
	// stamped here too: the vendor spreads freebucksTimeZoneHeaders() and the
	// first-tab discount header into the refund call exactly like the poll
	// and the probe.
	req.Header.Set(FreebucksTimezoneHeader, c.sessionTimezone())
	req.Header.Set(FirstTabDiscountHeader, "0")

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
// the /api/v1/me-derived id on chat (model-provider.ts) and on agent-runs
// START/FINISH (database.ts startAgentRun/finishAgentRun: Bearer plus the
// optional acting-user header). Omitted when unset. Only the token's own
// id is ever sent: any other value impersonates a foreign user (see the
// chat-path comment in ChatCompletions).
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
	// Credits is always 0 for the proxy (the upstream account owns spend).
	Credits int `json:"credits,omitempty"`
	// ChildRunIDs is empty for proxy-recorded steps (child runs are
	// separate runs, not steps).
	ChildRunIDs []string `json:"childRunIds,omitempty"`
	// MessageID is the completed chat response id; null when the stream
	// never carried one (the CLI schema allows a null messageId).
	MessageID *string `json:"messageId"`
	// Status mirrors the CLI step lifecycle; proxy-recorded steps are
	// always "completed" (recorded only after a successful chat).
	Status string `json:"status,omitempty"`
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
// session call funnels through here (admission POST, poll GET, probe GET), so
// this is where the locality header is stamped: the vendor's
// callFreebuffSession spreads freebucksTimeZoneHeaders() into EVERY session
// call, and the upstream server derives the account's daily reset zone from
// it at admission time — the call where it matters most.
func (c *Client) sessionCall(req *http.Request) (*SessionState, error) {
	req.Header.Set(FreebucksTimezoneHeader, c.sessionTimezone())
	// First-tab offer state rides every session call (callFreebuffSession
	// stamps [FIRST_TAB_DISCOUNT_HEADER] on POST/GET/DELETE alike). The proxy
	// holds no user-confirmed offer state, so it always sends the boring "0"
	// exactly like a CLI call with firstTabDiscount unset; a real offer change
	// surfaces as 409 first_tab_discount_changed through the classify matrix.
	req.Header.Set(FirstTabDiscountHeader, "0")
	resp, cancel, classErr := c.do(req, c.sessionCallTimeout)
	if classErr != nil && resp == nil {
		return nil, classErr
	}
	defer releaseCancel(cancel)
	defer func() { _ = resp.Body.Close() }()
	body := drainBody(resp.Body)
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
