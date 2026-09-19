package server_test

import (
	"context"
	"encoding/json"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
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
