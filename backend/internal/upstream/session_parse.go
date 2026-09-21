// Session response parsing: parseSessionResponse (the JSON decode and state
// build behind sessionCall), the per-model quota/standing parser, and the
// availability-window parser (issue #158).
package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// SessionState is the parsed result of a free-session create/poll.
type SessionState struct {
	Status             string
	InstanceID         string
	Model              string
	CurrentModel       string
	RequestedModel     string
	AccessTier         string
	ExpiresAt          time.Time
	AdmittedAt         time.Time
	RemainingMs        int64
	GracePeriodEndsAt  time.Time
	GraceRemainingMs   int64
	Position           int
	QueueDepth         int
	EstimatedWaitMs    int
	PollAt             time.Time
	CountryCode        string
	CountryBlockReason string
	IpPrivacySignals   []string
	ActiveUsersForIP   int
	Limit              float64
	RecentCount        float64
	ResetAt            time.Time
	ResumesAt          time.Time
	RetryAfterMs       int64
	AvailableHours     string
	Message            string
	// SubscriptionTierID is the raw upstream subscription.tierId from the
	// session response (the upstream plan id behind hasPaidSubscription).
	// Kept verbatim — never parsed into a plan name — and left "" when the
	// subscription block is absent or null.
	SubscriptionTierID string
	// WireBody is the raw upstream body the state was parsed from. ProbeAccount
	// uses it to build BanError/CountryBlockedError through the shared
	// banFromBody/countryBlockFromBody constructors (issue #306), so its typed
	// errors match the classification matrix exactly.
	WireBody string
	// UnavailableWindow is the parsed availability window carried by a
	// model_unavailable admission response (issue #158); nil when the
	// response omitted availableHours or the string could not be parsed.
	UnavailableWindow *AvailabilityWindow
	// GlmPromo carries the raw JSON of the upstream glmPromo block
	// ({dailySessions, endsAt}) when the probe/admission response includes
	// it. Kept as a string so callers render the shape without the upstream
	// adding fields; "" when absent.
	GlmPromo string
	// RateLimitsByModel carries the live per-model session quotas from the
	// admission/poll response (key = model id). Absent on compact polls and
	// pre-join (none) responses; never required.
	RateLimitsByModel map[string]ModelQuota
	// Standing is the upstream account standing block (issue #96), parsed
	// from the session response's "standing" field ({level,label,score,
	// nextLevelAt,nextLevel}); nil when the response omits it.
	Standing *SessionStanding
	// Referral is the upstream referral block (FreebuffReferralInfo), parsed
	// from the session response's "referral" field; nil when omitted.
	Referral *SessionReferral
	// Freebucks is the upstream Freebucks allowance block (issue #232),
	// parsed from the session response's "freebucks" field; nil when omitted.
	Freebucks *FreebucksInfo
	// UpgradeHint carries the upstream promotional or upgrade broadcast
	// hint ({url, message}) if provided by the session server; nil otherwise.
	UpgradeHint *SessionUpgradeHint
	// HTTPStatus is the HTTP status of the session control response the
	// state was parsed from (0 when synthesized, e.g. the 404 mappings).
	// Terminal admission refusals surface it so callers report the honest
	// upstream status instead of inventing one.
	HTTPStatus int
	// FreebucksRefund is the settled early-end refund from a session DELETE
	// receipt (freebucksRefund, including zero); nil when the server sent
	// none. FreebucksRefundPending mirrors freebucksRefundPending: final
	// usage is still outstanding and the DELETE must be replayed with the
	// same instance id for its receipt (vendor af898dc).
	FreebucksRefund        *float64
	FreebucksRefundPending bool
	// WalletConsent mirrors the walletConsent block of a consent_required
	// admission refusal (vendor af898dc: {price, walletSpend}); nil on any
	// other status.
	WalletConsent *WalletConsent
	// UpdateRequired mirrors the upstream model_unavailable updateRequired
	// flag (vendor 3f00c77, Desktop multi-session path only): the model is
	// fine but this client build cannot resume a purchased hour, so the
	// server refused rather than charge the hour again. False when the
	// response omits it. This is an admission refusal flag, not the
	// freebuff_update_required body marker (WireCodeFreebuffUpdateRequired:
	// stale CLI app version).
	UpdateRequired bool
	// PurchasesPaused mirrors the upstream model_unavailable purchasesPaused
	// flag (vendor 3f00c77, Desktop multi-session path only): minting new
	// purchased sessions is paused server-side, so an up-to-date build
	// needing a fresh purchase is refused rather than charged for an hour
	// it could not be issued. False when the response omits it.
	PurchasesPaused bool
	// LimitedOfferReason mirrors the upstream model_unavailable
	// limitedOfferReason (vendor e2b911eca): only set on
	// model_unavailable, '' otherwise. Values used|closed|exhausted are an
	// opaque passthrough like CountryBlockReason — never switched on, never
	// clamped; anything else normalizes to ''. A 'used' personal trial
	// cannot be replenished by waiting or upgrading, so unlike
	// closed/exhausted it carries no countdown (the state build skips the
	// UnavailableWindow parse for it).
	LimitedOfferReason string
	// LimitedModelOffers carries the capacity-limited models the picker may
	// additionally offer right now, parsed from the pre-join (none)
	// response (vendor e2b911eca, FreebuffLimitedModelOffer); nil when the
	// response carries none.
	LimitedModelOffers []LimitedModelOffer
	// WindowHours / FreebucksShortfall mirror the error-body window evidence
	// (RateLimitError.WindowHours / FreebucksShortfall): a session status
	// response reporting the vendor's freebucks ceiling carries the same two
	// markers, and the poll/admission status paths build their
	// RateLimitError from this state — without them a window refusal would
	// silently degrade to a plain rate limit on its way to the cooldown
	// memory.
	WindowHours        int
	FreebucksShortfall bool
}

// WalletConsent is the upstream wallet-consent demand: the admission price
// and the wallet Freebucks spend the user must confirm by re-picking the
// model with an explicit spend limit (vendor af898dc, consent_required).
type WalletConsent struct {
	Price       float64 `json:"price"`
	WalletSpend float64 `json:"walletSpend"`
}

// rawSubscription mirrors the upstream subscription block's plan tier id
// (vendor subscription.tierId; hasPaidSubscription derives from this
// block's presence). Only tierId is consumed; an absent or null block
// leaves the state's tier "".
type rawSubscription struct {
	TierID string `json:"tierId"`
}

// rawWalletConsent mirrors FreebuffWalletConsent (vendor af898dc).
type rawWalletConsent struct {
	Price       float64 `json:"price"`
	WalletSpend float64 `json:"walletSpend"`
}

// LimitedModelOffer mirrors FreebuffLimitedModelOffer (vendor e2b911eca):
// one capacity-limited model wave entry on the pre-join (none) response.
// UserResetAt is *string so a JSON null stays nil (never a zero time); a
// one-per-campaign offer carries null.
type LimitedModelOffer struct {
	Model         string  `json:"model"`
	Remaining     int     `json:"remaining"`
	Total         int     `json:"total"`
	UserRemaining int     `json:"userRemaining"`
	UserResetAt   *string `json:"userResetAt"`
}

// Joinable reports whether the user may still start a session from this
// offer: the client treats userRemaining == 0 as "not now" rather than
// hiding the row.
func (o LimitedModelOffer) Joinable() bool { return o.UserRemaining > 0 }

// Fable 5.1 trace-campaign pins (vendor e2b911eca,
// common/src/constants/freebuff-models.ts): the capacity-limited trial
// model id and its picker display name. Fable is deliberately NOT a
// standing picker model — the server advertises it only while the shared
// pool has sessions left (limitedModelOffers), and it runs on the base2
// root base2-free-fable as the campaign's deliberate base2 exception
// (free-agent-selection.ts; there is no base3-free-fable and one must
// never be resolved — the registry text parser cannot evaluate the base3
// by-model maps, which enforces this today).
const (
	FreebuffFable51ModelID     = "anthropic/claude-fable-5.1"
	FreebuffFable51DisplayName = "Claude Fable 5.1"
)

// LimitedOfferSessionLimit is the per-user campaign allowance: one
// admission per user for the entire campaign, and early ends do not
// refund it (vendor FREEBUFF_LIMITED_OFFER_SESSION_LIMIT).
const LimitedOfferSessionLimit = 1

// LimitedOfferMaxSessions is the hard ceiling for the Fable 5.1 trace
// campaign, even if an env value is larger (vendor
// FREEBUFF_LIMITED_OFFER_MAX_SESSIONS).
const LimitedOfferMaxSessions = 500

func (c *Client) parseSessionResponse(req *http.Request, resp *http.Response, body string) (*SessionState, error) {
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		if req.Method == http.MethodPost && isSessionAdmissionRequest(req) {
			// The dedicated admission route fails closed on servers
			// predating its guarantees: 404/405 means the server is too
			// old to start or resume sessions safely — NOT that free mode
			// is disabled, and never a cue to retry the legacy session
			// POST (upstream freebuff-session-api.ts callFreebuffSession
			// throws session_admission_unsupported there).
			return nil, fmt.Errorf("%w: %s", ErrSessionAdmissionUnsupported, SessionUnsupportedMessage)
		}
		if resp.StatusCode == http.StatusNotFound {
			if req.Method == http.MethodPost {
				// A legacy-path create 404 means no session slot exists upstream.
				return &SessionState{Status: "disabled", HTTPStatus: resp.StatusCode}, nil
			}
			// A poll 404 means the session no longer exists upstream (expired or
			// evicted). Treat it as ended so the session manager re-creates it,
			// instead of caching a permanent "disabled" with no expiry.
			return &SessionState{Status: "ended", HTTPStatus: resp.StatusCode}, nil
		}
	}

	c.dump("session", req, resp.StatusCode, body)

	var raw struct {
		Status                 string                   `json:"status"`
		InstanceID             string                   `json:"instanceId"`
		Model                  string                   `json:"model"`
		CurrentModel           string                   `json:"currentModel"`
		RequestedModel         string                   `json:"requestedModel"`
		ExpiresAt              any                      `json:"expiresAt"`
		AdmittedAt             any                      `json:"admittedAt"`
		RemainingMs            int64                    `json:"remainingMs"`
		GracePeriodEndsAt      any                      `json:"gracePeriodEndsAt"`
		GracePeriodRemainingMs int64                    `json:"gracePeriodRemainingMs"`
		Position               int                      `json:"position"`
		QueueDepth             int                      `json:"queueDepth"`
		EstimatedWaitMs        int                      `json:"estimatedWaitMs"`
		PollAt                 any                      `json:"pollAt"`
		CountryCode            string                   `json:"countryCode"`
		CountryBlockReason     string                   `json:"countryBlockReason"`
		AccessTier             string                   `json:"accessTier"`
		IpPrivacySignals       []string                 `json:"ipPrivacySignals"`
		ActiveUsersForIP       int                      `json:"activeUsersForIp"`
		Limit                  float64                  `json:"limit"`
		RecentCount            float64                  `json:"recentCount"`
		ResetAt                any                      `json:"resetAt"`
		ResumesAt              any                      `json:"resumes_at"`
		RetryAfterMs           int64                    `json:"retryAfterMs"`
		AvailableHours         string                   `json:"availableHours"`
		LimitedOfferReason     string                   `json:"limitedOfferReason"`
		LimitedModelOffers     []LimitedModelOffer      `json:"limitedModelOffers"`
		Message                string                   `json:"message"`
		GlmPromo               json.RawMessage          `json:"glmPromo"`
		RateLimitsByModel      map[string]rawModelQuota `json:"rateLimitsByModel"`
		Standing               *rawStanding             `json:"standing"`
		Referral               *rawReferral             `json:"referral"`
		Freebucks              *rawFreebucks            `json:"freebucks"`
		Subscription           *rawSubscription         `json:"subscription"`
		UpgradeHint            *struct {
			URL     string `json:"url"`
			Message string `json:"message"`
		} `json:"upgradeHint"`
		// UpdateRequired / PurchasesPaused decode the optional Desktop
		// multi-session refusal flags (vendor 3f00c77); absent = false.
		UpdateRequired  bool `json:"updateRequired"`
		PurchasesPaused bool `json:"purchasesPaused"`
		// freebucksRefund / freebucksRefundPending ride the ended DELETE
		// receipt (vendor af898dc); walletConsent rides consent_required.
		FreebucksRefund        *float64          `json:"freebucksRefund"`
		FreebucksRefundPending bool              `json:"freebucksRefundPending"`
		WalletConsent          *rawWalletConsent `json:"walletConsent"`
		// Window evidence for the freebucks ceiling, same markers as the
		// error-body parse (windowHours + freebucksShortfall).
		WindowHours        int `json:"windowHours"`
		FreebucksShortfall any `json:"freebucksShortfall"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err == nil && raw.Status != "" {
		state := &SessionState{
			Status:             raw.Status,
			WireBody:           body,
			InstanceID:         raw.InstanceID,
			Model:              raw.Model,
			CurrentModel:       raw.CurrentModel,
			RequestedModel:     raw.RequestedModel,
			RemainingMs:        raw.RemainingMs,
			GraceRemainingMs:   raw.GracePeriodRemainingMs,
			Position:           raw.Position,
			QueueDepth:         raw.QueueDepth,
			EstimatedWaitMs:    raw.EstimatedWaitMs,
			CountryCode:        raw.CountryCode,
			CountryBlockReason: raw.CountryBlockReason,
			IpPrivacySignals:   raw.IpPrivacySignals,
			AccessTier:         raw.AccessTier,
			ActiveUsersForIP:   raw.ActiveUsersForIP,
			Limit:              raw.Limit,
			RecentCount:        raw.RecentCount,
			RetryAfterMs:       raw.RetryAfterMs,
			WindowHours:        raw.WindowHours,
			FreebucksShortfall: freebucksShortfallPresent(raw.FreebucksShortfall),
			AvailableHours:     raw.AvailableHours,
			Message:            raw.Message,
			GlmPromo:           string(raw.GlmPromo),
			UpdateRequired:     raw.UpdateRequired,
			PurchasesPaused:    raw.PurchasesPaused,
			// LimitedOfferReason deliberately unset here: the switch below
			// admits only the three known vendor members, anything else stays ''.
		}
		// limitedOfferReason is an opaque passthrough like CountryBlockReason,
		// but only the three vendor members survive: an unknown string (a
		// newer server member this build does not know) normalizes to '' so
		// callers can treat '' as "no offer info".
		switch raw.LimitedOfferReason {
		case "used", "closed", "exhausted":
			state.LimitedOfferReason = raw.LimitedOfferReason
		}
		if len(raw.LimitedModelOffers) > 0 {
			state.LimitedModelOffers = append([]LimitedModelOffer(nil), raw.LimitedModelOffers...)
		}
		// A 'used' personal trial cannot be replenished by waiting, so it
		// carries no countdown: skip the UnavailableWindow parse and leave
		// the refusal terminal (vendor e2b911eca message/fallback branches
		// treat 'used' distinctly from 'closed'/'exhausted').
		if raw.Status == "model_unavailable" && raw.AvailableHours != "" && state.LimitedOfferReason != "used" {
			if w, ok := ParseAvailabilityWindow(raw.AvailableHours); ok {
				state.UnavailableWindow = &w
			}
		}
		state.HTTPStatus = resp.StatusCode
		// subscription.tierId is a plain passthrough: present keeps the raw
		// id, absent/null leaves it "" (never an error, never a plan name).
		if raw.Subscription != nil {
			state.SubscriptionTierID = raw.Subscription.TierID
		}
		state.FreebucksRefund = raw.FreebucksRefund
		state.FreebucksRefundPending = raw.FreebucksRefundPending
		if raw.WalletConsent != nil {
			state.WalletConsent = &WalletConsent{
				Price:       raw.WalletConsent.Price,
				WalletSpend: raw.WalletConsent.WalletSpend,
			}
		}
		if raw.Standing != nil {
			standing := &SessionStanding{
				Level:        raw.Standing.Level,
				Label:        raw.Standing.Label,
				Score:        raw.Standing.Score,
				NextLevel:    raw.Standing.NextLevel,
				CappedBy:     raw.Standing.CappedBy,
				CappedReason: raw.Standing.CappedReason,
				Blurb:        raw.Standing.Blurb,
			}
			if standing.NextLevelAt, err = parseFlexTime(raw.Standing.NextLevelAt); err != nil {
				standing.NextLevelAt = time.Time{}
			}
			for _, s := range raw.Standing.NextSteps {
				standing.NextSteps = append(standing.NextSteps, StandingNextStep(s))
			}
			state.Standing = standing
		}
		if raw.Referral != nil {
			ref := &SessionReferral{
				Code:                    raw.Referral.Code,
				ReferrerName:            raw.Referral.ReferrerName,
				QualifiedCount:          raw.Referral.QualifiedCount,
				WeeklySessionsRemaining: raw.Referral.WeeklySessionsRemaining,
				GithubLinked:            raw.Referral.GithubLinked,
			}
			if ref.ResetAt, err = parseFlexTime(raw.Referral.ResetAt); err != nil {
				ref.ResetAt = time.Time{}
			}
			state.Referral = ref
		}
		if raw.Freebucks != nil {
			fb := &FreebucksInfo{
				Balance:      raw.Freebucks.Balance,
				Prices:       raw.Freebucks.Prices,
				ListPrices:   raw.Freebucks.ListPrices,
				PriceNotices: raw.Freebucks.PriceNotices,
			}
			if raw.Freebucks.OffPeak != nil {
				fb.OffPeak = make(map[string]FreebuffOffPeakPrice, len(raw.Freebucks.OffPeak))
				for id, o := range raw.Freebucks.OffPeak {
					fb.OffPeak[id] = FreebuffOffPeakPrice(o)
				}
			}
			fb.ClaimableGrant = raw.Freebucks.ClaimableGrant
			if raw.Freebucks.Upgrade != nil {
				fb.Upgrade = &FreebucksUpgrade{
					Kind:    raw.Freebucks.Upgrade.Kind,
					CTA:     raw.Freebucks.Upgrade.CTA,
					Tooltip: raw.Freebucks.Upgrade.Tooltip,
					ModelID: raw.Freebucks.Upgrade.ModelID,
				}
			}
			if raw.Freebucks.QuotaExempt != nil {
				fb.QuotaExempt = *raw.Freebucks.QuotaExempt
			}
			if raw.Freebucks.FirstTabDiscount != nil {
				rd := raw.Freebucks.FirstTabDiscount
				d := &FreebucksFirstTabDiscount{Amount: rd.Amount, Available: rd.Available}
				if rd.Holder != nil {
					d.Holder = &FreebucksFirstTabDiscountHolder{
						Surface:   rd.Holder.Surface,
						ExpiresAt: rd.Holder.ExpiresAt,
					}
					if rd.Holder.InstanceID != nil {
						id := *rd.Holder.InstanceID
						d.Holder.InstanceID = &id
					}
				}
				fb.FirstTabDiscount = d
			}
			for _, c := range raw.Freebucks.PriceChanges {
				fb.PriceChanges = append(fb.PriceChanges, FreebucksPriceChange(c))
			}
			// Apply the server's announced schedule at parse time so every
			// consumer (pool meter, dashboard prices) reads effective prices
			// (issue #350 — mirrors freebucksOf applying the schedule).
			ApplyFreebucksPriceChanges(fb, time.Now())
			if raw.Freebucks.PlanID != nil {
				fb.PlanID = *raw.Freebucks.PlanID
			}
			fb.Daily = windowFromRaw(raw.Freebucks.Daily)
			if raw.Freebucks.Wallet != nil {
				fb.Wallet.Balance = raw.Freebucks.Wallet.Balance
				fb.Wallet.MonthlyBonus = raw.Freebucks.Wallet.MonthlyBonus
				if t, terr := parseFlexTime(raw.Freebucks.Wallet.NextBonusAt); terr == nil {
					fb.Wallet.NextBonusAt = t
				}
			}
			if raw.Freebucks.Spend != nil {
				fb.Spend.LimitUsd = raw.Freebucks.Spend.LimitUsd
				if t, terr := parseFlexTime(raw.Freebucks.Spend.ResetAt); terr == nil {
					fb.Spend.ResetAt = t
				}
			}
			if raw.Freebucks.Monthly != nil {
				m := &FreebucksMonthlyAllowance{
					LimitUsd:     raw.Freebucks.Monthly.LimitUsd,
					SpentUsd:     raw.Freebucks.Monthly.SpentUsd,
					RemainingUsd: raw.Freebucks.Monthly.RemainingUsd,
				}
				if t, terr := parseFlexTime(raw.Freebucks.Monthly.ResetAt); terr == nil {
					m.ResetAt = t
				}
				fb.Monthly = m
			}
			state.Freebucks = fb
		}
		if state.ExpiresAt, err = parseFlexTime(raw.ExpiresAt); err != nil {
			state.ExpiresAt = time.Time{}
		}
		if state.AdmittedAt, err = parseFlexTime(raw.AdmittedAt); err != nil {
			state.AdmittedAt = time.Time{}
		}
		if state.GracePeriodEndsAt, err = parseFlexTime(raw.GracePeriodEndsAt); err != nil {
			state.GracePeriodEndsAt = time.Time{}
		}
		if state.PollAt, err = parseFlexTime(raw.PollAt); err != nil {
			state.PollAt = time.Time{}
		}
		if state.ResetAt, err = parseFlexTime(raw.ResetAt); err != nil {
			state.ResetAt = time.Time{}
		}
		if state.ResumesAt, err = parseFlexTime(raw.ResumesAt); err != nil {
			state.ResumesAt = time.Time{}
		}
		if len(raw.RateLimitsByModel) > 0 {
			state.RateLimitsByModel = make(map[string]ModelQuota, len(raw.RateLimitsByModel))
			for modelID, q := range raw.RateLimitsByModel {
				mq := ModelQuota{
					Model:       q.Model,
					Limit:       q.Limit,
					RecentCount: q.RecentCount,
					Period:      q.Period,
					Pool:        q.Pool,
					PoolLabel:   q.PoolLabel,
					Entitlement: q.EntitlementBreakdown,
				}
				if mq.Model == "" {
					mq.Model = modelID
				}
				if resetAt, perr := parseFlexTime(q.ResetAt); perr == nil {
					mq.ResetAt = resetAt
				}
				state.RateLimitsByModel[modelID] = mq
			}
		}
		return state, nil
	}
	if resp.StatusCode >= 400 {
		// Pure matrix mapping only: do() already classified this same
		// response (ledger count + `upstream rate limit classified` line),
		// and both sessionCall/EndSession prefer do()'s error whenever one
		// exists — reusing the wrapper here would log and count the same
		// refusal twice. The classification log fires exactly once, in do().
		return nil, classifyError(resp.StatusCode, body, resp.Header)
	}
	if req.Method == http.MethodDelete {
		// Pre-receipt servers answer a DELETE with 2xx and an empty (or
		// otherwise receipt-less) body: the slot is released, there is
		// just no refund to record. Succeed with an empty receipt rather
		// than failing teardown on old servers.
		return &SessionState{Status: "ended", HTTPStatus: resp.StatusCode}, nil
	}

	return nil, fmt.Errorf("upstream: unparseable session response %q", truncate(body, 200))
}
