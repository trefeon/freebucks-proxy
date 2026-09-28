// session_claim.go — CLI-parity purchase-claim lifecycle (G4) plus the
// same-claim admission retry (G5).
//
// The CLI mints one claim id (cli:<uuid>) per lifetime, re-POSTs the same
// claim on every admission/rejoin, and rotates (new UUID) only after an
// explicit DELETE of the old row or a dead-claim terminal
// (use-freebuff-session.ts:576 newFreebuffCliInstanceId at startup,
// :1148-1203 restart DELETE-before-rotate). The proxy previously minted a
// fresh cli:<uuid> per admission inside the upstream client, so every rejoin
// was a new purchase identity — the self-supersede churn behind the live
// 503 → 409 refund loops.
//
// The manager holds the claim (claimID, guarded by mu) and persists it
// through the store (SaveClaim/LoadClaim), where it outlives the session
// row like the refund tracking: invalidation downgrades to a claim-only
// entry, and a restart rejoins on the same claim (the relaunch handoff).
//
// Wire: the manager's claim rides every admission POST via upstream
// Client.CreateSessionForModelWithClaim (as x-freebuff-instance-id with the
// multi-session attempt headers when cli:-prefixed, empty claim falling
// back to a fresh mint) — so retries, rejoins and re-admits all carry the
// same identity until rotateClaim swaps it.
package session

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"freebucks-proxy/backend/internal/upstream"
)

// claimPrefix marks a manager-minted purchase claim, matching the CLI's
// newFreebuffCliInstanceId (cli/src/utils/freebuff-session-identity.ts) and
// the upstream client's generateCliInstanceID wire shape.
const claimPrefix = "cli:"

// admissionMaxRetries bounds the same-claim admission POST retries (G5):
// the initial POST plus two retries, mirroring the vendor's 20s-base
// backoff (polling-backoff.ts:15-44, failedPollDelayMs 20s doubling, 5m
// cap) before the refusal surfaces to the pool's failover.
const admissionMaxRetries = 2

// admissionRetryBackoff / admissionRetrySleep are the G5 test-scale seams:
// backoff shapes the wait (default pollBackoff — 20s doubling, 5m cap,
// Retry-After floor), sleep waits interruptibly. Tests override both so
// retry loops run without real 20s stalls.
var (
	admissionRetryBackoff = pollBackoff
	admissionRetrySleep   = func(ctx context.Context, d time.Duration) error {
		if d <= 0 {
			return ctx.Err()
		}
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
)

// newClaimID mints one cli:<uuid> purchase claim (RFC 4122 v4, same shape
// as upstream's generateCliInstanceID, which lives in another package and
// cannot be reused here).
func newClaimID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf(claimPrefix+"%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// ensureClaim returns the manager's stable purchase claim, loading it from
// the store on first use and minting + persisting when absent. A mint
// failure degrades to "" (the admission still POSTs — the upstream mints
// per-call until the WithClaim follow-up lands — and the next admission
// retries the mint).
func (m *Manager) ensureClaim() string {
	m.mu.Lock()
	if m.claimID != "" {
		c := m.claimID
		m.mu.Unlock()
		return c
	}
	var loaded string
	if !m.claimLoaded && m.store != nil && m.key != "" {
		loaded = m.store.LoadClaim(m.key)
	}
	m.claimLoaded = true
	if loaded != "" {
		m.claimID = loaded
		c := m.claimID
		m.mu.Unlock()
		return c
	}
	id, err := newClaimID()
	if err != nil {
		m.mu.Unlock()
		slog.Warn("session: purchase-claim mint failed, admitting without a stable claim", "err", err)
		return ""
	}
	m.claimID = id
	c := m.claimID
	m.mu.Unlock()
	m.persistClaim(c)
	return c
}

// rotateClaim swaps in a fresh purchase claim after an explicit upstream
// DELETE of the old row or a dead-claim terminal (superseded,
// purchase_claim_released — the CLI's claimRetired single-use rule). A
// mint failure keeps the old claim rather than stranding the manager
// claimless. The store write runs unlocked (snapshot pattern, same as
// persistRefundSnapshot); memory stays authoritative either way.
func (m *Manager) rotateClaim(reason string) {
	id, err := newClaimID()
	if err != nil {
		slog.Warn("session: purchase-claim rotation failed, keeping old claim", "reason", reason, "err", err)
		return
	}
	m.mu.Lock()
	old := m.claimID
	m.claimID = id
	m.claimLoaded = true
	m.mu.Unlock()
	m.persistClaim(id)
	slog.Debug("session: purchase claim rotated", "reason", reason, "old", shortInstance(old), "new", shortInstance(id))
}

// persistClaim writes the claim to the store without holding m.mu across
// the call. A nil store is a no-op.
func (m *Manager) persistClaim(claim string) {
	m.mu.Lock()
	store, key := m.store, m.key
	m.mu.Unlock()
	if store == nil || key == "" {
		return
	}
	store.SaveClaim(key, claim)
}

// ClaimIDForTest reports the manager's current purchase claim ("" when none
// minted yet). Test hook for the G4 rotation pins.
func (m *Manager) ClaimIDForTest() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.claimID
}

// admissionRetryable reports whether an admission POST failure may be
// retried on the SAME claim with backoff (G5, mirroring
// classifyFreebuffSessionRequestFailure for POST: 408/429/503 edge and
// admission-shed refusals are produced before the session mutation can
// commit, so retrying cannot repeat a successful purchase):
//
//   - WaitingRoomError: any 503 plus the 429 waiting_room_queued admission
//     race (endsTheSession:false — the row is fine, re-POST the claim).
//   - CapacityDeferredError: the free-tier capacity queue ("retried
//     automatically" — same-claim re-POST, never a cooldown).
//   - Retryable UpstreamError (e.g. deployment_outside_hours) and 408:
//     temporarily unavailable, worth one more claim round.
//
// Everything else surfaces immediately: typed 429s (rate_limited,
// spend_limited, ip_capped) carry token cooldowns the pool owns; 428
// waiting_room_required needs the pool's ad-chain before any re-POST;
// transport failures have unknown disposition (the POST may have committed
// — retrying could double-purchase); terminals never retry.
func admissionRetryable(err error) bool {
	var wr *upstream.WaitingRoomError
	if errors.As(err, &wr) {
		return true
	}
	var cde *upstream.CapacityDeferredError
	if errors.As(err, &cde) {
		return true
	}
	var ue *upstream.UpstreamError
	if errors.As(err, &ue) && (ue.Retryable || ue.Status == http.StatusRequestTimeout) {
		return true
	}
	return false
}
