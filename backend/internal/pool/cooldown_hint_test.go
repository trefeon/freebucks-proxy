package pool

// cooldown_hint_test.go — pins the runtime-plane DB-hint contract
// (data-architecture "Changes proposed" §2):
//
//   - terminal classifications (ban/country-block) persist a timestamped
//     hint row; 429/ip_capped/limited_ip/burst persist NOTHING;
//   - missing reads as eligible, expired reads as absent (and converges);
//   - hints never fail Acquire alone (all-hinted walks attempt upstream
//     live) but skip one doomed probe when an alternative serves;
//   - bridge idle-eviction survivors are bounded, expiring, SHA-keyed and
//     fold into the usage accounting;
//   - the pool/probe/quota/* namespace is retired (sessions_persist owns
//     quota): the flush stages nothing there and drains legacy rows.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestCooldownHintTerminalWritesRow pins the terminal-only writer: ban and
// country-block classifications persist a {kind,until_ms} row under the
// token's SHA hash (never the raw token); a hard ban persists permanent
// (until_ms 0).
func TestCooldownHintTerminalWritesRow(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mem := newMemPoolPersist()
	p := newTestPool(t, mock0, mock1)
	p.SetPoolPersist(mem)

	p.CooldownTokenBan(0, &upstream.BanError{Body: "banned"})
	key0 := poolCooldownKey(poolTokenHash("tok-0"))
	raw, ok, err := mem.LoadPoolState(key0)
	if err != nil || !ok {
		t.Fatalf("ban hint row present = %v,%v, want true,nil", ok, err)
	}
	var b poolCooldownBlob
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("unmarshal ban hint: %v", err)
	}
	if b.Kind != cooldownHintKindBan {
		t.Errorf("ban hint kind = %q, want %q", b.Kind, cooldownHintKindBan)
	}
	if b.UntilMs != 0 {
		t.Errorf("hard-ban hint until_ms = %d, want 0 (permanent)", b.UntilMs)
	}
	if strings.Contains(string(raw), "tok-0") {
		t.Errorf("raw token leaked into ban hint row %q", key0)
	}

	p.CooldownTokenCountryBlocked(1, &upstream.CountryBlockedError{CountryCode: "XX"})
	key1 := poolCooldownKey(poolTokenHash("tok-1"))
	raw, ok, err = mem.LoadPoolState(key1)
	if err != nil || !ok {
		t.Fatalf("country hint row present = %v,%v, want true,nil", ok, err)
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("unmarshal country hint: %v", err)
	}
	if b.Kind != cooldownHintKindCountryBlocked {
		t.Errorf("country hint kind = %q, want %q", b.Kind, cooldownHintKindCountryBlocked)
	}
	if b.UntilMs <= time.Now().UnixMilli() {
		t.Errorf("country hint until_ms = %d, want a future window", b.UntilMs)
	}
	if strings.Contains(string(raw), "tok-1") {
		t.Errorf("raw token leaked into country hint row %q", key1)
	}
}

// TestCooldownHintMissingAndExpiredEligible pins the reader contract:
// missing reads as eligible, expired reads as absent (lazily in memory,
// converged on restore); corrupt and unknown-kind rows converge the same
// way instead of ever reading fresh.
func TestCooldownHintMissingAndExpiredEligible(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mem := newMemPoolPersist()
	p := newTestPool(t, mock)
	p.SetPoolPersist(mem)
	hash := poolTokenHash("tok-0")
	if p.cooldownHintFresh(hash, time.Now()) {
		t.Fatal("missing hint reads fresh, want eligible")
	}

	past := time.Now().Add(-time.Hour).UnixMilli()
	expiredKey := poolCooldownKey(hash)
	if err := mem.SavePoolState(expiredKey, mustMarshalPool(poolCooldownBlob{Kind: cooldownHintKindBan, UntilMs: past})); err != nil {
		t.Fatalf("seed expired hint: %v", err)
	}
	corruptKey := poolCooldownKey(poolTokenHash("tok-corrupt"))
	if err := mem.SavePoolState(corruptKey, []byte(`{not-json`)); err != nil {
		t.Fatalf("seed corrupt hint: %v", err)
	}
	kindKey := poolCooldownKey(poolTokenHash("tok-429"))
	if err := mem.SavePoolState(kindKey, mustMarshalPool(poolCooldownBlob{Kind: "rate_limited", UntilMs: time.Now().Add(time.Hour).UnixMilli()})); err != nil {
		t.Fatalf("seed unknown-kind hint: %v", err)
	}

	p2 := newTestPool(t, mock)
	p2.SetPoolPersist(mem)
	p2.RestorePoolPersist()
	if p2.cooldownHintFresh(hash, time.Now()) {
		t.Fatal("expired hint reads fresh after restore, want absent")
	}
	for _, key := range []string{expiredKey, corruptKey, kindKey} {
		if _, ok, _ := mem.LoadPoolState(key); ok {
			t.Errorf("hint row %q survives restore, want converged delete", key)
		}
	}
}

// TestCooldownHintNoRowsForRateLimitAndIpCapped pins the forbidden set: a
// direct 429 memory cooldown and a walk-observed ip_capped refusal (the
// per-IP wall) persist NO cooldown rows, even across a flush. Persisting
// either would spread one IP's limit to all accounts.
func TestCooldownHintNoRowsForRateLimitAndIpCapped(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"status":"ip_capped","activeUsersForIp":7,"limit":4,"retryAfterMs":45000}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ended"}`)
	}
	mem := newMemPoolPersist()
	p := newTestPool(t, mock)
	p.SetPoolPersist(mem)

	// Walk-observed ip_capped first (a live 429 memory cooldown would mask
	// the admission path behind its skip branch).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := p.Acquire(ctx, modelA)
	var ice *upstream.IpCappedError
	if !errors.As(err, &ice) {
		t.Fatalf("want *upstream.IpCappedError, got %v", err)
	}

	// Direct 429 memory cooldown: remembered live, never persisted.
	p.CooldownTokenRateLimit(0, &upstream.RateLimitError{Body: "rate limit", RetryAfter: 10 * time.Minute})

	p.markPersistDirty()
	if err := p.FlushPoolPersist(); err != nil {
		t.Fatalf("FlushPoolPersist: %v", err)
	}
	for _, k := range mem.keys() {
		if strings.HasPrefix(k, poolCooldownPrefix) {
			t.Fatalf("cooldown hint row %q after 429/ip_capped, want none", k)
		}
	}
}

// TestCooldownHintNeverAuthoritative pins the hint discipline: a pool whose
// every ordered token carries a fresh hint still attempts upstream live —
// the hint alone never fails Acquire. Live success then clears the hint.
func TestCooldownHintNeverAuthoritative(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mem := newMemPoolPersist()
	// Seed a fresh terminal hint directly: no memory quarantine/ban, so
	// only the hint could ever skip the token.
	hash := poolTokenHash("tok-0")
	future := time.Now().Add(time.Hour).UnixMilli()
	if err := mem.SavePoolState(poolCooldownKey(hash), mustMarshalPool(poolCooldownBlob{Kind: cooldownHintKindBan, UntilMs: future})); err != nil {
		t.Fatalf("seed hint: %v", err)
	}
	p := newTestPool(t, mock)
	p.SetPoolPersist(mem)
	p.RestorePoolPersist()
	if !p.cooldownHintFresh(hash, time.Now()) {
		t.Fatal("seeded hint not fresh after restore")
	}
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("all-hinted Acquire = %v, want a live attempt (success)", err)
	}
	p.LeaseRelease(lease)
	if got := mock.SessionCreatesSnapshot(); got == 0 {
		t.Fatal("all-hinted Acquire probed upstream 0 times, want >= 1 (hint is not authoritative)")
	}
	if p.cooldownHintFresh(hash, time.Now()) {
		t.Fatal("hint still fresh after live admission, want cleared")
	}
	if _, ok, _ := mem.LoadPoolState(poolCooldownKey(hash)); ok {
		t.Fatal("hint row survives live admission, want cleared")
	}
}

// TestCooldownHintSkipsDoomedProbe pins the hint's value: with a healthy
// alternative serving, the fresh-hinted token takes no upstream contact.
func TestCooldownHintSkipsDoomedProbe(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.Ban = true // would 403 on contact
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mem := newMemPoolPersist()
	if err := mem.SavePoolState(poolCooldownKey(poolTokenHash("tok-0")), mustMarshalPool(poolCooldownBlob{Kind: cooldownHintKindBan})); err != nil {
		t.Fatalf("seed hint: %v", err)
	}
	p := newTestPool(t, mock0, mock1)
	p.SetPoolPersist(mem)
	p.RestorePoolPersist()
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("Acquire with hinted token 0 = %v, want a lease on token 1", err)
	}
	defer p.LeaseRelease(lease)
	if lease.Token != 1 {
		t.Fatalf("lease token = %d, want 1 (hinted token 0 skipped)", lease.Token)
	}
	if got := mock0.SessionCreatesSnapshot(); got != 0 {
		t.Fatalf("hinted token session creates = %d, want 0 (doomed probe skipped)", got)
	}
}

// TestCooldownHintClearedByUnlock pins operator recovery: UnlockToken drops
// the terminal hint from memory and the row, so an appealed account
// re-admits immediately (also after a restart).
func TestCooldownHintClearedByUnlock(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mem := newMemPoolPersist()
	p := newTestPool(t, mock)
	p.SetPoolPersist(mem)
	p.CooldownTokenBan(0, &upstream.BanError{Body: "banned"})
	hash := poolTokenHash("tok-0")
	if !p.cooldownHintFresh(hash, time.Now()) {
		t.Fatal("ban hint not fresh after CooldownTokenBan")
	}
	if err := p.UnlockToken(0); err != nil {
		t.Fatalf("UnlockToken: %v", err)
	}
	if p.cooldownHintFresh(hash, time.Now()) {
		t.Fatal("hint still fresh after UnlockToken, want cleared")
	}
	if _, ok, _ := mem.LoadPoolState(poolCooldownKey(hash)); ok {
		t.Fatal("hint row survives UnlockToken, want deleted")
	}
}

// TestBridgeSurvivorCapDropsOldest pins the survivor bound: captures past
// maxBridgeSurvivors drop the oldest, the blob carries SHA keys only (raw
// client tokens never), and the retained list folds into the accounting.
func TestBridgeSurvivorCapDropsOldest(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mem := newMemPoolPersist()
	p := newBridgePool(t, mock)
	p.SetPoolPersist(mem)
	now := time.Now()
	total := maxBridgeSurvivors + 10
	for i := range total {
		raw := fmt.Sprintf("raw-secret-client-%d", i)
		entry, err := p.bridgeEntryFor(raw)
		if err != nil {
			t.Fatalf("bridgeEntryFor: %v", err)
		}
		p.bridgeMu.Lock()
		entry.ledger.recordChat(now)
		entry.ledger.recordDayRequest(now)
		p.bridgeMu.Unlock()
		p.captureBridgeSurvivor(tokenKey(raw), entry, now)
	}
	p.bridgeSurvivorMu.Lock()
	n := len(p.bridgeSurvivors)
	var first string
	if n > 0 {
		first = p.bridgeSurvivors[0].Key
	}
	p.bridgeSurvivorMu.Unlock()
	if n != maxBridgeSurvivors {
		t.Fatalf("survivors = %d, want cap %d", n, maxBridgeSurvivors)
	}
	if want := tokenKey(fmt.Sprintf("raw-secret-client-%d", total-maxBridgeSurvivors)); first != want {
		t.Errorf("oldest survivor = %q, want %q (oldest dropped)", first, want)
	}
	blob, ok, err := mem.LoadPoolState(poolBridgeSurvivorsKey)
	if err != nil || !ok {
		t.Fatalf("survivor blob present = %v,%v, want true,nil", ok, err)
	}
	for i := range total {
		if strings.Contains(string(blob), fmt.Sprintf("raw-secret-client-%d", i)) {
			t.Fatalf("raw client token %d leaked into survivor blob", i)
		}
	}
	if chats, _ := p.bridgeSurvivorUsage(now); chats != maxBridgeSurvivors {
		t.Errorf("survivor chats fold = %d, want %d", chats, maxBridgeSurvivors)
	}
}

// TestBridgeSurvivorExpiry pins the survivor TTL: records evicted over one
// usageWindow ago drop on restore (and converge the row); today's Pacific-day
// counts fold only while their day bucket is current.
func TestBridgeSurvivorExpiry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mem := newMemPoolPersist()
	now := time.Now()
	today := bucketStart(now, "day")
	seed := []bridgeSurvivor{
		{Key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", AtMs: now.Add(-25 * time.Hour).UnixMilli(), Chats: 3, Day: 3, DayStart: today},
		{Key: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", AtMs: now.UnixMilli(), Chats: 5, Day: 5, DayStart: today},
	}
	if err := mem.SavePoolState(poolBridgeSurvivorsKey, mustMarshalPool(seed)); err != nil {
		t.Fatalf("seed survivors: %v", err)
	}
	p := newBridgePool(t, mock)
	p.SetPoolPersist(mem)
	p.RestorePoolPersist()
	chats, day := p.bridgeSurvivorUsage(now)
	if chats != 5 || day != 5 {
		t.Errorf("survivor fold = (%d,%d), want (5,5) (aged record expired)", chats, day)
	}
	raw, ok, err := mem.LoadPoolState(poolBridgeSurvivorsKey)
	if err != nil || !ok {
		t.Fatalf("converged survivor row present = %v,%v, want true,nil", ok, err)
	}
	var kept []bridgeSurvivor
	if err := json.Unmarshal(raw, &kept); err != nil {
		t.Fatalf("unmarshal converged survivors: %v", err)
	}
	if len(kept) != 1 || kept[0].Key != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("converged survivors = %+v, want only the fresh record", kept)
	}
}

// TestQuotaSingleWriterRetiresProbeNamespace pins the quota cutover: with
// live session quota present, the flush stages NO pool/probe/quota/* row and
// drains the legacy row older builds left — while the sessions_persist owner
// still serves the quota (read path intact).
func TestQuotaSingleWriterRetiresProbeNamespace(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mem := newMemPoolPersist()
	legacyKey := retiredProbeQuotaPrefix + poolTokenHash("tok-0")
	if err := mem.SavePoolState(legacyKey, []byte(`{"saved_at":1,"quota_by_model":{}}`)); err != nil {
		t.Fatalf("seed legacy quota row: %v", err)
	}
	p := newTestPool(t, mock)
	p.SetPoolPersist(mem)
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	toks := p.roster.Load()
	(*toks)[0].session.UpdateQuotaFromProbe(&upstream.SessionState{
		RateLimitsByModel: map[string]upstream.ModelQuota{
			modelA: {Model: modelA, Limit: 10, RecentCount: 3, ResetAt: reset},
		},
	})
	p.markPersistDirty()
	if err := p.FlushPoolPersist(); err != nil {
		t.Fatalf("FlushPoolPersist: %v", err)
	}
	for _, k := range mem.keys() {
		if strings.HasPrefix(k, retiredProbeQuotaPrefix) {
			t.Fatalf("retired quota row staged/kept: %q (single writer = sessions_persist)", k)
		}
	}
	snap := (*toks)[0].session.Snapshot()
	q, ok := snap.QuotaByModel[modelA]
	if !ok || q.RecentCount != 3 {
		t.Fatalf("session quota = %+v,%v, want the live row (owner intact)", q, ok)
	}
}
