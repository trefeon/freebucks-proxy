package server

import (
	"context"
	"errors"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/runs"
	"freebuff-proxy/backend/internal/upstream"
	"io"
	"log/slog"
	"net/http"
	"testing"
)

// fakeAttemptBackend adapts the chatBackend interface for tests: nil hooks
// are no-ops, and the recorded hooks mirror the closure table chatAttempt
// used to take (issue #255).
type fakeAttemptBackend struct {
	acquire         func(ctx context.Context, model string) (*pool.Lease, error)
	chat            func(ctx context.Context, l *pool.Lease, opts upstream.ChatOptions, body []byte) (io.ReadCloser, error)
	invalidate      func(l *pool.Lease)
	supersede       func(l *pool.Lease)
	invalidateRun   func(l *pool.Lease, agentID string)
	cooldownAuth    func(l *pool.Lease)
	cooldownBan     func(l *pool.Lease, be *upstream.BanError)
	cooldownRate    func(l *pool.Lease, rle *upstream.RateLimitError)
	cooldownIP      func(l *pool.Lease, ice *upstream.IpCappedError)
	cooldownCountry func(l *pool.Lease, cbe *upstream.CountryBlockedError)
}

func (b *fakeAttemptBackend) Acquire(ctx context.Context, model string) (*pool.Lease, error) {
	if b.acquire == nil {
		return nil, errors.New("no acquire")
	}
	return b.acquire(ctx, model)
}

func (b *fakeAttemptBackend) Chat(ctx context.Context, l *pool.Lease, opts upstream.ChatOptions, body []byte) (io.ReadCloser, error) {
	return b.chat(ctx, l, opts, body)
}
func (b *fakeAttemptBackend) InvalidateSession(l *pool.Lease)           { b.invalidate(l) }
func (b *fakeAttemptBackend) InvalidateSessionSuperseded(l *pool.Lease) { b.supersede(l) }
func (b *fakeAttemptBackend) InvalidateRun(l *pool.Lease, agentID string) {
	b.invalidateRun(l, agentID)
}
func (b *fakeAttemptBackend) CooldownAuth(l *pool.Lease)                       { b.cooldownAuth(l) }
func (b *fakeAttemptBackend) CooldownBan(l *pool.Lease, be *upstream.BanError) { b.cooldownBan(l, be) }
func (b *fakeAttemptBackend) CooldownRateLimit(l *pool.Lease, rle *upstream.RateLimitError) {
	b.cooldownRate(l, rle)
}

func (b *fakeAttemptBackend) CooldownIpCapped(l *pool.Lease, ice *upstream.IpCappedError) {
	b.cooldownIP(l, ice)
}

func (b *fakeAttemptBackend) CooldownCountry(l *pool.Lease, cbe *upstream.CountryBlockedError) {
	b.cooldownCountry(l, cbe)
}
func (b *fakeAttemptBackend) LeaseRelease(*pool.Lease)          {}
func (b *fakeAttemptBackend) LeaseAbandon(*pool.Lease)          {}
func (b *fakeAttemptBackend) MarkRunFailed(*pool.Lease)         {}
func (b *fakeAttemptBackend) RecordRunStep(*pool.Lease, string) {}
func (b *fakeAttemptBackend) RecordSpend(*pool.Lease, int64)    {}

func TestChatAttemptTurnSpendTerminal(t *testing.T) {
	// turn_spend_limit is terminal for the current request: a failed turn
	// must surface immediately — no failover re-acquire onto another token
	// (that would burn a second account into the same agent loop), no
	// cooldown scheduled (a genuinely new turn must flow), and exactly one
	// upstream chat call.
	runA := &runs.Run{RunID: "run-1", TraceSessionID: "trace-1", ClientID: "client-1", AgentID: "agent-1"}
	firstLease := &pool.Lease{Token: 0, Model: "deepseek/deepseek-v4-flash", AgentID: "agent-1", Run: runA, SessionInstanceID: "inst-1"}

	tsle := &upstream.TurnSpendLimitError{Status: http.StatusTooManyRequests, Body: `{"error":"turn_spend_limit"}`}
	acquires, chats := 0, 0
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.cfg.Store(&config.Config{})
	backend := &fakeAttemptBackend{
		acquire: func(ctx context.Context, model string) (*pool.Lease, error) {
			acquires++
			return firstLease, nil
		},
		chat: func(ctx context.Context, l *pool.Lease, opts upstream.ChatOptions, body []byte) (io.ReadCloser, error) {
			chats++
			return nil, tsle
		},
		cooldownRate: func(l *pool.Lease, err *upstream.RateLimitError) {
			t.Error("cooldownRate must not fire for turn_spend")
		},
	}

	_, _, err := s.chatAttempt(context.Background(), "deepseek/deepseek-v4-flash", []byte(`{}`), &chatTraceState{reqID: "req-ts"}, backend)
	if !errors.Is(err, upstream.ErrTurnSpendLimited) {
		t.Fatalf("expected ErrTurnSpendLimited, got: %v", err)
	}
	if acquires != 1 {
		t.Errorf("acquires = %d, want 1 (no failover re-acquire)", acquires)
	}
	if chats != 1 {
		t.Errorf("upstream chat calls = %d, want 1", chats)
	}
}
