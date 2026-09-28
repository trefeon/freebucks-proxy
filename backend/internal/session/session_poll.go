// session_poll.go — session liveness polling, split from session.go (CI line
// cap): the periodic compact poll (Poll), persisted-session resume
// (pollPersisted), the upstream status → typed-error/reason mappings
// (statusError, tableReason) shared by the poll and refresh paths, and the
// admission probe-cache TTL setter (SetAdmissionProbeTTL).
package session

import (
	"context"
	cryptoRand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"freebucks-proxy/backend/internal/upstream"
)

// SetAdmissionProbeTTL configures the admission probe cache TTL (issue #60):
// session poll GETs within d of the last successful session response are
// skipped. d <= 0 disables. Wired by the pool from SESSION_PROBE_CACHE_TTL;
// safe to call at runtime.
func (m *Manager) SetAdmissionProbeTTL(d time.Duration) {
	m.mu.Lock()
	m.probeTTL = d
	m.mu.Unlock()
}

// tableReason maps an upstream session status to the terminal-event reason
// vocabulary. Used by the poll/refresh drop paths so the logged reason
// is always one of the table values; the raw upstream status rides in the
// log's status field.
func tableReason(status string) string {
	if status == "superseded" {
		return reasonSuperseded
	}
	return reasonEnded
}

// statusError maps an upstream session status to the typed error callers
// use for recovery (token cooldown, region surfacing). st supplies the
// fields carried by the error; non-error statuses return nil. Shared by
// refresh and Poll so both map the same way.
func statusError(status string, st *upstream.SessionState) error {
	switch status {
	case "banned":
		return &upstream.BanError{ResumesAt: st.ResumesAt, Body: st.Message}
	case "country_blocked":
		return &upstream.CountryBlockedError{
			CountryCode:        st.CountryCode,
			CountryBlockReason: st.CountryBlockReason,
			IpPrivacySignals:   st.IpPrivacySignals,
		}
	case "rate_limited", "spend_limited":
		retryAfter := upstream.CooldownFromMillis(float64(st.RetryAfterMs))
		if retryAfter <= 0 {
			retryAfter = time.Minute
		}
		return &upstream.RateLimitError{
			Status:      status,
			Model:       st.Model,
			RetryAfter:  retryAfter,
			ResetAt:     st.ResetAt,
			Limit:       st.Limit,
			RecentCount: st.RecentCount,
			// Window evidence survives the status path too (prod
			// 2026-09-16): a session status response reporting the freebucks
			// ceiling must not degrade to a plain rate limit on its way to
			// the cooldown memory, or /metrics and the payloads would lose
			// the distinction the body carried.
			WindowHours:        st.WindowHours,
			FreebucksShortfall: st.FreebucksShortfall,
			Body:               st.Message,
		}
	case "ip_capped":
		// Distinct error: ip_capped is admission-only (too many distinct
		// users on the egress IP) and NOT tied to a quota reset, so the
		// cooldown is bounded to retryAfterMs only — never the
		// Pacific-midnight lock (upstream/freebuff freebuff-session.ts).
		retryAfter := upstream.CooldownFromMillis(float64(st.RetryAfterMs))
		if retryAfter <= 0 {
			retryAfter = time.Minute
		}
		return &upstream.IpCappedError{
			ActiveUsersForIP: st.ActiveUsersForIP,
			Limit:            st.Limit,
			RetryAfter:       retryAfter,
			Body:             st.Message,
		}
	case "session_model_mismatch", "limited_ip":
		// The egress IP cannot serve the requested model. The session row is
		// fine (bound to its admitted model) — not session-invalid, so it
		// must never be invalidated/refreshed (re-admitting burns a daily
		// session slot). Non-limited messages keep today's exact error text.
		if strings.Contains(strings.ToLower(st.Message), "limited") {
			return &upstream.LimitedIpError{
				RetryAfter: upstream.CooldownFromMillis(float64(st.RetryAfterMs)),
				Body:       st.Message,
			}
		}
		return fmt.Errorf("session: unknown upstream status %q", status)
	case "consent_required":
		// 409 wallet-consent demand (vendor af898dc): the balance moved
		// since the spend limit was confirmed. Terminal for the request —
		// the CLI drops to the picker with retry:null (use-freebuff-session
		// .ts nextDelayMs) — so surface 409 with the confirm copy, never a
		// cooldown and never a retry.
		spend := 0.0
		if st.WalletConsent != nil {
			spend = st.WalletConsent.WalletSpend
		}
		return &upstream.UpstreamError{
			Status: http.StatusConflict,
			Body:   fmt.Sprintf("upstream balance changed: re-confirm %g wallet Freebucks to admit (consent_required)", spend),
		}
	case string(upstream.WireCodePurchaseClaimReleased), string(upstream.WireCodePurchaseInUse), string(upstream.WireCodePurchaseCapacity), string(upstream.WireCodePremiumSlotTaken):
		// Desktop purchase-flow admission shapes (vendor af898dc) and
		// premium-slot concurrency limit (premium_slot_taken): terminal
		// session failures (nextDelayMs returns null = stop polling). The
		// proxy runs no purchase flow and respects the single active
		// premium session bound, so surface the honest upstream
		// status with its message — no cooldown, no retry, no WireCode.
		code := st.HTTPStatus
		if code == 0 {
			code = http.StatusConflict
		}
		msg := st.Message
		if msg == "" {
			if status == string(upstream.WireCodePremiumSlotTaken) {
				msg = "upstream session admission refused: premium slot already active for this account (premium_slot_taken)"
			} else {
				msg = "upstream purchase flow blocked admission (" + status + ")"
			}
		}
		return &upstream.UpstreamError{Status: code, Body: msg}
	case "first_tab_discount_changed":
		// First-tab discount re-quote (vendor 6cd8970): the account-wide
		// offer moved mid-admission, so the quoted price is stale and no
		// Freebucks were charged. Terminal for the request — the CLI drops
		// to the picker with retry:null (use-freebuff-session.ts
		// nextDelayMs) showing FIRST_TAB_DISCOUNT_CHANGED_MESSAGE — so
		// surface that copy with no cooldown and no retry.
		code := st.HTTPStatus
		if code == 0 {
			code = http.StatusConflict
		}
		return &upstream.UpstreamError{
			Status: code,
			Body:   "Your first-tab discount changed. Review the model menu and choose again. No Freebucks were charged. (first_tab_discount_changed)",
		}
	}
	return nil
}

// noteAdmissionErr feeds admission-side terminal states into the client's
// tokenhealth memory (PORT-QUEUE P4): ban is enforced at the admission POST
// while the tokenhealth probes are GET-only, so without this feedback a GET
// probe reports OK for an account whose last admission returned banned
// (observed live: dashboard ban None while admission banned on both tokens
// and the pool quarantined both). Only terminal states stick (banned,
// country_blocked, auth-rejected); every other error leaves the memory
// untouched. Shared by refresh and Poll so both map the same way.
func (m *Manager) noteAdmissionErr(err error) {
	if err == nil || m.client == nil {
		return
	}
	switch {
	case errors.Is(err, upstream.ErrBanned):
		var be *upstream.BanError
		_ = errors.As(err, &be)
		hint := "banned at admission (terminal)"
		if be != nil && !be.ResumesAt.IsZero() {
			hint = "banned at admission (resumes at " + be.ResumesAt.Format(time.RFC3339) + ")"
		}
		m.client.NoteAdmissionTerminal(upstream.TokenBanned, hint)
	case errors.Is(err, upstream.ErrCountryBlocked):
		var cbe *upstream.CountryBlockedError
		_ = errors.As(err, &cbe)
		hint := "country_blocked at admission (terminal for this account+egress)"
		if cbe != nil && cbe.CountryCode != "" {
			hint = "country_blocked at admission (country " + cbe.CountryCode + "; terminal for this account+egress)"
		}
		m.client.NoteAdmissionTerminal(upstream.TokenCountryBlocked, hint)
	case errors.Is(err, upstream.ErrAuthRejected):
		m.client.NoteAdmissionTerminal(upstream.TokenInvalid, "token rejected at admission (HTTP 401)")
	}
}

// noteAdmissionStatus feeds a terminal admission status string into the
// client's tokenhealth memory (same P4 feedback as noteAdmissionErr, for the
// refresh path that switches on the raw status before building the typed
// error). Non-terminal statuses leave the memory untouched.
func (m *Manager) noteAdmissionStatus(status string, st *upstream.SessionState) {
	if m.client == nil {
		return
	}
	switch status {
	case "banned":
		hint := "banned at admission (terminal)"
		if st != nil && !st.ResumesAt.IsZero() {
			hint = "banned at admission (resumes at " + st.ResumesAt.Format(time.RFC3339) + ")"
		}
		m.client.NoteAdmissionTerminal(upstream.TokenBanned, hint)
	case "country_blocked":
		hint := "country_blocked at admission (terminal for this account+egress)"
		if st != nil && st.CountryCode != "" {
			hint = "country_blocked at admission (country " + st.CountryCode + "; terminal for this account+egress)"
		}
		m.client.NoteAdmissionTerminal(upstream.TokenCountryBlocked, hint)
	}
}

// MergeCompactSnapshot is the compact-merge carry (PORT-QUEUE
// P2, vendor freebuff-session-api.ts mergeCompactActiveSession): a compact
// poll omits quota fields already returned by admission, so the previous
// snapshot's values carry across — otherwise quota displays go stale until
// the next full poll. Fresh compact values win when present
// (vendor `next ?? current`).
//
// keepCompact=false mirrors the vendor's null return: the sessions are not
// the same active slot (status/instance/model mismatch), so the caller must
// fetch one full response before compacting again instead of carrying
// another session's meter forward. Single implementation — pool delegates
// via mergeCompactSessionSnapshot (pool imports session, not the reverse).
func MergeCompactSnapshot(current, next SessionSnapshot) (merged SessionSnapshot, keepCompact bool) {
	if current.Status != "active" || next.Status != "active" ||
		current.InstanceID != next.InstanceID ||
		current.Model != next.Model {
		return SessionSnapshot{}, false
	}
	merged = next
	if len(merged.QuotaByModel) == 0 {
		merged.QuotaByModel = current.QuotaByModel
	}
	if merged.SubscriptionTierID == "" {
		merged.SubscriptionTierID = current.SubscriptionTierID
	}
	if merged.Freebucks == nil {
		merged.Freebucks = current.Freebucks
	}
	return merged, true
}

// quotaSnapshotFromWire converts a wire quota map to the snapshot view for
// the compact merge; quotaWireFromSnapshot converts back for the commit.
// Empty stays nil so the commit-time saved restores keep their shape.
func quotaSnapshotFromWire(in map[string]upstream.ModelQuota) map[string]QuotaSnapshot {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]QuotaSnapshot, len(in))
	for id, q := range in {
		out[id] = QuotaSnapshot{
			Model:       q.Model,
			Limit:       q.Limit,
			RecentCount: q.RecentCount,
			ResetAt:     q.ResetAt,
			Period:      q.Period,
			Pool:        q.Pool,
			PoolLabel:   q.PoolLabel,
			Entitlement: q.Entitlement,
		}
	}
	return out
}

func quotaWireFromSnapshot(in map[string]QuotaSnapshot) map[string]upstream.ModelQuota {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]upstream.ModelQuota, len(in))
	for id, q := range in {
		out[id] = upstream.ModelQuota{
			Model:       q.Model,
			Limit:       q.Limit,
			RecentCount: q.RecentCount,
			ResetAt:     q.ResetAt,
			Period:      q.Period,
			Pool:        q.Pool,
			PoolLabel:   q.PoolLabel,
			Entitlement: q.Entitlement,
		}
	}
	return out
}

// pollPersisted attempts to resume a persisted session before a fresh create
// (restart reuse). requestedModel is the model the caller is about to create
// for: a persisted slot bound to a different model is dropped instead of
// adopted, so a model-mismatch refresh falls through to a create for the
// requested model.
//
// It returns a non-nil SessionState when the persisted slot is still active
// upstream and model-compatible (adopted), and (nil, nil) otherwise — either
// there is no persisted slot, it is expired, it is model-incompatible, or it
// is dead upstream (in which case it is removed from the store). Transport
// errors are returned so the caller surfaces them like a failed create
// instead of burning a fresh daily session slot on a merely-flaky upstream.
func (m *Manager) pollPersisted(ctx context.Context, requestedModel string) (*upstream.SessionState, error) {
	if m.store == nil || m.key == "" {
		return nil, nil
	}
	cs := m.store.Load(m.key)
	if cs == nil || cs.status != "active" || cs.instanceID == "" {
		return nil, nil
	}
	// Only resume a slot that is still genuinely active (with the 5s safety
	// margin). An expired-but-in-grace slot is draining; resume it is not
	// worth the risk of admitting new work onto a dying session.
	if cs.expiresAt.IsZero() || !time.Now().Before(cs.expiresAt.Add(-expiryMargin)) {
		m.store.Remove(m.key, cs.instanceID)
		return nil, nil
	}
	// Model gate (pre-flight): a persisted slot known to be bound to a
	// different model must never be re-adopted for another model — that
	// would pin the old model's session forever on every refresh.
	if cs.model != "" && requestedModel != "" && cs.model != requestedModel {
		m.store.Remove(m.key, cs.instanceID)
		return nil, nil
	}

	st, err := m.client.GetSession(ctx, cs.instanceID)
	if err != nil {
		// 428 waiting_room_required is session-ENDING (endsTheSession:true
		// — the seat is gone, same as the live Poll/refresh drop paths
		// #116/#140): drop the dead persisted row so the next iteration
		// re-admits fresh instead of re-polling it forever.
		if errors.Is(err, upstream.ErrWaitingRoomRequired) {
			m.store.Remove(m.key, cs.instanceID)
		}
		// Transport error: surface it instead of swallowing and falling
		// through to a fresh create. The caller retries (single-flight /
		// TRANSIENT_RETRIES); a create here would burn a session slot.
		return nil, err
	}
	switch st.Status {
	case "active":
		// Model gate (post-flight): the upstream may have bound the resumed
		// slot to a different model than requested. Adopt only when the
		// resumed model is compatible; otherwise drop the slot and fall
		// through to a create for the requested model.
		if st.Model != "" && requestedModel != "" && st.Model != requestedModel {
			m.store.Remove(m.key, st.InstanceID)
			return nil, nil
		}
		resumedModel := st.Model
		if resumedModel == "" {
			resumedModel = requestedModel
		}
		slog.Debug("session resumed from store", "instance_id", st.InstanceID, "model", resumedModel, "status", "active", "expires_at", st.ExpiresAt.Format(time.RFC3339))
		return st, nil
	case "ended", "superseded", "none", "banned":
		m.store.Remove(m.key, st.InstanceID)
		return nil, nil
	case "country_blocked", "rate_limited", "ip_capped", "spend_limited":
		// Terminal admission refusals: the persisted slot is dead upstream;
		// drop it so a restart re-admits from scratch instead of re-polling.
		m.store.Remove(m.key, st.InstanceID)
		return nil, nil
	default:
		// queued or an unknown status: not resumable as an active slot, but
		// non-terminal — keep the entry for a later restart.
		return nil, nil
	}
}

// Poll runs the periodic session-liveness poll: a compact GET with NO
// heartbeat header — the CLI never beats (x-freebuff-heartbeat is
// Desktop-only, upstream/freebuff freebuff-models.ts:1212-1215); liveness
// comes from the recurring compact GET itself. It refreshes the
// cached state the way the CLI's 30s compact poll does: statusError
// mappings, drop-on-ban, invalidate on superseded/none, and — within the
// 30-minute grace drain — an ended response that still carries the instance
// id is kept as a usable "ended" row instead of being invalidated.
func (m *Manager) Poll(ctx context.Context) error {
	m.mu.Lock()
	if m.state == nil || (m.state.status != "active" && m.state.status != "ended") || m.state.instanceID == "" {
		m.mu.Unlock()
		return nil
	}
	// Issue #60: admission probe caching — within the probe TTL of the last
	// successful session response the cached state is authoritative, so the
	// poll GET is redundant; skip it (less upstream traffic, fewer chances
	// to trip the one-client-at-a-time gate).
	if m.probeTTL > 0 && !m.lastAdmitted.IsZero() && time.Since(m.lastAdmitted) < m.probeTTL {
		m.mu.Unlock()
		return nil
	}
	instanceID := m.state.instanceID
	// polledModel joins every poll-path line below to the session's model:
	// the poll GET carries only the instance id, so without this snapshot
	// the drop/park/heartbeat lines cannot name their model.
	polledModel := m.state.model
	// P2: a declined compact merge forces one full (non-compact) poll before
	// resuming compact polls. Consumed once: the flag clears even when the
	// GET below fails, so a flaky upstream cannot pin every poll to full.
	wasFullPoll := m.forceFullPoll
	m.forceFullPoll = false
	m.mu.Unlock()

	start := time.Now()
	st, err := m.client.GetSessionWithOpts(ctx, instanceID, !wasFullPoll)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		// #116: 428 waiting_room_required is session-ENDING
		// (endsTheSession:true per FREEBUFF_GATE_CODES — the seat is gone;
		// upstream/freebuff freebuff-session.ts). Drop the cached admission
		// so the next EnsureSession re-admits fresh (the pool's
		// WAITING_ROOM_CHAIN fires before the create). Any other poll error
		// is left for the pool's failure backoff.
		if errors.Is(err, upstream.ErrWaitingRoomRequired) {
			dropped := false
			m.mu.Lock()
			if m.state != nil && m.state.instanceID == instanceID {
				m.commit(nil)
				dropped = true
			}
			m.mu.Unlock()
			if dropped {
				m.recordInvalidation(reasonPoll)
				slog.Warn("session dropped during poll", "reason", reasonPoll, "status", "waiting_room_required", "instance_id", instanceID, "model", polledModel)
			}
		}
		// P4: a poll GET can itself observe the terminal state (e.g. 403
		// banned on a dead slot) — feed it back to tokenhealth the same way
		// admission does. Non-terminal errors leave the memory untouched.
		m.noteAdmissionErr(err)
		// Hold: any other poll GET error keeps the slot — the row stays
		// cached (never commit(nil)/Invalidate/ClearQueued/DELETE) and the
		// consecutive-failure count paces the re-GET with the vendor
		// failedPollDelayMs shape (20s doubling, 5m cap, Retry-After
		// floor). The pool's own failure backoff schedules the actual
		// wait; the count here keeps the session layer's view in step
		// and visible in logs.
		m.mu.Lock()
		m.pollFailures++
		failures := m.pollFailures
		m.mu.Unlock()
		slog.Debug("session parked during poll", "instance_id", instanceID, "model", polledModel, "failures", failures,
			"backoff_ms", pollBackoff(failures, pollRetryAfter(err)).Milliseconds(), "err", err)
		return err
	}
	m.mu.Lock()
	// A successful GET confirms the cached state: refresh the probe window
	// and reset the transient-failure count (transport is healthy; any
	// status mapping below is a typed refusal, not a poll failure).
	m.pollFailures = 0
	m.lastAdmitted = time.Now()
	// Refund-pending replay holder: a refund receipt can land on a compact
	// poll GET between polls (the ended body carries freebucksRefund /
	// freebucksRefundPending exactly like a DELETE receipt — the parse is
	// shared). A pending receipt parks the polled instance so a later
	// EndSession/RefreshRefund replays its DELETE with the same instance id
	// (vendor af898dc replay semantics); a settled amount records lastRefund
	// so the balance is never mis-stated, clearing only a matching pending
	// entry (a newer release may have superseded it). Memory-only like the
	// CLI zustand store (freebuff-session-store.ts refreshRefund): the
	// pending entry settles via immediate replay, not across restarts, so a
	// persisted stale instance id would only buy a tolerated-404 on boot.
	if st.FreebucksRefundPending && instanceID != "" {
		m.pendingRefund = instanceID
	} else if st.FreebucksRefund != nil {
		m.lastRefund = st.FreebucksRefund
		if m.pendingRefund == instanceID {
			m.pendingRefund = ""
		}
	}
	m.mu.Unlock()
	if serr := statusError(st.Status, st); serr != nil {
		// P4: feed a terminal poll status (banned, country_blocked) back to
		// tokenhealth so the Tokens view reflects poll reality too.
		m.noteAdmissionErr(serr)
		// A banned session is dead until the account unban: drop the cached
		// admission (cooldown) so the token re-admits only after the pool's
		// ban window, instead of polling a stale slot.
		if st.Status == "banned" {
			dropped := false
			m.mu.Lock()
			if m.state != nil && m.state.instanceID == instanceID {
				m.commit(nil)
				dropped = true
			}
			m.mu.Unlock()
			if dropped {
				m.recordInvalidation(reasonPoll)
				slog.Warn("session dropped during poll", "reason", reasonPoll, "status", st.Status, "instance_id", instanceID, "model", polledModel)
			}
		}
		return serr
	}
	if st.Status == "superseded" || st.Status == "none" {
		dropped := false
		m.mu.Lock()
		if m.state != nil && m.state.instanceID == instanceID {
			m.commit(nil)
			dropped = true
		}
		m.mu.Unlock()
		if dropped {
			m.recordInvalidation(tableReason(st.Status))
			slog.Warn("session ended during poll", "reason", tableReason(st.Status), "status", st.Status, "instance_id", instanceID, "model", polledModel)
		}
		return nil
	}
	if st.Status == "ended" {
		// Ended WITH the instance id still present: the row is in the 30-min
		// grace drain and stays usable. Refresh the cached state
		// as ended-with-instance so the fast path keeps serving it until
		// grace closes; the pool keeps polling. The grace end comes from the
		// response when present, else expiresAt + graceWindow.
		graceEnd := graceEndFromState(st.ExpiresAt, st.GracePeriodEndsAt)
		if st.InstanceID != "" && !graceEnd.IsZero() && time.Now().Before(graceEnd) {
			m.mu.Lock()
			if m.state != nil && m.state.instanceID == instanceID {
				m.commit(&cachedState{
					status:            "ended",
					instanceID:        st.InstanceID,
					model:             m.state.model,
					expiresAt:         st.ExpiresAt,
					gracePeriodEndsAt: graceEnd,
				})
				slog.Debug("session in grace drain during poll", "instance_id", instanceID, "model", m.state.model, "status", st.Status, "grace_ends_at", graceEnd.Format(time.RFC3339))
			}
			m.mu.Unlock()
			return nil
		}
		// The row is gone (no instance id) or past grace: drop it so the
		// next EnsureSession re-creates a fresh session.
		dropped := false
		m.mu.Lock()
		if m.state != nil && m.state.instanceID == instanceID {
			m.commit(nil)
			dropped = true
		}
		m.mu.Unlock()
		if dropped {
			m.recordInvalidation(tableReason(st.Status))
			slog.Warn("session ended during poll", "reason", tableReason(st.Status), "status", st.Status, "instance_id", instanceID, "model", polledModel)
		}
		return nil
	}
	// P2 compact-merge carry (vendor freebuff-session-api.ts
	// mergeCompactActiveSession; pool-side port in pool/compact_merge.go): a
	// compact poll omits quota fields already returned by admission, so the
	// cached admission values carry across — otherwise quota displays go
	// stale until the next full poll while a compact poll that DID carry
	// fresh quota would be discarded. Fresh compact values win when present.
	// A slot mismatch declines the merge and forces one full poll before
	// resuming compact polls instead of carrying another session's meter
	// forward; a forced-full response is authoritative and commits directly.
	// P3's refund fold-in above stays untouched.
	var quota map[string]upstream.ModelQuota
	var tier string
	var fb *upstream.FreebucksInfo
	if !wasFullPoll {
		current := m.Snapshot()
		nextModel := st.Model
		if nextModel == "" {
			// The poll GET carries only the instance id, so an omitted
			// model is "not reported", never a different slot (see
			// polledModel above).
			nextModel = current.Model
		}
		next := SessionSnapshot{
			Status:             st.Status,
			InstanceID:         st.InstanceID,
			Model:              nextModel,
			QuotaByModel:       quotaSnapshotFromWire(st.RateLimitsByModel),
			SubscriptionTierID: st.SubscriptionTierID,
			Freebucks:          st.Freebucks,
		}
		merged, keep := MergeCompactSnapshot(current, next)
		if !keep {
			m.mu.Lock()
			// A concurrent refresh may have replaced the row mid-poll:
			// only a mismatch against the still-polled row refetches.
			if m.state != nil && m.state.instanceID == instanceID {
				m.forceFullPoll = true
			}
			m.mu.Unlock()
			slog.Debug("session: compact poll declined, forcing full poll", "instance_id", shortInstance(instanceID), "model", polledModel, "status", st.Status)
			return nil
		}
		quota = quotaWireFromSnapshot(merged.QuotaByModel)
		tier = merged.SubscriptionTierID
		fb = merged.Freebucks
	} else {
		quota = st.RateLimitsByModel
		tier = st.SubscriptionTierID
		fb = st.Freebucks
	}
	m.mu.Lock()
	if m.state != nil && m.state.instanceID == instanceID && (st.InstanceID == "" || st.InstanceID == instanceID) {
		// Copy-then-overwrite: fields the poll body omits keep their cached
		// admission values; fresh poll values win when present.
		cs := *m.state
		cs.status = "active"
		if st.InstanceID != "" {
			cs.instanceID = st.InstanceID
		}
		if !st.ExpiresAt.IsZero() {
			cs.expiresAt = st.ExpiresAt
			cs.gracePeriodEndsAt = graceEndFromState(st.ExpiresAt, st.GracePeriodEndsAt)
		}
		if st.CountryCode != "" {
			cs.countryCode = st.CountryCode
			cs.countryBlockReason = st.CountryBlockReason
		}
		if len(st.IpPrivacySignals) > 0 {
			cs.ipPrivacySignals = st.IpPrivacySignals
		}
		if st.ActiveUsersForIP != 0 {
			cs.activeUsersForIP = st.ActiveUsersForIP
		}
		if st.Limit != 0 {
			cs.limit = st.Limit
		}
		if st.GlmPromo != "" {
			cs.glmPromo = st.GlmPromo
		}
		if st.Standing != nil {
			cs.standing = st.Standing
		}
		if st.RemainingMs != 0 {
			cs.remainingMs = st.RemainingMs
		}
		if st.Referral != nil {
			cs.referral = st.Referral
		}
		if st.UpgradeHint != nil {
			cs.upgradeHint = st.UpgradeHint
		}
		if st.Message != "" {
			cs.serverMessage = st.Message
		}
		if st.AccessTier != "" {
			cs.accessTier = st.AccessTier
		}
		if len(st.LimitedModelOffers) > 0 {
			cs.limitedModelOffers = st.LimitedModelOffers
		}
		if st.LimitedOfferReason != "" {
			cs.limitedOfferReason = st.LimitedOfferReason
		}
		cs.quotaByModel = quota
		cs.freebucks = fb
		cs.subscriptionTierID = tier
		m.commit(&cs)
	}
	m.mu.Unlock()
	// A live active poll proves the account reachable: drop any remembered
	// admission terminal (mirrors the pool clearing its hint on live
	// admission).
	m.client.ClearAdmissionTerminal()
	// Heartbeat liveness confirmed: the compact poll returned a usable
	// status (active). instance/ms/status standardize the heartbeat poll
	// line so ops can see each liveness beat and its latency.
	slog.Debug("session: heartbeat poll", "instance_id", shortInstance(instanceID), "model", polledModel, "ms", ms, "status", st.Status)
	return nil
}

// pollBackoffBase / pollBackoffMax mirror the vendor poll pacing
// (failedPollDelayMs 20s doubling, 5m cap): the wait before the next re-GET
// after failures consecutive transient failures, with equal jitter over the
// lower half of the window and never before the server's Retry-After floor.
// Hardcoded vendor shape (no knobs); the pool carries the twin for its own
// failure backoff (see pool_lifecycle.go).
const (
	pollBackoffBase = 20 * time.Second
	pollBackoffMax  = 5 * time.Minute
)

// pollBackoff returns the vendor-shaped wait before the next re-GET after
// failures consecutive transient failures.
func pollBackoff(failures int, retryAfter time.Duration) time.Duration {
	if failures < 1 {
		failures = 1
	}
	d := pollBackoffBase << min(failures-1, 5)
	if d > pollBackoffMax {
		d = pollBackoffMax
	}
	d = d/2 + time.Duration(pollRand()%uint64(d/2))
	if retryAfter > 0 {
		if retryAfter < 5*time.Nanosecond {
			retryAfter = 5 * time.Nanosecond
		}
		ra := retryAfter - retryAfter/5 + time.Duration(pollRand()%uint64(2*retryAfter/5))
		if ra > d {
			d = ra
		}
		if d > pollBackoffMax {
			d = pollBackoffMax
		}
	}
	return d
}

// pollRetryAfter extracts the server's Retry-After floor from a failed poll
// error (0 when the error carries none).
func pollRetryAfter(err error) time.Duration {
	var ue *upstream.UpstreamError
	if errors.As(err, &ue) {
		return ue.RetryAfter
	}
	var rle *upstream.RateLimitError
	if errors.As(err, &rle) {
		return rle.RetryAfter
	}
	var ice *upstream.IpCappedError
	if errors.As(err, &ice) {
		return ice.RetryAfter
	}
	var lie *upstream.LimitedIpError
	if errors.As(err, &lie) {
		return lie.RetryAfter
	}
	var wrr *upstream.WaitingRoomRequiredError
	if errors.As(err, &wrr) {
		return wrr.RetryAfter
	}
	var uwr *upstream.WaitingRoomError
	if errors.As(err, &uwr) {
		return uwr.RetryAfter
	}
	return 0
}

// pollRand draws one uint64 from crypto/rand (the jitter source). A read
// failure falls back to the clock rather than panicking in a serving path.
func pollRand() uint64 {
	var b [8]byte
	if _, err := cryptoRand.Read(b[:]); err != nil {
		return uint64(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint64(b[:])
}
