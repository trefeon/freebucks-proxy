package server_test

// Replay tests for the shared ops surfaces every harness client can hit:
// the OpenAI 429 + Retry-After error contract (opencode/qwen-style clients
// honor the header backoff), the CORS preflight contract for browser
// frontends, and the model-list / model-retrieve catalog surface.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// TestReplayChat429RetryAfter mirrors replay-chat-429-retry-after: an
// upstream 429 quota refusal on /v1/chat/completions must be surfaced to
// the client as a well-formed OpenAI error body + Retry-After header (an
// opencode/qwen-style client honors the header to back off) — and the
// classified 429 must NEVER be retried upstream.
func TestReplayChat429RetryAfter(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "13")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"status":"rate_limited","message":"Daily session quota exhausted","retryAfterMs":13000}`)
	}
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", resp.StatusCode, truncate(string(data), 300))
	}
	if ra := resp.Header.Get("Retry-After"); ra != "13" {
		t.Errorf("Retry-After = %q, want 13 (ceil of retryAfterMs 13000)", ra)
	}
	// OpenAI error shape: {"error":{"message":...,"type":...,"param":null,"code":...}}.
	var errBody struct {
		Error struct {
			Message string  `json:"message"`
			Type    string  `json:"type"`
			Param   *string `json:"param"`
			Code    string  `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &errBody); err != nil {
		t.Fatalf("body is not OpenAI error JSON: %v (%q)", err, truncate(string(data), 300))
	}
	if errBody.Error.Code != "rate_limited" {
		t.Errorf("error.code = %q, want rate_limited", errBody.Error.Code)
	}
	if errBody.Error.Message == "" || errBody.Error.Type == "" {
		t.Errorf("error message/type empty: %q", truncate(string(data), 300))
	}
	if errBody.Error.Param != nil {
		t.Errorf("error.param = %v, want null", *errBody.Error.Param)
	}
	// Retry-After must also be honored for the error message hint the
	// client surfaces; and classified 429s are never re-sent upstream.
	if chatCalls.Load() != 1 {
		t.Errorf("upstream chat calls = %d, want exactly 1 (429 must not be retried)", chatCalls.Load())
	}
}

// TestReplayCorsPreflight204 mirrors replay-cors-preflight-204: an OPTIONS
// preflight on a /v1/* surface (here /v1/responses, the codex surface a
// browser client would use) must answer 204 with the CORS allow headers
// covering every credential header the auth path accepts (Content-Type,
// Authorization, x-api-key, anthropic-version) — and must be answered
// before any routing or upstream contact.
func TestReplayCorsPreflight204(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodOptions, ts.URL+"/v1/responses", nil,
		map[string]string{"Origin": "https://example.com"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("OPTIONS /v1/responses status = %d, want 204: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if len(data) != 0 {
		t.Errorf("preflight body = %q, want empty", string(data))
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want * (unpinned default)", got)
	}
	headers := resp.Header.Get("Access-Control-Allow-Headers")
	for _, want := range []string{"Content-Type", "Authorization", "x-api-key", "anthropic-version"} {
		if !strings.Contains(headers, want) {
			t.Errorf("Access-Control-Allow-Headers missing %q: %q", want, headers)
		}
	}
	for _, want := range []string{"POST", "GET", "OPTIONS"} {
		if !strings.Contains(resp.Header.Get("Access-Control-Allow-Methods"), want) {
			t.Errorf("Access-Control-Allow-Methods missing %q", want)
		}
	}
	if mock.RequestsSnapshot() != 0 {
		t.Errorf("upstream requests = %d, want 0 (preflight answered before routing)", mock.RequestsSnapshot())
	}
}

// TestReplayModelsSurface pins the /v1/models catalog surface: GET
// /v1/models returns the OpenAI list shape (data rows id/object/created/
// owned_by) and GET /v1/models/{id} returns the single-row shape; an
// unknown id must 404 with the model_not_found hint pointing at GET
// /v1/models. The catalog is served from the registry — nothing upstream
// is contacted.
func TestReplayModelsSurface(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	// List.
	resp, data := doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("GET /v1/models Content-Type = %q, want application/json", ct)
	}
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID        string `json:"id"`
			Object    string `json:"object"`
			Created   int64  `json:"created"`
			OwnedBy   string `json:"owned_by"`
			Status    string `json:"status"`
			Available *bool  `json:"available"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("list not JSON: %v", err)
	}
	if list.Object != "list" {
		t.Errorf("list object = %q, want list", list.Object)
	}
	if len(list.Data) == 0 {
		t.Fatal("list data empty")
	}
	foundDefault := false
	for _, m := range list.Data {
		if m.Object != "model" || m.Created == 0 || m.OwnedBy != "freebuff" {
			t.Errorf("model row shape wrong: %+v", m)
		}
		if m.ID == modelA {
			foundDefault = true
		}
	}
	if !foundDefault {
		t.Errorf("list missing %q (the guaranteed fallback model)", modelA)
	}

	// Retrieve.
	resp2, data2 := doJSON(t, http.MethodGet, ts.URL+"/v1/models/"+modelA, nil, nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models/%s status = %d: %s", modelA, resp2.StatusCode, truncate(string(data2), 300))
	}
	var modelObj struct {
		ID        string `json:"id"`
		Object    string `json:"object"`
		Created   int64  `json:"created"`
		OwnedBy   string `json:"owned_by"`
		Status    string `json:"status"`
		Available *bool  `json:"available"`
	}
	if err := json.Unmarshal(data2, &modelObj); err != nil {
		t.Fatalf("retrieve not JSON: %v", err)
	}
	if modelObj.ID != modelA || modelObj.Object != "model" || modelObj.OwnedBy != "freebuff" || modelObj.Created == 0 {
		t.Errorf("retrieve shape = %+v, want id=%s object=model owned_by=freebuff", modelObj, modelA)
	}

	// Unknown model: 404 + hint.
	resp3, data3 := doJSON(t, http.MethodGet, ts.URL+"/v1/models/unknown-model-xyz", nil, nil)
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /v1/models/unknown-model-xyz status = %d, want 404: %s", resp3.StatusCode, truncate(string(data3), 300))
	}
	var notFound struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Hint    string `json:"hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data3, &notFound); err != nil {
		t.Fatalf("404 body not JSON: %v (%q)", err, truncate(string(data3), 300))
	}
	if notFound.Error.Code != "model_not_found" {
		t.Errorf("404 error.code = %q, want model_not_found", notFound.Error.Code)
	}
	if !strings.Contains(notFound.Error.Hint, "GET /v1/models") {
		t.Errorf("404 hint = %q, want it to point at GET /v1/models", notFound.Error.Hint)
	}

	// The catalog surface is served locally — no upstream contact at all.
	if mock.RequestsSnapshot() != 0 {
		t.Errorf("upstream requests = %d, want 0 (catalog served from registry)", mock.RequestsSnapshot())
	}
}
