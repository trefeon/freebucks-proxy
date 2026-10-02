package server_test

import (
	"encoding/json"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const modelBResume = "mimo/mimo-v2.5"

func resumeChatBody(model, history string) []byte {
	return []byte(`{"model":"` + model + `","messages":[` + history + `],"stream":false}`)
}

func servedModelOf(t *testing.T, resp *http.Response, data []byte) (header, body string) {
	t.Helper()
	header = resp.Header.Get("X-FreeBuff-Served-Model")
	var comp map[string]any
	if err := json.Unmarshal(data, &comp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	body, _ = comp["model"].(string)
	return header, body
}

// T3a (resume B2): a session bound to A serves a resumed turn requesting B
// on B (re-admit, old slot released) — never silently on A. The served
// model is signaled on the header and the body.
func TestResumeModelSwitchServesRequested(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	histA := `{"role":"user","content":"earlier"},{"role":"assistant","content":"ack"},{"role":"user","content":"now A"}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", resumeChatBody(modelA, histA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat A status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if h, b := servedModelOf(t, resp, data); h != modelA || b != modelA {
		t.Errorf("chat A served header=%q body=%q, want %q", h, b, modelA)
	}

	histB := `{"role":"user","content":"earlier"},{"role":"assistant","content":"ack on A"},{"role":"user","content":"now B"}`
	resp, data = doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", resumeChatBody(modelBResume, histB), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat B status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if h, b := servedModelOf(t, resp, data); h != modelBResume || b != modelBResume {
		t.Errorf("resumed chat served header=%q body=%q, want requested %q (never silent sticky-A)", h, b, modelBResume)
	}
	if got := mock.SessionCreatesSnapshot(); got != 2 {
		t.Errorf("session creates = %d, want 2 (A admit + B re-admit)", got)
	}
}

// T3b: an unknown model id fails at the gate with 404 and zero upstream
// session contact.
func TestResumeModelSwitchUnknownModel404(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", resumeChatBody("nope/not-a-model", `{"role":"user","content":"hi"}`), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (unknown model): %s", resp.StatusCode, truncate(string(data), 200))
	}
	if got := mock.SessionCreatesSnapshot(); got != 0 {
		t.Errorf("session creates = %d, want 0 (gate rejects before admission)", got)
	}
}

// T3c: a model every slot is pinned away from fails loud (explicit routing
// error, no upstream admission) instead of silently serving another model.
func TestResumeModelSwitchPinFailFast(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.PinModel = map[int]string{0: modelA, 1: modelA}
	}, mock0, mock1)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", resumeChatBody(modelBResume, `{"role":"user","content":"hi"}`), nil)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("status = 200, want a loud routing refusal (no slot serves %q)", modelBResume)
	}
	if !strings.Contains(string(data), "no account pinned") {
		t.Errorf("body missing the pin fail-fast signal: %s", truncate(string(data), 200))
	}
	if n := mock0.SessionCreatesSnapshot() + mock1.SessionCreatesSnapshot(); n != 0 {
		t.Errorf("session creates = %d, want 0 (fail-fast burns no admission)", n)
	}
}

// T3d (documented sticky-A): when upstream binds the new-model admission to
// the OLD model (limited-tier coercion), the turn serves on the live session
// — but the divergence is signaled (header + body name the served model),
// never silent.
func TestResumeModelSwitchCoercedStickySignalsServed(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var creates atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			creates.Add(1)
			model := r.Header.Get("x-freebuff-model")
			served := model
			if model == modelBResume {
				served = modelA // upstream coerces the B admission onto A
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-`+strings.ReplaceAll(served, "/", "-")+`","model":"`+served+`","expiresAt":"2030-01-01T00:00:00Z"}`)
		case http.MethodDelete:
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"status":"ended"}`)
		default:
			http.NotFound(w, r)
		}
	}
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", resumeChatBody(modelA, `{"role":"user","content":"bind A"}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat A status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}

	resp, data = doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", resumeChatBody(modelBResume, `{"role":"user","content":"resume asking B"}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("coerced chat status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	h, b := servedModelOf(t, resp, data)
	if h != modelA || b != modelA {
		t.Errorf("coerced chat served header=%q body=%q, want documented sticky %q (divergence must be signaled)", h, b, modelA)
	}
	if got := creates.Load(); got != 2 {
		t.Errorf("admission POSTs = %d, want 2 (A bind + coerced B attempt)", got)
	}
}

// T4 (stale persisted claim through chat): a resumed turn whose admission
// carries a dead claim 409s once (admission_attempt_closed), rotates to a
// fresh claim, and is adopted — exactly 2 admissions, chat 200.
func TestResumeStaleClaimRotatesThroughChat(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var posts atomic.Int32
	var mu sync.Mutex
	var claims []string
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		n := posts.Add(1)
		mu.Lock()
		claims = append(claims, r.Header.Get("x-freebuff-instance-id"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"admission_attempt_closed","message":"The referenced admission attempt is closed."}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-rotated","model":"`+modelA+`","expiresAt":"2030-01-01T00:00:00Z"}`)
	}
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", resumeChatBody(modelA, `{"role":"user","content":"resume after restart"}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200 (rotated claim adopted): %s", resp.StatusCode, truncate(string(data), 200))
	}
	if got := posts.Load(); got != 2 {
		t.Fatalf("admission POSTs = %d, want exactly 2 (stale 409 + one rotated retry)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if claims[0] == "" || claims[1] == "" || claims[0] == claims[1] {
		t.Errorf("claims = %q, want two distinct non-empty claim ids (rotation before retry)", claims)
	}
}
