package server_test

import (
	"context"
	"encoding/json"
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/testutil"
	"net/http"
	"strings"
	"testing"
	"time"
)

// dropOutcome decodes one drop-session response body.
func dropOutcome(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("drop-session body is not JSON: %v (%q)", err, body)
	}
	return out
}

// TestDropSessionRealDropShape pins the honest real-drop wire shape: a
// session that was never precious drops for real and the response is the
// bare {ok:true,kept:false} acknowledgment — no message, so no caller can
// mistake it for the keep note.
func TestDropSessionRealDropShape(t *testing.T) {
	t.Chdir(t.TempDir())
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, mock)
	cookie := authedCookie(t, ts)

	resp := doTokenAction(t, ts.URL, cookie, "/admin/tokens/0/drop-session")
	body := bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("drop-session status = %d, want 200: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	out := dropOutcome(t, body)
	if out["ok"] != true {
		t.Errorf("ok = %v, want true", out["ok"])
	}
	if out["kept"] != false {
		t.Errorf("kept = %v, want false (real drop)", out["kept"])
	}
	if _, hasMsg := out["message"]; hasMsg {
		t.Errorf("response carries message = %q, want the bare kept:false shape", out["message"])
	}
	if len(out) != 2 {
		t.Errorf("response keys = %v, want exactly {ok, kept}", out)
	}
}

// TestDropSessionKeptShape pins the precious-keep wire shape: dropping a
// session the account served keeps it (no re-admit), and the response
// carries kept:true with the keep note verbatim.
func TestDropSessionKeptShape(t *testing.T) {
	t.Chdir(t.TempDir())
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	ts, p := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, mock)
	cookie := authedCookie(t, ts)

	// Serve the model once: the granted lease marks the session precious.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	inst := lease.SessionInstanceID
	p.LeaseRelease(lease)
	creates := mock.SessionCreatesSnapshot()

	resp := doTokenAction(t, ts.URL, cookie, "/admin/tokens/0/drop-session")
	body := bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("drop-session status = %d, want 200: %s", resp.StatusCode, body)
	}
	out := dropOutcome(t, body)
	if out["ok"] != true {
		t.Errorf("ok = %v, want true", out["ok"])
	}
	if out["kept"] != true {
		t.Fatalf("kept = %v, want true (precious session kept)", out["kept"])
	}
	const wantMsg = "Session kept (precious) — next request still rides it."
	if out["message"] != wantMsg {
		t.Errorf("message = %q, want %q verbatim", out["message"], wantMsg)
	}

	// The keep is a no-op upstream: no new admission, same live instance.
	if got := mock.SessionCreatesSnapshot(); got != creates {
		t.Errorf("session creates after kept drop = %d, want still %d (no re-admit)", got, creates)
	}
	if got := mock.SessionEndsSnapshot(); got != 0 {
		t.Errorf("session ends after kept drop = %d, want 0 (nothing dropped)", got)
	}
	reuse, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("re-acquire after kept drop: %v", err)
	}
	defer p.LeaseRelease(reuse)
	if reuse.SessionInstanceID != inst {
		t.Errorf("re-acquire instance = %q, want precious %q (session kept)", reuse.SessionInstanceID, inst)
	}
}

// TestDropSessionForcedPreciousDrop pins the force-drop wire path
// end-to-end at the handler level: a plain drop on a precious session
// keeps it (prior shape), then ?force=1 drops it for real — the bare
// {ok:true,kept:false} acknowledgment, one upstream EndSession, and the
// next acquire re-admits fresh (a new upstream create) instead of riding
// the kept session.
func TestDropSessionForcedPreciousDrop(t *testing.T) {
	t.Chdir(t.TempDir())
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	ts, p := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, mock)
	cookie := authedCookie(t, ts)

	// Serve the model once: the granted lease marks the session precious.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	p.LeaseRelease(lease)
	creates := mock.SessionCreatesSnapshot()

	// Plain drop first: the precious session is kept (unforced shape).
	resp := doTokenAction(t, ts.URL, cookie, "/admin/tokens/0/drop-session")
	out := dropOutcome(t, bodyOf(t, resp))
	if out["kept"] != true {
		t.Fatalf("plain drop kept = %v, want true (precious session kept)", out["kept"])
	}

	// Forced drop via query: the precious session ends upstream.
	resp = doTokenAction(t, ts.URL, cookie, "/admin/tokens/0/drop-session?force=1")
	body := bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("forced drop status = %d, want 200: %s", resp.StatusCode, body)
	}
	out = dropOutcome(t, body)
	if out["ok"] != true {
		t.Errorf("ok = %v, want true", out["ok"])
	}
	if out["kept"] != false {
		t.Errorf("kept = %v, want false (forced drop ends precious)", out["kept"])
	}
	if _, hasMsg := out["message"]; hasMsg {
		t.Errorf("response carries message = %q, want the bare kept:false shape", out["message"])
	}
	if len(out) != 2 {
		t.Errorf("response keys = %v, want exactly {ok, kept}", out)
	}
	if got := mock.SessionEndsSnapshot(); got != 1 {
		t.Errorf("session ends after forced drop = %d, want 1 (ended upstream)", got)
	}

	// The next acquire re-admits fresh instead of riding the kept session:
	// no new admission happens on the drop itself (it lands on the next
	// acquire), and the re-admit surfaces as creates+1. The proof is the
	// create counter, not the instance id — the mock always issues
	// inst-abc-123, while the kept path reuses with no new create.
	if got := mock.SessionCreatesSnapshot(); got != creates {
		t.Errorf("session creates after forced drop = %d, want still %d (re-admit happens on next acquire)", got, creates)
	}
	fresh, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("re-acquire after forced drop: %v", err)
	}
	defer p.LeaseRelease(fresh)
	if got := mock.SessionCreatesSnapshot(); got != creates+1 {
		t.Errorf("session creates after re-acquire = %d, want %d (fresh admission)", got, creates+1)
	}
}

// TestDropSessionForcedViaJSONBody pins the second force transport: a
// {"force":true} JSON body (what the SPA's postAPI sends) drops a
// precious session for real.
func TestDropSessionForcedViaJSONBody(t *testing.T) {
	t.Chdir(t.TempDir())
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	ts, p := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, mock)
	cookie := authedCookie(t, ts)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	p.LeaseRelease(lease)

	resp := postJSON(t, ts.URL, cookie, "/admin/tokens/0/drop-session", `{"force":true}`)
	body := bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("forced drop status = %d, want 200: %s", resp.StatusCode, body)
	}
	out := dropOutcome(t, body)
	if out["ok"] != true {
		t.Errorf("ok = %v, want true", out["ok"])
	}
	if out["kept"] != false {
		t.Errorf("kept = %v, want false (JSON-body force drops precious)", out["kept"])
	}
	if got := mock.SessionEndsSnapshot(); got != 1 {
		t.Errorf("session ends after JSON-body forced drop = %d, want 1", got)
	}
}
