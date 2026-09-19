package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream/login"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// --- #103 / free_mode_run_fanout: client_id is PER RUN ----------------------

// TestInjectEnvelopeClientIDPerRun pins the client_id scope. The CLI mints it
// once per prompt (run.ts:722 promptId -> run.ts:822 clientSessionId ->
// llm.ts:117 client_id) and every LLM step of that run repeats it, so the
// envelope must REPEAT ChatOptions.ClientID rather than draw a fresh id per
// call: N ids under one run_id is the fanout shape upstream refuses with
// free_mode_run_fanout. The shape stays SDK-faithful and unprefixed, and an
// empty ClientID still falls back to a well-shaped draw.
func TestInjectEnvelopeClientIDPerRun(t *testing.T) {
	base36 := regexp.MustCompile(`^[a-z0-9]{13}$`)
	const runClientID = "abc123def4567"

	out, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "run-1", SessionInstanceID: "inst-9", TraceSessionID: "trace-abc", ClientID: runClientID})
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal(out, &sent); err != nil {
		t.Fatal(err)
	}
	md := sent["codebuff_metadata"].(map[string]any)
	if md["client_id"] != runClientID {
		t.Errorf("client_id = %v, want the run's id %q repeated", md["client_id"], runClientID)
	}
	if md["trace_session_id"] != "trace-abc" {
		t.Errorf("trace_session_id = %v, want trace-abc (per run)", md["trace_session_id"])
	}
	if md["freebuff_instance_id"] != "inst-9" {
		t.Errorf("freebuff_instance_id = %v, want inst-9 (per session)", md["freebuff_instance_id"])
	}

	// The SECOND call of the same run must carry the SAME id — this is the
	// regression that produced free_mode_run_fanout.
	out2, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "run-1", SessionInstanceID: "inst-9", ClientID: runClientID})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out2, &sent); err != nil {
		t.Fatal(err)
	}
	if got := sent["codebuff_metadata"].(map[string]any)["client_id"]; got != runClientID {
		t.Errorf("client_id on the run's second call = %v, want %q (one client_id per run)", got, runClientID)
	}

	// No ClientID supplied (admin smoke ping, bridge callers): fall back to a
	// fresh SDK-faithful draw, never a prefixed proxy fingerprint.
	out3, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "run-3"})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out3, &sent); err != nil {
		t.Fatal(err)
	}
	fallback, _ := sent["codebuff_metadata"].(map[string]any)["client_id"].(string)
	if !base36.MatchString(fallback) {
		t.Errorf("fallback client_id = %q, want a 13-char base36 draw", fallback)
	}
	for _, prefix := range []string{"sess:", "run:", "wf-"} {
		if strings.HasPrefix(fallback, prefix) {
			t.Errorf("client_id = %q, must not carry the %q prefix (proxy fingerprint)", fallback, prefix)
		}
	}
}

// TestNewClientIDIsPerRunDraw pins that the run manager's generator produces
// distinct SDK-faithful ids — one per run, not one per process.
func TestNewClientIDIsPerRunDraw(t *testing.T) {
	base36 := regexp.MustCompile(`^[a-z0-9]{13}$`)
	a, b := NewClientID(), NewClientID()
	for _, id := range []string{a, b} {
		if !base36.MatchString(id) {
			t.Errorf("NewClientID = %q, want 13-char base36", id)
		}
	}
	if a == b {
		t.Error("NewClientID returned the same id twice; runs must not share one")
	}
}

// TestInjectEnvelopeStepNumber verifies #113: llm_step_number is injected as
// a STRING when ChatOptions.StepNumber > 0 and absent when zero.
func TestInjectEnvelopeStepNumber(t *testing.T) {
	out, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "r", StepNumber: 3})
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal(out, &sent); err != nil {
		t.Fatal(err)
	}
	md := sent["codebuff_metadata"].(map[string]any)
	if md["llm_step_number"] != "3" {
		t.Errorf("llm_step_number = %v (%T), want %q (string form)", md["llm_step_number"], md["llm_step_number"], "3")
	}

	out2, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out2, &sent); err != nil {
		t.Fatal(err)
	}
	if _, present := sent["codebuff_metadata"].(map[string]any)["llm_step_number"]; present {
		t.Error("llm_step_number present when StepNumber == 0")
	}
}

// TestInjectEnvelopeNAndCacheDebugCorrelation verifies llm.ts:118-122
// mirror: N (when >0) and cache_debug_correlation (when non-empty) are
// stamped into codebuff_metadata, and ExtraCodebuffMetadata is merged
// BEFORE reserved identifiers so reserved keys win. Also covers payload
// codebuff_metadata extra passthrough (payload extras are kept, reserved
// extras are overwritten).
func TestInjectEnvelopeNAndCacheDebugCorrelation(t *testing.T) {
	// N + cache_debug_correlation present
	out, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{
		RunID:                 "r",
		N:                     3,
		CacheDebugCorrelation: "corr-123",
		ExtraCodebuffMetadata: map[string]string{"custom_key": "custom_val", "run_id": "smuggled"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal(out, &sent); err != nil {
		t.Fatal(err)
	}
	md := sent["codebuff_metadata"].(map[string]any)
	if md["n"] != float64(3) { // JSON numbers decode as float64
		t.Errorf("n = %v (%T), want 3", md["n"], md["n"])
	}
	if md["cache_debug_correlation"] != "corr-123" {
		t.Errorf("cache_debug_correlation = %v, want corr-123", md["cache_debug_correlation"])
	}
	if md["custom_key"] != "custom_val" {
		t.Errorf("custom_key = %v, want custom_val (extra metadata passthrough)", md["custom_key"])
	}
	if md["run_id"] != "r" {
		t.Errorf("run_id = %v, want r (reserved must win over smuggled extra)", md["run_id"])
	}

	// Absent when zero/empty
	out2, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out2, &sent); err != nil {
		t.Fatal(err)
	}
	md2 := sent["codebuff_metadata"].(map[string]any)
	if _, present := md2["n"]; present {
		t.Error("n present when N == 0")
	}
	if _, present := md2["cache_debug_correlation"]; present {
		t.Error("cache_debug_correlation present when empty")
	}

	// Payload codebuff_metadata extra keys are preserved; reserved payload
	// keys are overwritten.
	out3, err := injectEnvelope([]byte(`{"model":"m","codebuff_metadata":{"payload_extra":"keep","run_id":"old","cache_debug_correlation":"old_corr"}}`), "free", ChatOptions{RunID: "r", CacheDebugCorrelation: "new_corr"})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out3, &sent); err != nil {
		t.Fatal(err)
	}
	md3 := sent["codebuff_metadata"].(map[string]any)
	if md3["payload_extra"] != "keep" {
		t.Errorf("payload_extra = %v, want keep (payload extra passthrough)", md3["payload_extra"])
	}
	if md3["run_id"] != "r" {
		t.Errorf("run_id = %v, want r (payload reserved overwrite)", md3["run_id"])
	}
	if md3["cache_debug_correlation"] != "new_corr" {
		t.Errorf("cache_debug_correlation = %v, want new_corr (ChatOptions wins over payload)", md3["cache_debug_correlation"])
	}

	// Downstream client sending n via payload codebuff_metadata must be
	// forwarded when opts.N==0 (watchdog: n is forwardable, not reserved).
	out4, err := injectEnvelope([]byte(`{"model":"m","codebuff_metadata":{"n":2,"custom":"x"}}`), "free", ChatOptions{RunID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out4, &sent); err != nil {
		t.Fatal(err)
	}
	md4 := sent["codebuff_metadata"].(map[string]any)
	if md4["n"] != float64(2) {
		t.Errorf("payload n = %v, want 2 (forwarded when opts.N==0)", md4["n"])
	}
	// opts.N overwrites payload n when set
	out5, err := injectEnvelope([]byte(`{"model":"m","codebuff_metadata":{"n":2}}`), "free", ChatOptions{RunID: "r", N: 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out5, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["codebuff_metadata"].(map[string]any)["n"] != float64(5) {
		t.Errorf("n = %v, want 5 (opts.N overwrites payload)", sent["codebuff_metadata"].(map[string]any)["n"])
	}
}

// --- #94: 428 waiting_room_required classification ---------------------------

func TestClassifyWaitingRoomRequired(t *testing.T) {
	err := classifyError(428, `{"error":"waiting_room_required","message":"walk the ads chain"}`, http.Header{})
	if !errors.Is(err, ErrWaitingRoomRequired) {
		t.Fatalf("428 waiting_room_required: errors.Is = false, want ErrWaitingRoomRequired (got %v)", err)
	}
	if errors.Is(err, ErrSessionInvalid) {
		t.Fatal("428 waiting_room_required must NOT be ErrSessionInvalid (#94)")
	}
	if errors.Is(err, ErrWaitingRoom) {
		t.Fatal("428 waiting_room_required must NOT be ErrWaitingRoom (it is its own signal)")
	}
	// Retry-After honored.
	err = classifyError(428, `{"error":"waiting_room_required"}`, http.Header{"Retry-After": {"45"}})
	var wrr *WaitingRoomRequiredError
	if !errors.As(err, &wrr) {
		t.Fatalf("want *WaitingRoomRequiredError, got %T", err)
	}
	if wrr.RetryAfter != 45*time.Second {
		t.Errorf("RetryAfter = %v, want 45s from header", wrr.RetryAfter)
	}
}

// TestClientClassifySetsWaitingRoomFlag verifies the client wrapper records
// the 428 flag so the pool can fire the gated WAITING_ROOM_CHAIN (#94).
func TestClientClassifySetsWaitingRoomFlag(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = 428
	mock.ChatErrorBody = `{"error":"waiting_room_required"}`
	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if client.PendingWaitingRoomChain() {
		t.Fatal("flag set before any 428")
	}
	_, err = client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))
	if err == nil {
		t.Fatal("expected 428 error")
		return
	}
	if !errors.Is(err, ErrWaitingRoomRequired) {
		t.Fatalf("err = %v, want ErrWaitingRoomRequired", err)
	}
	if !client.PendingWaitingRoomChain() {
		t.Fatal("PendingWaitingRoomChain = false after 428, want true")
	}
	if !client.ConsumeWaitingRoomChain() {
		t.Fatal("ConsumeWaitingRoomChain = false, want true (flag was set)")
	}
	if client.PendingWaitingRoomChain() {
		t.Fatal("flag still set after Consume")
	}
	// A second consume must return false (fired exactly once).
	if client.ConsumeWaitingRoomChain() {
		t.Fatal("second ConsumeWaitingRoomChain = true, want false")
	}
}

// --- #62: headless OAuth login flow ------------------------------------------

func TestStartCLILogin(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	client, err := NewForAuth(testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	code, err := client.StartCLILogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if mock.AuthCLICodeRequests != 1 {
		t.Errorf("AuthCLICodeRequests = %d, want 1", mock.AuthCLICodeRequests)
	}
	if !strings.HasPrefix(code.FingerprintID, "enhanced-") {
		t.Errorf("FingerprintID = %q, want enhanced- prefix", code.FingerprintID)
	}
	if code.FingerprintHash == "" || code.LoginURL == "" || code.ExpiresAt.IsZero() {
		t.Errorf("code = %+v, want hash+loginURL+expiresAt", code)
	}
}

func TestStartCLILoginError(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.AuthCLICodeStatus = 500
	mock.AuthCLICodeBody = `{"error":"boom"}`
	client, err := NewForAuth(testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.StartCLILogin(context.Background()); err == nil {
		t.Fatal("StartCLILogin succeeded, want error on 500")
		return
	}
}

func TestPollCLILoginPendingThenComplete(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	client, err := NewForAuth(testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	code, err := client.StartCLILogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Pending: the mock serves 401 while AuthCLIStatusBody is empty.
	status, err := client.PollCLILogin(context.Background(), code)
	if err != nil {
		t.Fatal(err)
	}
	if status.Done || status.AuthToken != "" {
		t.Errorf("pending status = %+v, want Done=false", status)
	}

	// Completed: token + user metadata once the body is served.
	mock.AuthCLIStatusBody = `{"authToken":"cb_complete","user":{"id":"gh-1","name":"Ada","email":"ada@example.com"}}`
	status, err = client.PollCLILogin(context.Background(), code)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Done {
		t.Fatal("Done = false, want true after token appears")
	}
	if status.AuthToken != "cb_complete" {
		t.Errorf("AuthToken = %q, want cb_complete", status.AuthToken)
	}
	if status.User.ID != "gh-1" || status.User.Name != "Ada" {
		t.Errorf("user = %+v, want gh-1/Ada", status.User)
	}
	if mock.AuthCLIStatusRequests != 2 {
		t.Errorf("AuthCLIStatusRequests = %d, want 2", mock.AuthCLIStatusRequests)
	}
}

// TestPollCLILoginTransient5xxKeepsPolling verifies #125: a transient 5xx
// from /api/auth/cli/status must report pending (Done=false, no error) so
// the caller keeps polling until the 5-minute deadline — mirroring
// login-flow.ts pollLoginStatus, which retries every non-401 status.
func TestPollCLILoginTransient5xxKeepsPolling(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	client, err := NewForAuth(testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	code, err := client.StartCLILogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// The upstream answers 500 (transient blip). Handler is set AFTER
	// StartCLILogin, so only status polls hit it.
	mock.AuthCLIHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/cli/status" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"boom"}`)
			return
		}
		http.NotFound(w, r)
	}
	status, err := client.PollCLILogin(context.Background(), code)
	if err != nil {
		t.Fatalf("PollCLILogin errored on transient 5xx, want pending (no error): %v", err)
	}
	if status.Done || status.AuthToken != "" {
		t.Errorf("status = %+v, want Done=false on transient 5xx", status)
	}
}

// TestPollCLILoginTransportErrorTransient verifies #125: a transport-level
// failure (connection refused) must report pending, not abort — the CLI
// keeps polling through network errors (login-flow.ts catch branch).
func TestPollCLILoginTransportErrorTransient(t *testing.T) {
	// A closed listener makes every request fail at dial time.
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	client, err := NewForAuth(testConfig(closed.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	code := &CLILoginCode{
		FingerprintID:   "enhanced-x",
		FingerprintHash: "h",
		ExpiresAtRaw:    time.Now().Add(5 * time.Minute).UnixMilli(),
	}
	status, err := client.PollCLILogin(context.Background(), code)
	if err != nil {
		t.Fatalf("PollCLILogin errored on transport failure, want pending (no error): %v", err)
	}
	if status.Done || status.AuthToken != "" {
		t.Errorf("status = %+v, want Done=false on transport failure", status)
	}
}

// TestAuthLoginRequestsCarryBunUA verifies the UA scoping: the
// /api/auth/cli/code and /api/auth/cli/status calls go through plain Bun
// fetch in the real CLI (login-flow.ts request() sets no UA override), so
// they carry bunUserAgent (Bun/1.3.14) — never the chat ai-sdk cliUserAgent.
func TestAuthLoginRequestsCarryBunUA(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var codeUA, statusUA string
	mock.AuthCLIHandler = func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/cli/code":
			codeUA = r.Header.Get("User-Agent")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"fingerprintId":"enhanced-x","fingerprintHash":"h","loginUrl":"https://github.com/login/oauth/authorize?auth_code=abc","expiresAt":`+strconv.FormatInt(time.Now().Add(5*time.Minute).UnixMilli(), 10)+`}`)
		case "/api/auth/cli/status":
			statusUA = r.Header.Get("User-Agent")
			w.WriteHeader(http.StatusUnauthorized) // pending
		default:
			http.NotFound(w, r)
		}
	}
	client, err := NewForAuth(testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	code, err := client.StartCLILogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.PollCLILogin(context.Background(), code); err != nil {
		t.Fatal(err)
	}
	if codeUA != bunUserAgent {
		t.Errorf("/api/auth/cli/code User-Agent = %q, want %q", codeUA, bunUserAgent)
	}
	if statusUA != bunUserAgent {
		t.Errorf("/api/auth/cli/status User-Agent = %q, want %q", statusUA, bunUserAgent)
	}
}

// --- Stable machine-derived login fingerprint ---------------------------------

// TestLoginCarriesIsolatedFingerprint verifies StartCLILoginIsolated sends a
// fresh distinct fingerprint per login.
func TestLoginCarriesIsolatedFingerprint(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	client, err := NewForAuth(testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.StartCLILoginIsolated(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := mock.LastAuthFingerprintID
	if !strings.HasPrefix(first, "enhanced-") {
		t.Errorf("login fingerprintId = %q, want enhanced- prefix", first)
	}
	if _, err := client.StartCLILoginIsolated(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := mock.LastAuthFingerprintID
	if first == second {
		t.Fatalf("isolated logins produced identical fingerprint: %q", first)
	}
}

// TestLoginCarriesMachineFingerprint verifies POST /api/auth/cli/code sends
// the stable process-wide fingerprint as its fingerprintId (not a fresh
// random value per login).
func TestLoginCarriesMachineFingerprint(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	client, err := NewForAuth(testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.StartCLILogin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := login.GenerateFingerprintID(); mock.LastAuthFingerprintID != want {
		t.Errorf("login fingerprintId = %q, want the stable machine id %q", mock.LastAuthFingerprintID, want)
	}
}

// TestProtocolGitHubLoginOffline exercises the protocol login's status
// vocabulary against a scripted mock that serves NO GitHub HTML — the flow
// must fail with a parse-style message naming the login URL, never panic
// (the live GitHub walk cannot be exercised in CI).
func TestProtocolGitHubLoginFormNotFound(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	client, err := NewForAuth(testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	// The mock's /api/auth/cli/code loginUrl points at the mock itself
	// (github.com is never contacted), which serves 404 JSON — no forms.
	mock.AuthCLICodeBody = `{"fingerprintId":"enhanced-x","fingerprintHash":"h","loginUrl":"` + mock.URL() + `/login","expiresAt":` + rfc3339MillisUnix() + `}`
	_, err = client.ProtocolGitHubLogin(context.Background(), "user", "pass", "JBSWY3DPEHPK3PXP", nil)
	if err == nil {
		t.Fatal("ProtocolGitHubLogin succeeded, want form-not-found error")
		return
	}
	if !strings.Contains(err.Error(), "login form not found") {
		t.Errorf("err = %v, want login-form-not-found message", err)
	}
}

func TestProtocolTOTP(t *testing.T) {
	// RFC 6238 test vector: secret "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	// (ASCII "12345678901234567890"), T=59s → 287082.
	code, err := githubProtocolTOTPAt("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", time.Unix(59, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(code, "287082") {
		t.Errorf("TOTP at 59s = %q, want 287082 prefix", code)
	}
	// T=1111111109 → 081804; T=1111111111 → 050471.
	if c, _ := githubProtocolTOTPAt("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", time.Unix(1111111109, 0)); !strings.HasPrefix(c, "081804") {
		t.Errorf("TOTP at 1111111109 = %q, want 081804 prefix", c)
	}
	if c, _ := githubProtocolTOTPAt("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", time.Unix(1111111111, 0)); !strings.HasPrefix(c, "050471") {
		t.Errorf("TOTP at 1111111111 = %q, want 050471 prefix", c)
	}
}

func rfc3339MillisUnix() string {
	return "1750000000000" // a fixed future epoch ms (2025-06-15)
}

// TestAuthClientSendsNoToken verifies the token-less auth client never
// attaches credential headers on the login endpoints (#62).
func TestAuthClientSendsNoToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	client, err := NewForAuth(testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.StartCLILogin(context.Background()); err != nil {
		t.Fatal(err)
	}
	// There is no request-body recording for the auth route; assert via the
	// recorded session route on a harmless probe instead — the auth client
	// must not carry tokens anywhere.
	if client.token != "" {
		t.Errorf("auth client token = %q, want empty", client.token)
	}
}

// TestInjectEnvelopeSanitizesForeignPromptMarkers verifies that system messages
// containing foreign harness markers (e.g. Claude Code identity and billing headers)
// have those markers stripped/sanitized while ensuring the canonical Buffy prefix
// opens at byte 0 and preserving custom user instructions.
func TestInjectEnvelopeSanitizesForeignPromptMarkers(t *testing.T) {
	t.Run("string content with claude code identity and billing headers", func(t *testing.T) {
		rawBody := `{
			"model": "anthropic/claude-3-7-sonnet",
			"messages": [
				{
					"role": "system",
					"content": "You are Claude Code, Anthropic's official CLI for Claude. Billing info: cc_version=1.0.0; cc_entrypoint=cli. Follow user instructions carefully."
				},
				{
					"role": "user",
					"content": "Hello world"
				}
			]
		}`
		out, err := injectEnvelope([]byte(rawBody), "free", ChatOptions{RunID: "r-1", ClientID: "cid1234567890"})
		if err != nil {
			t.Fatalf("injectEnvelope error: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatalf("unmarshal output: %v", err)
		}
		msgs, ok := payload["messages"].([]any)
		if !ok || len(msgs) < 2 {
			t.Fatalf("unexpected messages: %v", payload["messages"])
		}
		sysMsg, ok := msgs[0].(map[string]any)
		if !ok || sysMsg["role"] != "system" {
			t.Fatalf("first message is not system: %v", msgs[0])
		}
		content, ok := sysMsg["content"].(string)
		if !ok {
			t.Fatalf("system content is not string: %T %v", sysMsg["content"], sysMsg["content"])
		}

		// Must open with canonical Buffy prefix at byte 0
		if !hasCanonicalOpening(content) {
			t.Errorf("system content %q does not have canonical opening", content)
		}
		if !strings.HasPrefix(content, cliSystemMarkerPhrase) {
			t.Errorf("system content %q does not start with cliSystemMarkerPhrase", content)
		}

		// Must NOT contain any foreign prompt markers
		for _, marker := range foreignHarnessPromptMarkers {
			if strings.Contains(content, marker) {
				t.Errorf("system content %q still contains foreign marker %q", content, marker)
			}
		}

		// Remainder of user instructions must be preserved
		if !strings.Contains(content, "Follow user instructions carefully.") {
			t.Errorf("user instructions were lost from system content: %q", content)
		}
	})

	t.Run("array parts content with foreign markers", func(t *testing.T) {
		rawBody := `{
			"model": "anthropic/claude-3-7-sonnet",
			"messages": [
				{
					"role": "system",
					"content": [
						{
							"type": "text",
							"text": "You are Claude Code, Anthropic's official CLI."
						},
						{
							"type": "text",
							"text": "System config: cc_version=2.0; cc_entrypoint=agent. Keep code clean."
						}
					]
				},
				{
					"role": "user",
					"content": "Refactor this"
				}
			]
		}`
		out, err := injectEnvelope([]byte(rawBody), "free", ChatOptions{RunID: "r-2", ClientID: "cid1234567890"})
		if err != nil {
			t.Fatalf("injectEnvelope error: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatalf("unmarshal output: %v", err)
		}
		msgs, ok := payload["messages"].([]any)
		if !ok || len(msgs) < 2 {
			t.Fatalf("unexpected messages: %v", payload["messages"])
		}
		sysMsg, ok := msgs[0].(map[string]any)
		if !ok || sysMsg["role"] != "system" {
			t.Fatalf("first message is not system: %v", msgs[0])
		}
		parts, ok := sysMsg["content"].([]any)
		if !ok || len(parts) == 0 {
			t.Fatalf("system content is not non-empty array: %T %v", sysMsg["content"], sysMsg["content"])
		}

		// First part must have canonical Buffy prefix
		firstPart, ok := parts[0].(map[string]any)
		if !ok {
			t.Fatalf("first part is not map: %v", parts[0])
		}
		firstText, _ := firstPart["text"].(string)
		if !hasCanonicalOpening(firstText) {
			t.Errorf("first part text %q does not have canonical opening", firstText)
		}

		// All parts must be free of foreign markers
		for i, p := range parts {
			pMap, ok := p.(map[string]any)
			if !ok {
				continue
			}
			txt, _ := pMap["text"].(string)
			for _, marker := range foreignHarnessPromptMarkers {
				if strings.Contains(txt, marker) {
					t.Errorf("part %d text %q still contains foreign marker %q", i, txt, marker)
				}
			}
		}

		// Instruction preserved
		lastPart, _ := parts[len(parts)-1].(map[string]any)
		lastText, _ := lastPart["text"].(string)
		if !strings.Contains(lastText, "Keep code clean.") {
			t.Errorf("instructions lost from last part: %q", lastText)
		}
	})

	t.Run("canonical opening already present with foreign marker later in text", func(t *testing.T) {
		rawBody := `{
			"model": "anthropic/claude-3-7-sonnet",
			"messages": [
				{
					"role": "system",
					"content": "You are Buffy, the strategic coding agent behind Codebuff.\n\nContext: You are Claude Code running tools."
				}
			]
		}`
		out, err := injectEnvelope([]byte(rawBody), "free", ChatOptions{RunID: "r-3", ClientID: "cid1234567890"})
		if err != nil {
			t.Fatalf("injectEnvelope error: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatalf("unmarshal output: %v", err)
		}
		msgs := payload["messages"].([]any)
		sysMsg := msgs[0].(map[string]any)
		content := sysMsg["content"].(string)

		if !hasCanonicalOpening(content) {
			t.Errorf("canonical opening missing: %q", content)
		}
		if strings.Contains(content, "You are Claude Code") {
			t.Errorf("foreign marker was not stripped: %q", content)
		}
		if !strings.Contains(content, "running tools.") {
			t.Errorf("instruction lost: %q", content)
		}
	})

	t.Run("all new universal markers scrubbed from string content", func(t *testing.T) {
		newMarkers := []string{
			"You are Kimi Code CLI",
			"You are Hermes Agent, built by Nous Research",
			"You are a general-purpose AI agent called goose",
			"You are an expert on the AI coding tool called Aider",
			"Gemini CLI",
			"Generated with Crush",
			"Assisted-by: Crush",
			"Co-Authored-By: Crush",
			"Co-Authored-By: Claude Code",
			"*** Begin Patch",
			"*** End Patch",
		}
		var sb strings.Builder
		sb.WriteString("Session preamble. ")
		for _, m := range newMarkers {
			sb.WriteString("Marker[" + m + "] ")
		}
		sb.WriteString("Follow user instructions carefully.")
		body, err := json.Marshal(map[string]any{
			"model": "openai/gpt-5",
			"messages": []any{
				map[string]any{"role": "system", "content": sb.String()},
				map[string]any{"role": "user", "content": "Hello world"},
			},
		})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		out, err := injectEnvelope(body, "free", ChatOptions{RunID: "r-4", ClientID: "cid1234567890"})
		if err != nil {
			t.Fatalf("injectEnvelope error: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatalf("unmarshal output: %v", err)
		}
		msgs, ok := payload["messages"].([]any)
		if !ok || len(msgs) < 2 {
			t.Fatalf("unexpected messages: %v", payload["messages"])
		}
		sysMsg, ok := msgs[0].(map[string]any)
		if !ok || sysMsg["role"] != "system" {
			t.Fatalf("first message is not system: %v", msgs[0])
		}
		content, ok := sysMsg["content"].(string)
		if !ok {
			t.Fatalf("system content is not string: %T %v", sysMsg["content"], sysMsg["content"])
		}
		if !strings.HasPrefix(content, cliSystemMarkerPhrase) {
			t.Errorf("system content %q does not start with cliSystemMarkerPhrase", content)
		}
		for _, marker := range foreignHarnessPromptMarkers {
			if strings.Contains(content, marker) {
				t.Errorf("system content %q still contains foreign marker %q", content, marker)
			}
		}
		if !strings.Contains(content, "Follow user instructions carefully.") {
			t.Errorf("user instructions were lost from system content: %q", content)
		}
	})

	t.Run("all new universal markers scrubbed from parts-array content", func(t *testing.T) {
		newMarkers := []string{
			"You are Kimi Code CLI",
			"You are Hermes Agent, built by Nous Research",
			"You are a general-purpose AI agent called goose",
			"You are an expert on the AI coding tool called Aider",
			"Gemini CLI",
			"Generated with Crush",
			"Assisted-by: Crush",
			"Co-Authored-By: Crush",
			"Co-Authored-By: Claude Code",
			"*** Begin Patch",
			"*** End Patch",
		}
		parts := make([]any, 0, len(newMarkers)+1)
		for _, m := range newMarkers {
			parts = append(parts, map[string]any{"type": "text", "text": "Note: " + m + " applies."})
		}
		parts = append(parts, map[string]any{"type": "text", "text": "Keep code clean."})
		body, err := json.Marshal(map[string]any{
			"model": "openai/gpt-5",
			"messages": []any{
				map[string]any{"role": "system", "content": parts},
				map[string]any{"role": "user", "content": "Refactor this"},
			},
		})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		out, err := injectEnvelope(body, "free", ChatOptions{RunID: "r-5", ClientID: "cid1234567890"})
		if err != nil {
			t.Fatalf("injectEnvelope error: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatalf("unmarshal output: %v", err)
		}
		msgs, ok := payload["messages"].([]any)
		if !ok || len(msgs) < 2 {
			t.Fatalf("unexpected messages: %v", payload["messages"])
		}
		sysMsg, ok := msgs[0].(map[string]any)
		if !ok || sysMsg["role"] != "system" {
			t.Fatalf("first message is not system: %v", msgs[0])
		}
		gotParts, ok := sysMsg["content"].([]any)
		if !ok || len(gotParts) == 0 {
			t.Fatalf("system content is not non-empty array: %T %v", sysMsg["content"], sysMsg["content"])
		}
		firstPart, ok := gotParts[0].(map[string]any)
		if !ok {
			t.Fatalf("first part is not map: %v", gotParts[0])
		}
		if firstText, _ := firstPart["text"].(string); !hasCanonicalOpening(firstText) {
			t.Errorf("first part text %q does not have canonical opening", firstText)
		}
		for i, p := range gotParts {
			pMap, ok := p.(map[string]any)
			if !ok {
				continue
			}
			txt, _ := pMap["text"].(string)
			for _, marker := range foreignHarnessPromptMarkers {
				if strings.Contains(txt, marker) {
					t.Errorf("part %d text %q still contains foreign marker %q", i, txt, marker)
				}
			}
		}
		lastPart, _ := gotParts[len(gotParts)-1].(map[string]any)
		if lastText, _ := lastPart["text"].(string); !strings.Contains(lastText, "Keep code clean.") {
			t.Errorf("instructions lost from last part: %q", lastText)
		}
	})

	t.Run("user-role content with markers left untouched", func(t *testing.T) {
		userText := "My notes mention You are Kimi Code CLI, *** Begin Patch, Generated with Crush, and Co-Authored-By: Claude Code verbatim."
		body, err := json.Marshal(map[string]any{
			"model": "openai/gpt-5",
			"messages": []any{
				map[string]any{"role": "system", "content": "Custom instructions here."},
				map[string]any{"role": "user", "content": userText},
			},
		})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		out, err := injectEnvelope(body, "free", ChatOptions{RunID: "r-6", ClientID: "cid1234567890"})
		if err != nil {
			t.Fatalf("injectEnvelope error: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatalf("unmarshal output: %v", err)
		}
		msgs, ok := payload["messages"].([]any)
		if !ok || len(msgs) != 2 {
			t.Fatalf("unexpected messages: %v", payload["messages"])
		}
		if got := msgs[1].(map[string]any)["content"]; got != userText {
			t.Errorf("user content rewritten: %q, want %q", got, userText)
		}
		sysContent, ok := msgs[0].(map[string]any)["content"].(string)
		if !ok {
			t.Fatalf("system content is not string: %T", msgs[0].(map[string]any)["content"])
		}
		if !strings.HasPrefix(sysContent, cliSystemMarkerPhrase) {
			t.Errorf("system content %q does not start with cliSystemMarkerPhrase", sysContent)
		}
		if !strings.Contains(sysContent, "Custom instructions here.") {
			t.Errorf("system instructions lost: %q", sysContent)
		}
	})

	t.Run("canonical buffy opening skips prepend without duplication", func(t *testing.T) {
		rawBody := `{
			"model": "openai/gpt-5",
			"messages": [
				{
					"role": "system",
					"content": "You are Buffy, the strategic coding agent behind Codebuff.\n\nCustom persona with Gemini CLI mentioned later."
				}
			]
		}`
		out, err := injectEnvelope([]byte(rawBody), "free", ChatOptions{RunID: "r-7", ClientID: "cid1234567890"})
		if err != nil {
			t.Fatalf("injectEnvelope error: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatalf("unmarshal output: %v", err)
		}
		msgs, ok := payload["messages"].([]any)
		if !ok || len(msgs) != 1 {
			t.Fatalf("messages = %v, want single system message (no unshift)", payload["messages"])
		}
		sysMsg, ok := msgs[0].(map[string]any)
		if !ok || sysMsg["role"] != "system" {
			t.Fatalf("first message is not system: %v", msgs[0])
		}
		content, ok := sysMsg["content"].(string)
		if !ok {
			t.Fatalf("system content is not string: %T", sysMsg["content"])
		}
		if !hasCanonicalOpening(content) {
			t.Errorf("canonical opening missing: %q", content)
		}
		if n := strings.Count(content, "You are Buffy, the strategic coding agent behind Codebuff."); n != 1 {
			t.Errorf("canonical opening appears %d times, want exactly 1 (no prepend duplication)", n)
		}
		if strings.Contains(content, "Gemini CLI") {
			t.Errorf("foreign marker was not stripped: %q", content)
		}
		if !strings.Contains(content, "Custom persona with") {
			t.Errorf("instruction lost: %q", content)
		}
	})
}
