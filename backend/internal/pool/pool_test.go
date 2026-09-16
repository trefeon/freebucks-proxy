package pool

import (
	"context"
	"errors"
	"fmt"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
	"net/http"
	"sync"
	"testing"
	"time"
)

// Test models must map to agents with EXCLUSIVE ownership in the registry
// FALLBACK map (see backend/internal/registry/registry_test.go expectedFallback):
// the five base2-free models are root-mapped to their per-model agents, while
// glm-5.2 and claude-fable-5 are owned by their dedicated one-model agents.
// Tests pin the offline (fallback) state.
const (
	modelA = "anthropic/claude-fable-5"
	modelB = "deepseek/deepseek-v4-flash"
	agentA = "base2-free-fable"
	agentB = "base2-free-deepseek-flash"
)

// newTestPool wires one mock upstream per token through real clients and
// session managers, backed by the registry fallback map.
func newTestPool(t *testing.T, mocks ...*testutil.MockUpstream) *Pool {
	return newTestPoolCfg(t, nil, mocks...)
}

// newTestPoolCfg is newTestPool with a config mutation hook (e.g. enabling
// TRANSIENT_RETRIES / TLS_FINGERPRINT for retry tests).
func newTestPoolCfg(t *testing.T, mut func(*config.Config), mocks ...*testutil.MockUpstream) *Pool {
	t.Helper()
	cfg := &config.Config{
		AuthTokens:         make([]string, len(mocks)),
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
		// Park-OFF pins the historical path: a hand-built Config zero-values
		// the flag while production Load defaults it ON (see
		// TestHandBuiltConfigDisablesPark); park-ON is covered by
		// TestBridgeSweepParksShortCooldown.
		SessionParkEnabledFlag: false,
	}
	if mut != nil {
		mut(cfg)
	}
	clients := make([]*upstream.Client, 0, len(mocks))
	sessions := make([]*session.Manager, 0, len(mocks))
	for i, mock := range mocks {
		cfg.AuthTokens[i] = fmt.Sprintf("tok-%d", i)
		clientCfg := *cfg
		clientCfg.UpstreamBaseURL = mock.URL()
		client, err := upstream.New(cfg.AuthTokens[i], &clientCfg)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
		sessions = append(sessions, session.NewManager(client))
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := New(cfg, clients, sessions, reg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// regAgentIDs re-reads the pool's registry agent list through a fresh
// fallback registry (the pool does not export its registry).
func (p *Pool) regAgentIDs(t *testing.T) []string {
	t.Helper()
	reg := registry.New(p.cfg.Load(), nil)
	reg.LoadFallback()
	return reg.AgentIDs()
}

// chatOnce sends one chat through the leased token against the mock
// upstream and closes the body; used to accumulate successful chats for the
// daily-cap tests.
func chatOnce(t *testing.T, p *Pool, lease *Lease) {
	t.Helper()
	opts := upstream.ChatOptions{Model: modelA, RunID: lease.Run.RunID, SessionInstanceID: lease.SessionInstanceID}
	body := []byte(`{"model":"` + modelA + `","messages":[{"role":"user","content":"ping"}]}`)
	rc, err := p.Chat(context.Background(), lease, opts, body)
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
}

// eventually polls cond until it holds or the deadline passes. Thin
// package wrapper over testutil.WaitFor so the pool package's many call
// sites keep their signature; the loop itself lives in testutil/poll.go.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	testutil.WaitFor(t, 5*time.Second, cond, what)
}

// atomicErr is a thread-safe first-error holder for the hammer.
type atomicErr struct {
	mu  sync.Mutex
	err error
}

func (e *atomicErr) set(err error) {
	e.mu.Lock()
	if e.err == nil {
		e.err = err
	}
	e.mu.Unlock()
}

func (e *atomicErr) get() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

// --- bridge mode ---

// newBridgePool wires a pool in bridge mode (no AUTH_TOKENS) whose lazily
// created per-client-token clients talk to the given mock upstream.
func newBridgePool(t *testing.T, mock *testutil.MockUpstream) *Pool {
	t.Helper()
	cfg := &config.Config{
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		// Park-OFF like newTestPoolCfg above (hand-built Config zero-values
		// the flag; production Load defaults ON).
		SessionParkEnabledFlag: false,
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// flakyFirstRT fails the very first request with a transient transport error
// and delegates everything else to base. It drives a real retry through the
// full stack deterministically (a live connection teardown surfaces as
// context.Canceled on some platforms, which must never be retried).
type flakyFirstRT struct {
	mu     sync.Mutex
	failed bool
	base   http.RoundTripper
}

func (f *flakyFirstRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	shouldFail := !f.failed
	if shouldFail {
		f.failed = true
	}
	f.mu.Unlock()
	if shouldFail {
		return nil, errors.New("read tcp 127.0.0.1:443: connection reset by peer")
	}
	return f.base.RoundTrip(req)
}
