package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
)

func TestSessionControlCalls(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	client, _ := New("tok", testConfig(mock.URL(), nil))

	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "active" || st.InstanceID != "inst-abc-123" {
		t.Fatalf("create state = %+v", st)
	}
	if st.ExpiresAt.IsZero() {
		t.Error("expiresAt not parsed")
	}

	// poll requires instance header
	polled, err := client.GetSession(context.Background(), "inst-abc-123")
	if err != nil {
		t.Fatal(err)
	}
	if polled.Status != "active" {
		t.Errorf("poll status = %q", polled.Status)
	}

	// end + tolerated 404 (DELETE carries the held instance id).
	if _, err := client.EndSession(context.Background(), "inst-abc-123"); err != nil {
		t.Fatal(err)
	}
}

func TestSessionParseAccessTier(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.AccessTier = "limited"

	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.AccessTier != "limited" {
		t.Fatalf("AccessTier = %q, want limited", st.AccessTier)
	}
}

// TestSessionParseSubscriptionTierID pins the subscription.tierId
// passthrough: a present tier survives verbatim, while an explicit null and
// an absent block both leave the state's tier empty (never an error).
func TestSessionParseSubscriptionTierID(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"present", `{"status":"active","instanceId":"i","subscription":{"tierId":"plan_pro_max"}}`, "plan_pro_max"},
		{"null", `{"status":"active","instanceId":"i","subscription":null}`, ""},
		{"absent", `{"status":"active","instanceId":"i"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}
			client, err := New("tok", testConfig(mock.URL(), nil))
			if err != nil {
				t.Fatal(err)
			}
			st, err := client.CreateSession(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if st.SubscriptionTierID != tc.want {
				t.Errorf("SubscriptionTierID = %q, want %q", st.SubscriptionTierID, tc.want)
			}
		})
	}
}

// TestProbeAccount verifies the zero-cost token probe: a GET
// /api/v1/freebuff/session with NO instance header that claims no session
// slot, returns the live per-model quota, and classifies
// auth/ban/region/transport failures through the standard matrix.
func TestProbeAccount(t *testing.T) {
	t.Run("200 with quota", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()

		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		st, err := client.ProbeAccount(context.Background())
		if err != nil {
			t.Fatalf("ProbeAccount: %v", err)
		}
		if st.Status != "active" || st.InstanceID != "inst-abc-123" {
			t.Fatalf("probe state = %+v", st)
		}
		q, ok := st.RateLimitsByModel["deepseek/deepseek-v4-flash"]
		if !ok {
			t.Fatalf("RateLimitsByModel missing flash quota: %+v", st.RateLimitsByModel)
		}
		if q.Limit != 6 || q.RecentCount != 2 {
			t.Errorf("quota limit/recentCount = %v/%v, want 6/2", q.Limit, q.RecentCount)
		}
		if q.Period != "pacific_day" {
			t.Errorf("period = %q, want pacific_day", q.Period)
		}
		if q.ResetAt.IsZero() {
			t.Error("resetAt not parsed")
		}
		// A probe must not claim a session slot (no POST).
		if got := mock.SessionCreatesSnapshot(); got != 0 {
			t.Errorf("session creates = %d, want 0 (probe is zero-cost)", got)
		}
		if got := mock.SessionProbesSnapshot(); got != 1 {
			t.Errorf("session probes = %d, want 1", got)
		}
	})

	t.Run("404 maps to ErrNoActiveSession", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"error":"session not found"}`)
		}

		client, _ := New("tok", testConfig(mock.URL(), nil))
		_, err := client.ProbeAccount(context.Background())
		if !errors.Is(err, ErrNoActiveSession) {
			t.Fatalf("err = %v, want ErrNoActiveSession", err)
		}
	})

	t.Run("200 ended maps to ErrNoActiveSession", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"status":"ended"}`)
		}

		client, _ := New("tok", testConfig(mock.URL(), nil))
		_, err := client.ProbeAccount(context.Background())
		if !errors.Is(err, ErrNoActiveSession) {
			t.Fatalf("err = %v, want ErrNoActiveSession", err)
		}
	})

	t.Run("401 auth rejected", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.AuthReject = true

		client, _ := New("tok", testConfig(mock.URL(), nil))
		_, err := client.ProbeAccount(context.Background())
		if !errors.Is(err, ErrAuthRejected) {
			t.Fatalf("err = %v, want ErrAuthRejected", err)
		}
	})

	t.Run("403 banned", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.Ban = true

		client, _ := New("tok", testConfig(mock.URL(), nil))
		_, err := client.ProbeAccount(context.Background())
		if !errors.Is(err, ErrBanned) {
			t.Fatalf("err = %v, want ErrBanned", err)
		}
	})

	t.Run("403 country blocked", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"status":"country_blocked","countryCode":"CN","countryBlockReason":"region_restricted","ipPrivacySignals":["vpn"]}`)
		}

		client, _ := New("tok", testConfig(mock.URL(), nil))
		_, err := client.ProbeAccount(context.Background())
		if !errors.Is(err, ErrCountryBlocked) {
			t.Fatalf("err = %v, want ErrCountryBlocked", err)
		}
		var cbe *CountryBlockedError
		if !errors.As(err, &cbe) {
			t.Fatalf("err = %T, want *CountryBlockedError", err)
		}
		if cbe.CountryCode != "CN" {
			t.Errorf("countryCode = %q, want CN", cbe.CountryCode)
		}
	})

	t.Run("transport error", func(t *testing.T) {
		mock := testutil.NewMock()
		url := mock.URL()
		mock.Close()

		client, _ := New("tok", testConfig(url, nil))
		_, err := client.ProbeAccount(context.Background())
		if err == nil {
			t.Fatal("ProbeAccount returned nil error for closed server")
			return
		}
	})
}

// TestSessionCallParsesRateLimitsByModel verifies the live per-model quota
// map from an admission response is parsed into SessionState, including the
// nested entitlement breakdown and flex-time resetAt.
func TestSessionCallParsesRateLimitsByModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimitsByModel = map[string]any{
		"z-ai/glm-5.2": map[string]any{
			"model":       "z-ai/glm-5.2",
			"limit":       5,
			"recentCount": 4,
			"period":      "pacific_day",
			"resetAt":     "2026-08-16T07:00:00.000Z",
			"entitlementBreakdown": map[string]any{
				"base":     1,
				"referral": 1,
				"streak":   3,
			},
		},
	}

	client, _ := New("tok", testConfig(mock.URL(), nil))
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	q, ok := st.RateLimitsByModel["z-ai/glm-5.2"]
	if !ok {
		t.Fatalf("RateLimitsByModel missing model z-ai/glm-5.2: %+v", st.RateLimitsByModel)
	}
	if q.Limit != 5 || q.RecentCount != 4 {
		t.Errorf("quota limit/recentCount = %v/%v, want 5/4", q.Limit, q.RecentCount)
	}
	if q.Period != "pacific_day" {
		t.Errorf("period = %q, want pacific_day", q.Period)
	}
	if q.ResetAt.IsZero() {
		t.Error("resetAt not parsed")
	} else if want := "2026-08-16T07:00:00Z"; q.ResetAt.UTC().Format(time.RFC3339) != want {
		t.Errorf("resetAt = %s, want %s", q.ResetAt.UTC().Format(time.RFC3339), want)
	}
	if q.Entitlement["base"] != 1 || q.Entitlement["referral"] != 1 || q.Entitlement["streak"] != 3 {
		t.Errorf("entitlement = %+v, want base=1 referral=1 streak=3", q.Entitlement)
	}
	if q.Model != "z-ai/glm-5.2" {
		t.Errorf("quota model = %q", q.Model)
	}
}

func TestSession404Mapping(t *testing.T) {
	// A create hitting 404/405 on the dedicated admission route means the
	// server predates the route's guarantees: fail closed with
	// ErrSessionAdmissionUnsupported, never "disabled" (vendor af898dc,
	// freebuff-session-api.ts session_admission_unsupported).
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "404"

	client, _ := New("tok", testConfig(mock.URL(), nil))
	_, err := client.CreateSession(context.Background())
	if !errors.Is(err, ErrSessionAdmissionUnsupported) {
		t.Errorf("create 404 err = %v, want ErrSessionAdmissionUnsupported", err)
	}

	// A poll 404 means the session vanished upstream (expired/evicted) →
	// ended (recreate path), NOT a permanent disabled (which the session
	// manager would cache with no expiry, disabling the token forever).
	polled, err := client.GetSession(context.Background(), "inst-gone")
	if err != nil {
		t.Fatal(err)
	}
	if polled.Status != "ended" {
		t.Errorf("poll 404 status = %q, want ended", polled.Status)
	}
}

func TestQueuedSessionParsing(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "queued"
	mock.QueuePosition = 4
	mock.QueueDepth = 9
	mock.EstimatedWaitMs = 0

	client, _ := New("tok", testConfig(mock.URL(), nil))
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "queued" || st.Position != 4 || st.QueueDepth != 9 {
		t.Fatalf("queued state = %+v", st)
	}
	if st.PollAt.IsZero() {
		t.Error("pollAt not parsed")
	}
}

func TestStartAndFinishRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	client, _ := New("tok", testConfig(mock.URL(), nil))

	runID, err := client.StartRun(context.Background(), "base2-free-deepseek-flash")
	if err != nil {
		t.Fatal(err)
	}
	if runID != "run-0001" {
		t.Errorf("runID = %q", runID)
	}
	if len(mock.StartedRuns) != 1 || mock.StartedRuns[0] != "base2-free-deepseek-flash" {
		t.Errorf("START not recorded: %v", mock.StartedRuns)
	}

	msg1 := "msg-1"
	// Step ids are UUIDs, like the CLI's crypto.randomUUID() per step — the
	// vendor schema (pendingAgentStepSchema id: z.string().uuid()) rejects
	// anything else, so non-UUID fixtures would let a bad FINISH shape pass.
	steps := []RunStep{
		{ID: "2f1a9c3e-5b7d-4a11-8f2e-6c0d9b4a7e31", StepNumber: 1, MessageID: &msg1, Status: "completed", StartTime: "2026-08-18T00:00:00.000Z"},
		{ID: "9c4b2e77-1d38-4f6a-9c05-73e8a1b2d4f6", StepNumber: 2, Status: "completed", StartTime: "2026-08-18T00:00:01.000Z"},
	}
	if err := client.FinishRun(context.Background(), runID, "completed", len(steps), steps, ""); err != nil {
		t.Fatal(err)
	}
	if len(mock.FinishedRuns) != 1 {
		t.Fatalf("FINISH not recorded: %v", mock.FinishedRuns)
	}
	f := mock.FinishedRuns[0]
	if f.RunID != runID || f.Status != "completed" || f.TotalSteps != 2 {
		t.Errorf("FINISH payload = %+v", f)
	}
	// Issue #114: steps ride IN the FINISH payload (the CLI has no /steps
	// endpoint) with the CLI step shape: id, stepNumber, messageId
	// (null-able), status, startTime.
	if len(f.Steps) != 2 || f.Steps[0].StepNumber != 1 || f.Steps[0].MessageID == nil || *f.Steps[0].MessageID != "msg-1" ||
		f.Steps[1].StepNumber != 2 || f.Steps[1].MessageID != nil || f.Steps[1].StartTime == "" {
		t.Errorf("FINISH steps = %+v, want 2 CLI-shaped steps", f.Steps)
	}
}

// TestFinishRunStepZeroValuesVerbatim pins the live-captured free-tier step
// encoding (2026-09-27, docs/LIVE-CAPTURE.md): the CLI sends "credits":0
// and "childRunIds":[] verbatim — never elided or null.
func TestFinishRunStepZeroValuesVerbatim(t *testing.T) {
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent-runs" {
			raw, _ = io.ReadAll(r.Body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true}`)
	}))
	t.Cleanup(srv.Close)
	client, _ := New("tok", testConfig(srv.URL, nil))
	steps := []RunStep{
		{ID: "2f1a9c3e-5b7d-4a11-8f2e-6c0d9b4a7e31", StepNumber: 1, ChildRunIDs: []string{}, Status: "completed", StartTime: "2026-08-18T00:00:00.000Z"},
	}
	if err := client.FinishRun(context.Background(), "run-1", "completed", 1, steps, ""); err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{`"credits":0`, `"childRunIds":[]`, `"status":"completed"`} {
		if !strings.Contains(body, want) {
			t.Errorf("FINISH body missing %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, `"childRunIds":null`) {
		t.Errorf("FINISH body carries null childRunIds, want []:\n%s", body)
	}
}

func TestFinishRunErrorTruncation(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	client, _ := New("tok", testConfig(mock.URL(), nil))

	// errorMessage must be truncated to 5000 runes (CLI parity:
	// truncateString(errorMessage, 5000) in database.ts) — a full Go stack
	// trace must not blow the cap.
	long := strings.Repeat("エ", 6000)
	if err := client.FinishRun(context.Background(), "run-0001", "failed", 0, nil, long); err != nil {
		t.Fatal(err)
	}
	finished := mock.FinishedRunsSnapshot()
	if len(finished) != 1 || finished[0].RunID != "run-0001" || finished[0].Status != "failed" {
		t.Fatalf("finished runs = %+v, want run-0001 failed", finished)
	}
	if got := len([]rune(finished[0].ErrorMessage)); got != 5000 {
		t.Errorf("errorMessage runes = %d, want 5000 (truncated)", got)
	}
}

func TestControlCallTimeout(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Hang the session create; the 50ms control timeout must win even when
	// the caller passes a much longer deadline (the control timeout is the
	// tighter bound and must never be defeated by the caller's context).
	mock.SessionCreateDelay = 10 * time.Second

	client, _ := New("tok", testConfig(mock.URL(), func(c *config.Config) { c.SessionCallTimeout = 50 * time.Millisecond }))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := client.CreateSession(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
}

func TestCreateSessionForModelHeaders(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var gotMethod, gotPath string
	var capturedHeaders http.Header
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		capturedHeaders = r.Header.Clone()
		model := r.Header.Get("x-freebuff-model")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1","model":"`+model+`","expiresAt":"2030-01-01T00:00:00Z"}`)
	}
	client, err := New("tok-a", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.CreateSessionForModel(context.Background(), "thudm/glm-5.2")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "active" || st.Model != "thudm/glm-5.2" || st.InstanceID != "inst-1" {
		t.Errorf("got %+v, want active with model thudm/glm-5.2", st)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/freebuff/session/admission" {
		t.Errorf("request = %s %q, want POST /api/v1/freebuff/session/admission", gotMethod, gotPath)
	}
	instID := capturedHeaders.Get("x-freebuff-instance-id")
	uuid := strings.TrimPrefix(instID, "cli:")
	if !strings.HasPrefix(instID, "cli:") || !regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(uuid) {
		t.Errorf("x-freebuff-instance-id = %q, want cli:<RFC4122-v4 UUID>", instID)
	}
	for _, name := range []string{"x-freebuff-multi-session", "x-freebuff-purchase-continuity"} {
		if got := capturedHeaders.Get(name); got != "1" {
			t.Errorf("%s = %q, want '1'", name, got)
		}
	}
	if got := capturedHeaders.Get("x-freebuff-desktop-attempt-id"); got != uuid || uuid == "" {
		t.Errorf("x-freebuff-desktop-attempt-id = %q, want instance ID suffix %q", got, uuid)
	}
	if got := capturedHeaders.Get("x-freebuff-first-tab-discount"); got != "0" {
		t.Errorf("x-freebuff-first-tab-discount = %q, want '0'", got)
	}
	if got := capturedHeaders.Get("x-freebuff-wallet-spend-limit"); got != "0" {
		t.Errorf("x-freebuff-wallet-spend-limit = %q, want '0'", got)
	}
}

func TestLimitedOfferAdmissionUsesLegacySessionHeaders(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var gotMethod, gotPath string
	var capturedHeaders http.Header
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		capturedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1","expiresAt":"2030-01-01T00:00:00Z"}`)
	}
	client, err := New("tok-a", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateSessionForModel(context.Background(), "anthropic/claude-fable-5.1"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/freebuff/session/admission" {
		t.Errorf("request = %s %q, want POST /api/v1/freebuff/session/admission", gotMethod, gotPath)
	}
	for _, name := range []string{
		"x-freebuff-instance-id",
		"x-freebuff-multi-session",
		"x-freebuff-purchase-continuity",
		"x-freebuff-desktop-attempt-id",
	} {
		if got := capturedHeaders.Get(name); got != "" {
			t.Errorf("%s = %q, want absent for limited offer model", name, got)
		}
	}
}

// TestAdmissionPostOmitsTakeoverHeader pins U1 (vendor tip 57943aa71,
// common/src/constants/freebuff-models.ts:3481-3482): the vendor sends
// x-freebuff-takeover-instance-id on the admission POST only for Desktop's
// explicit "Use it here" — naming the single-slot holder the rejection just
// identified. The proxy NEVER sends it: there is no user-confirmed holder,
// and the superseded path is terminal (auto-takeover risks ping-pong), so the
// NEXT request re-joins fresh without naming a holder.
func TestAdmissionPostOmitsTakeoverHeader(t *testing.T) {
	for _, model := range []string{"thudm/glm-5.2", "anthropic/claude-fable-5.1", ""} {
		t.Run("model="+model, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			var captured http.Header
			mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
				captured = r.Header.Clone()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1","expiresAt":"2030-01-01T00:00:00Z"}`)
			}
			client, err := New("tok-a", testConfig(mock.URL(), nil))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.CreateSessionForModel(context.Background(), model); err != nil {
				t.Fatal(err)
			}
			if got := captured.Get("x-freebuff-takeover-instance-id"); got != "" {
				t.Errorf("x-freebuff-takeover-instance-id = %q, want absent (no user-confirmed holder; never auto-takeover)", got)
			}
		})
	}
}

func TestGetSessionWithOptsHeaders(t *testing.T) {
	for _, compact := range []bool{false, true} {
		name := "non-compact"
		if compact {
			name = "compact"
		}
		t.Run(name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			var gotMethod, gotPath string
			var capturedHeaders http.Header
			mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				capturedHeaders = r.Header.Clone()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1","expiresAt":"2030-01-01T00:00:00Z"}`)
			}
			client, err := New("tok-a", testConfig(mock.URL(), nil))
			if err != nil {
				t.Fatal(err)
			}
			st, err := client.GetSessionWithOpts(context.Background(), "cli:123e4567-e89b-42d3-a456-426614174000", compact)
			if err != nil {
				t.Fatal(err)
			}
			if st.Status != "active" {
				t.Errorf("status = %q, want active", st.Status)
			}
			if gotMethod != http.MethodGet || gotPath != "/api/v1/freebuff/session" {
				t.Errorf("request = %s %q, want GET /api/v1/freebuff/session", gotMethod, gotPath)
			}
			if got := capturedHeaders.Get("x-freebuff-instance-id"); got != "cli:123e4567-e89b-42d3-a456-426614174000" {
				t.Errorf("x-freebuff-instance-id = %q, want supplied cli: instance", got)
			}
			for _, name := range []string{"x-freebuff-multi-session", "x-freebuff-purchase-continuity", "x-freebuff-heartbeat"} {
				if got := capturedHeaders.Get(name); got != "1" {
					t.Errorf("%s = %q, want '1'", name, got)
				}
			}
			if compact {
				if got := capturedHeaders.Get("x-freebuff-compact-session"); got != "1" {
					t.Errorf("x-freebuff-compact-session = %q, want '1'", got)
				}
				if got := capturedHeaders.Get("x-freebuff-include-unused-rate-limits"); got != "" {
					t.Errorf("x-freebuff-include-unused-rate-limits = %q, want absent on compact GET", got)
				}
			} else {
				if got := capturedHeaders.Get("x-freebuff-compact-session"); got != "" {
					t.Errorf("x-freebuff-compact-session = %q, want absent", got)
				}
				if got := capturedHeaders.Get("x-freebuff-include-unused-rate-limits"); got != "1" {
					t.Errorf("x-freebuff-include-unused-rate-limits = %q, want '1'", got)
				}
			}
		})
	}
}

func TestSessionCallStructured4xx(t *testing.T) {
	cases := []struct {
		name                   string
		statusCode             int
		body                   string
		wantStatus             string
		wantUpdateRequired     bool
		wantPurchasesPaused    bool
		wantLimitedOfferReason string
	}{
		{
			name:       "model_locked 409",
			statusCode: http.StatusConflict,
			body:       `{"status":"model_locked","currentModel":"deepseek/deepseek-v4-flash","requestedModel":"thudm/glm-5.2"}`,
			wantStatus: "model_locked",
		},
		{
			name:       "model_unavailable 409",
			statusCode: http.StatusConflict,
			body:       `{"status":"model_unavailable","requestedModel":"thudm/glm-5.2","availableHours":"08:00-20:00"}`,
			wantStatus: "model_unavailable",
		},
		{
			// Vendor 3f00c77: Desktop multi-session refusal for a client
			// build too old to rotate a purchase claim (the model is fine).
			name:               "model_unavailable with updateRequired",
			statusCode:         http.StatusConflict,
			body:               `{"status":"model_unavailable","requestedModel":"thudm/glm-5.2","availableHours":"Update Freebuff Desktop to resume your purchased hour.","updateRequired":true}`,
			wantStatus:         "model_unavailable",
			wantUpdateRequired: true,
		},
		{
			// Vendor 3f00c77: Desktop multi-session refusal while minting
			// new purchased sessions is paused server-side.
			name:                "model_unavailable with purchasesPaused",
			statusCode:          http.StatusConflict,
			body:                `{"status":"model_unavailable","requestedModel":"thudm/glm-5.2","availableHours":"Purchased Desktop sessions are temporarily unavailable.","purchasesPaused":true}`,
			wantStatus:          "model_unavailable",
			wantPurchasesPaused: true,
		},
		{
			// Vendor e2b911e: capacity-limited offer trial refusal carries
			// the reason the trial cannot be joined (used|closed|exhausted).
			name:                   "model_unavailable with limitedOfferReason",
			statusCode:             http.StatusConflict,
			body:                   `{"status":"model_unavailable","requestedModel":"anthropic/claude-fable-5.1","availableHours":"Campaign ended.","limitedOfferReason":"used"}`,
			wantStatus:             "model_unavailable",
			wantLimitedOfferReason: "used",
		},
		{
			name:       "ip_capped 429",
			statusCode: http.StatusTooManyRequests,
			body:       `{"status":"ip_capped","activeUsersForIp":5,"limit":4,"retryAfterMs":30000}`,
			wantStatus: "ip_capped",
		},
		{
			name:       "spend_limited 429",
			statusCode: http.StatusTooManyRequests,
			body:       `{"status":"spend_limited","message":"Daily budget reached","retryAfterMs":60000}`,
			wantStatus: "spend_limited",
		},
		{
			name:       "country_blocked 403",
			statusCode: http.StatusForbidden,
			body:       `{"status":"country_blocked","countryCode":"CN","countryBlockReason":"country_not_allowed"}`,
			wantStatus: "country_blocked",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.statusCode)
				_, _ = io.WriteString(w, tc.body)
			}

			client, err := New("tok-a", testConfig(mock.URL(), nil))
			if err != nil {
				t.Fatal(err)
			}

			st, err := client.CreateSession(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if st.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", st.Status, tc.wantStatus)
			}
			if st.UpdateRequired != tc.wantUpdateRequired {
				t.Errorf("UpdateRequired = %v, want %v", st.UpdateRequired, tc.wantUpdateRequired)
			}
			if st.PurchasesPaused != tc.wantPurchasesPaused {
				t.Errorf("PurchasesPaused = %v, want %v", st.PurchasesPaused, tc.wantPurchasesPaused)
			}
			if st.LimitedOfferReason != tc.wantLimitedOfferReason {
				t.Errorf("LimitedOfferReason = %q, want %q", st.LimitedOfferReason, tc.wantLimitedOfferReason)
			}
		})
	}
}

// TestSessionCallUnknownStatus5xx pins current sessionCall behavior (G10):
// any status code with a parseable body carrying a non-empty status field
// yields a SessionState, not an error — even a 5xx with an unknown status.
func TestSessionCallUnknownStatus5xx(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"status":"weird","message":"unknown status"}`)
	}
	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatalf("unexpected error for a parseable 5xx body: %v", err)
	}
	if st.Status != "weird" {
		t.Errorf("status = %q, want weird", st.Status)
	}
}

// TestEndSession404Tolerated guards the EndSession 404 contract (E2E flow
// 10): a 404 DELETE is "nothing to end", not an error, while a 5xx is.
func TestEndSession404Tolerated(t *testing.T) {
	t.Run("404 tolerated", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"session not found"}`)
		}
		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.EndSession(context.Background(), "inst-1"); err != nil {
			t.Errorf("EndSession 404 = %v, want nil", err)
		}
	})

	t.Run("5xx surfaces error", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"boom"}`)
		}
		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.EndSession(context.Background(), "inst-1"); err == nil {
			t.Error("EndSession 500 succeeded, want error")
		}
	})
}

// TestEndSessionInstanceHeader pins the CLI multi-session attempt DELETE and
// preserves the legacy single-session DELETE contract.
func TestEndSessionInstanceHeader(t *testing.T) {
	tests := []struct {
		name       string
		instanceID string
		wantPath   string
		multi      bool
	}{
		{
			name:       "cli attempt uses attempt route",
			instanceID: "cli:123e4567-e89b-42d3-a456-426614174000",
			wantPath:   "/api/v1/freebuff/session/attempt",
			multi:      true,
		},
		{
			name:       "legacy instance uses base route",
			instanceID: "inst-held-1",
			wantPath:   "/api/v1/freebuff/session",
		},
		{
			name:       "empty cli prefix stays legacy",
			instanceID: "cli:",
			wantPath:   "/api/v1/freebuff/session",
		},
		{
			name:     "empty instance uses base route",
			wantPath: "/api/v1/freebuff/session",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			var gotMethod, gotPath string
			var capturedHeaders http.Header
			mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				capturedHeaders = r.Header.Clone()
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"status":"ended"}`)
			}
			client, err := New("tok", testConfig(mock.URL(), nil))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.EndSession(context.Background(), tc.instanceID); err != nil {
				t.Fatal(err)
			}
			if gotMethod != http.MethodDelete || gotPath != tc.wantPath {
				t.Errorf("request = %s %q, want DELETE %q", gotMethod, gotPath, tc.wantPath)
			}
			if got := capturedHeaders.Get("x-freebuff-instance-id"); got != tc.instanceID {
				t.Errorf("x-freebuff-instance-id = %q, want %q", got, tc.instanceID)
			}
			if tc.multi {
				if got := capturedHeaders.Get("x-freebuff-desktop-attempt-id"); got != "123e4567-e89b-42d3-a456-426614174000" {
					t.Errorf("x-freebuff-desktop-attempt-id = %q, want instance ID suffix", got)
				}
				for _, name := range []string{"x-freebuff-multi-session", "x-freebuff-purchase-continuity"} {
					if got := capturedHeaders.Get(name); got != "1" {
						t.Errorf("%s = %q, want '1'", name, got)
					}
				}
			} else {
				for _, name := range []string{"x-freebuff-multi-session", "x-freebuff-purchase-continuity", "x-freebuff-desktop-attempt-id"} {
					if got := capturedHeaders.Get(name); got != "" {
						t.Errorf("%s = %q, want absent for legacy DELETE", name, got)
					}
				}
			}
		})
	}
}

// TestCompactPollAbsentTolerant verifies compact multi-session polls still
// parse responses without quota/offer fields and carry heartbeat.
func TestCompactPollAbsentTolerant(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	gotCompact := make(chan string, 1)
	gotHeartbeat := make(chan string, 1)
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		gotCompact <- r.Header.Get("x-freebuff-compact-session")
		gotHeartbeat <- r.Header.Get("x-freebuff-heartbeat")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1","expiresAt":"2026-08-17T10:00:00.000Z"}`)
	}

	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.GetSessionWithOpts(context.Background(), "cli:123e4567-e89b-42d3-a456-426614174000", true)
	if err != nil {
		t.Fatalf("compact poll: %v", err)
	}
	if st.Status != "active" || st.InstanceID != "inst-1" {
		t.Errorf("state = %+v, want active inst-1", st)
	}
	if st.RateLimitsByModel != nil {
		t.Errorf("RateLimitsByModel = %v, want nil on a compact poll without quotas", st.RateLimitsByModel)
	}
	if got := <-gotCompact; got != "1" {
		t.Errorf("compact header = %q, want 1", got)
	}
	if got := <-gotHeartbeat; got != "1" {
		t.Errorf("heartbeat header = %q, want '1'", got)
	}
}

// TestClassifyCapacityDeferred verifies #75: a free_mode_capacity_deferred
// response classifies as the distinct CapacityDeferredError (retryable
// same-session condition), never a token cooldown or session invalidation.
func TestClassifyCapacityDeferred(t *testing.T) {
	err := classifyError(http.StatusTooManyRequests, `{"error":{"code":"free_mode_capacity_deferred","message":"Free mode is at capacity; your request will be retried automatically"}}`, http.Header{})
	var cde *CapacityDeferredError
	if !errors.As(err, &cde) {
		t.Fatalf("err = %v, want *CapacityDeferredError", err)
	}
	if !errors.Is(err, ErrCapacityDeferred) {
		t.Errorf("err = %v, want ErrCapacityDeferred", err)
	}
	// Unwraps to a Retryable UpstreamError (errors.As finds it), but
	// writeError surfaces 429 free_mode_capacity_deferred + Retry-After
	// via its dedicated CapacityDeferredError branch (#105).
	var ue *UpstreamError
	if !errors.As(err, &ue) || !ue.Retryable {
		t.Errorf("err = %v, want unwrap to Retryable UpstreamError", err)
	}
	if cde.Status != http.StatusTooManyRequests {
		t.Errorf("Status = %d, want 429", cde.Status)
	}
}

// TestClassifyWaitingRoomQueued verifies #81: a 429 waiting_room_queued body
// is a transient admission race (endsTheSession:false) — surfaced as a
// WaitingRoomError, never session-invalid (no session refresh/recreate).
func TestClassifyWaitingRoomQueued(t *testing.T) {
	err := classifyError(http.StatusTooManyRequests, `{"error":{"code":"waiting_room_queued","message":"row caught mid-admit"}}`, http.Header{})
	if errors.Is(err, ErrSessionInvalid) {
		t.Fatal("waiting_room_queued classified as session-invalid, want transient WaitingRoomError")
	}
	var wr *WaitingRoomError
	if !errors.As(err, &wr) {
		t.Fatalf("err = %v, want *WaitingRoomError", err)
	}
}

// TestClassifySessionLimitReached verifies #82: a 409 session_limit_reached
// response is a distinct non-invalid error carrying the code — the ACCOUNT
// is over its concurrent-tab budget but the session row is fine
// (endsTheSession:false), so no session refresh/recreate may trigger.
func TestClassifySessionLimitReached(t *testing.T) {
	err := classifyError(http.StatusConflict, `{"error":{"code":"session_limit_reached","message":"Concurrent tab limit reached"}}`, http.Header{})
	if errors.Is(err, ErrSessionInvalid) {
		t.Fatal("session_limit_reached classified as session-invalid; the row is fine")
	}
	var sle *SessionLimitError
	if !errors.As(err, &sle) {
		t.Fatalf("err = %v, want *SessionLimitError", err)
	}
	if !errors.Is(err, ErrSessionLimitReached) {
		t.Errorf("err = %v, want ErrSessionLimitReached", err)
	}
	if sle.Status != http.StatusConflict {
		t.Errorf("Status = %d, want 409", sle.Status)
	}
}

// TestProbeAccountDoesNotSendIncludeUnusedRateLimits verifies #140: the
// zero-cost GET probe sends NO x-freebuff-include-unused-rate-limits header
// (a third-party-proxy fingerprint the vendored CLI never sends; its session
// GET returns the same response shape without it). The probe carries only
// the standard Authorization + the plain Bun fetch UA (session
// paths are bare Bun traffic in the real CLI), and sessionCall still parses
// glmPromo/rateLimitsByModel when the response includes them.
func TestProbeAccountDoesNotSendIncludeUnusedRateLimits(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var gotHeader, gotAuth, gotUA string
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("x-freebuff-include-unused-rate-limits")
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1","glmPromo":{"dailySessions":2,"endsAt":"2026-08-20T07:00:00.000Z"},"rateLimitsByModel":{"deepseek/deepseek-v4-flash":{"model":"deepseek/deepseek-v4-flash","limit":6,"recentCount":2,"period":"pacific_day","resetAt":"2026-08-18T07:00:00.000Z"}}}`)
	}
	client, err := New("tok-a", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.ProbeAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotHeader != "" {
		t.Errorf("x-freebuff-include-unused-rate-limits = %q, want absent", gotHeader)
	}
	if gotAuth != "Bearer tok-a" {
		t.Errorf("Authorization = %q, want Bearer tok-a", gotAuth)
	}
	if gotUA != bunUserAgent {
		t.Errorf("User-Agent = %q, want %q (Bun fetch default on session paths, no browser persona)", gotUA, bunUserAgent)
	}
	if st.GlmPromo == "" || !strings.Contains(st.GlmPromo, "dailySessions") {
		t.Errorf("GlmPromo = %q, want raw glmPromo JSON", st.GlmPromo)
	}
	if st.RateLimitsByModel == nil || st.RateLimitsByModel["deepseek/deepseek-v4-flash"].Limit != 6 {
		t.Errorf("RateLimitsByModel = %+v, want parsed per-model quota", st.RateLimitsByModel)
	}
}

// TestCreateAdmissionRoute pins the vendor af898dc admission shape: the
// create POST targets the dedicated admission route (never the legacy
// session path) and always carries the wallet spend-limit header at the
// server default, exactly like a CLI POST with no explicit model pick.
func TestCreateAdmissionRoute(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var gotPath, gotSpend, gotModel string
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotSpend = r.Header.Get("x-freebuff-wallet-spend-limit")
		gotModel = r.Header.Get("x-freebuff-model")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1","expiresAt":"2026-09-12T10:00:00.000Z"}`)
	}
	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateSessionForModel(context.Background(), "deepseek/deepseek-v4-flash"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v1/freebuff/session/admission" {
		t.Errorf("create path = %q, want the dedicated admission route", gotPath)
	}
	if gotSpend != "0" {
		t.Errorf("x-freebuff-wallet-spend-limit = %q, want server default 0", gotSpend)
	}
	if gotModel != "deepseek/deepseek-v4-flash" {
		t.Errorf("x-freebuff-model = %q, want requested model", gotModel)
	}
}

// TestCreateAdmissionMethodNotAllowed pins the fail-closed half of the
// admission contract: a 405 (like a 404) means a pre-route server, never a
// cue to retry the legacy path.
func TestCreateAdmissionMethodNotAllowed(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = io.WriteString(w, `{"error":"method not allowed"}`)
	}
	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CreateSession(context.Background())
	if !errors.Is(err, ErrSessionAdmissionUnsupported) {
		t.Errorf("create 405 err = %v, want ErrSessionAdmissionUnsupported", err)
	}
	if err == nil || !strings.Contains(err.Error(), "No purchase was made") {
		t.Errorf("create 405 err = %v, want the verbatim fail-closed copy", err)
	}
}

// TestSessionParsesClaimableGrantAndUpgrade pins the vendor af898dc meter
// additions: claimable earned grants (counted toward canStart, excluded
// from the spendable display) and the upgrade nudge (limited_offer kind).
func TestSessionParsesClaimableGrantAndUpgrade(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1","expiresAt":"2026-09-12T10:00:00.000Z",`+
			`"freebucks":{"balance":1.5,"claimableGrantFreebucks":4.0,"daily":{"limit":10,"spent":8.5,"remaining":1.5},`+
			`"prices":{"deepseek/deepseek-v4-flash":5.0},`+
			`"upgrade":{"kind":"limited_offer","cta":"Get 50% off","tooltip":"Half-price Flash until renewal","modelId":"deepseek/deepseek-v4-flash"}}}`)
	}
	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fb := st.Freebucks
	if fb == nil {
		t.Fatal("Freebucks is nil, want parsed block")
	} else {
		if fb.ClaimableGrant != 4.0 {
			t.Errorf("ClaimableGrant = %v, want 4.0", fb.ClaimableGrant)
		}
		if got := fb.Spendable(); got != 5.5 {
			t.Errorf("Spendable = %v, want 5.5 (balance + claimable)", got)
		}
		if fb.Upgrade == nil {
			t.Fatal("Upgrade is nil, want parsed nudge")
		} else {
			if fb.Upgrade.Kind != "limited_offer" || fb.Upgrade.CTA != "Get 50% off" || fb.Upgrade.ModelID != "deepseek/deepseek-v4-flash" {
				t.Errorf("Upgrade = %+v, want limited_offer nudge for flash", fb.Upgrade)
			}
			if !strings.Contains(fb.Upgrade.Tooltip, "Half-price") {
				t.Errorf("Upgrade.Tooltip = %q, want full promise", fb.Upgrade.Tooltip)
			}
		}
	}
}

// TestSessionNullFreebucks pins the vendor af898dc null-wallet shape: a
// consent_required refusal (and pending-settlement polls) carry
// freebucks:null, which parses to a nil block — never a zero meter and
// never a revived legacy quota.
func TestSessionNullFreebucks(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"status":"consent_required","accessTier":"full",`+
			`"walletConsent":{"price":5.0,"walletSpend":2.0},"freebucks":null}`)
	}
	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatalf("consent_required must parse (taxonomy decides), got %v", err)
	}
	if st.Status != "consent_required" {
		t.Errorf("status = %q, want consent_required", st.Status)
	}
	if st.Freebucks != nil {
		t.Errorf("Freebucks = %+v, want nil for freebucks:null", st.Freebucks)
	}
	if st.WalletConsent == nil {
		t.Fatal("WalletConsent is nil, want parsed demand")
	}
	if st.WalletConsent.Price != 5.0 || st.WalletConsent.WalletSpend != 2.0 {
		t.Errorf("WalletConsent = %+v, want price=5 walletSpend=2", st.WalletConsent)
	}
	if st.HTTPStatus != http.StatusConflict {
		t.Errorf("HTTPStatus = %d, want 409", st.HTTPStatus)
	}
}

// TestEndSessionRefundReceipt pins the vendor af898dc DELETE receipt: the
// ended body carries the settled refund (including zero) and the pending
// flag that demands a same-instance replay.
func TestEndSessionRefundReceipt(t *testing.T) {
	t.Run("settled refund parsed", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-1","freebucksRefund":2.5}`)
		}
		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		rcpt, err := client.EndSession(context.Background(), "inst-1")
		if err != nil {
			t.Fatal(err)
		}
		if rcpt == nil {
			t.Fatal("receipt is nil, want parsed ended receipt")
		} else {
			if rcpt.Status != "ended" {
				t.Errorf("receipt status = %q, want ended", rcpt.Status)
			}
			if rcpt.Refund == nil || *rcpt.Refund != 2.5 {
				t.Errorf("receipt refund = %+v, want 2.5", rcpt.Refund)
			}
			if rcpt.Pending {
				t.Error("receipt pending = true, want false")
			}
		}
	})

	t.Run("pending demands replay", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-1","freebucksRefundPending":true}`)
		}
		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		rcpt, err := client.EndSession(context.Background(), "inst-1")
		if err != nil {
			t.Fatal(err)
		}
		if rcpt == nil || !rcpt.Pending {
			t.Errorf("receipt = %+v, want pending replay demand", rcpt)
		}
		if rcpt != nil && rcpt.Refund != nil {
			t.Errorf("receipt refund = %v, want nil (unsettled)", *rcpt.Refund)
		}
	})

	t.Run("empty body succeeds without receipt fields", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}
		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		rcpt, err := client.EndSession(context.Background(), "inst-1")
		if err != nil {
			t.Fatalf("DELETE 200 empty body = %v, want success (pre-receipt server)", err)
		}
		if rcpt == nil || rcpt.Status != "ended" {
			t.Errorf("receipt = %+v, want ended success", rcpt)
		}
	})

	t.Run("404 yields nil receipt", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"session not found"}`)
		}
		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		rcpt, err := client.EndSession(context.Background(), "inst-gone")
		if err != nil {
			t.Fatal(err)
		}
		if rcpt != nil {
			t.Errorf("receipt = %+v, want nil (nothing to end)", rcpt)
		}
	})
}

// TestSessionParsePurchaseCapacityHolder pins the purchase_capacity opaque
// decode: the holder instance, capacity bucket, and desktop metadata ride
// the state verbatim, and the holder id is surfaced in the message text
// (enrichment only — no admission branching).
func TestSessionParsePurchaseCapacityHolder(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"purchase_capacity","requestedModel":"openai/gpt-5.6-luna",`+
			`"currentInstanceId":"inst-holder-1","concurrency":"multi-tab","slotLimit":8,`+
			`"desktopPurchases":[{"model":"openai/gpt-5.6-luna","expiresAt":"2026-09-29T00:00:00Z","holderInstanceId":"inst-holder-1"}],`+
			`"desktopSessionCounts":{"premium":1,"unlimited":2,"nextExpiryAt":"2026-09-29T00:00:00Z"}}`)
	}
	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st.Status != "purchase_capacity" {
		t.Fatalf("Status = %q, want purchase_capacity", st.Status)
	}
	if st.CurrentInstanceID != "inst-holder-1" {
		t.Errorf("CurrentInstanceID = %q, want inst-holder-1", st.CurrentInstanceID)
	}
	if st.Concurrency != "multi-tab" {
		t.Errorf("Concurrency = %q, want multi-tab", st.Concurrency)
	}
	if st.SlotLimit == nil || *st.SlotLimit != 8 {
		t.Errorf("SlotLimit = %+v, want 8", st.SlotLimit)
	}
	if len(st.DesktopPurchases) != 1 || st.DesktopPurchases[0].HolderInstanceID != "inst-holder-1" {
		t.Errorf("DesktopPurchases = %+v, want the holder row", st.DesktopPurchases)
	}
	if st.DesktopSessionCounts == nil || st.DesktopSessionCounts.Premium != 1 || st.DesktopSessionCounts.Unlimited != 2 {
		t.Errorf("DesktopSessionCounts = %+v, want premium=1 unlimited=2", st.DesktopSessionCounts)
	}
	if !strings.Contains(st.Message, "inst-holder-1") {
		t.Errorf("Message = %q, want the holder id surfaced", st.Message)
	}
}

// TestSessionParseSupersededDesktopBlocks pins the superseded poll decode:
// desktop counts, purchases, and refunds ride the state verbatim.
func TestSessionParseSupersededDesktopBlocks(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"superseded",`+
			`"desktopSessionCounts":{"premium":0,"unlimited":1},`+
			`"desktopPurchases":[{"model":"openai/gpt-5.6-luna","expiresAt":"2026-09-29T00:00:00Z"}],`+
			`"desktopRefunds":[{"purchaseId":"p-1","model":"openai/gpt-5.6-luna","amount":2,"walletAmount":2,`+
			`"refundedAt":"2026-09-28T00:00:00Z","poolDate":"2026-09-28"}]}`)
	}
	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st.Status != "superseded" {
		t.Fatalf("Status = %q, want superseded", st.Status)
	}
	if st.DesktopSessionCounts == nil || st.DesktopSessionCounts.Unlimited != 1 {
		t.Errorf("DesktopSessionCounts = %+v, want unlimited=1", st.DesktopSessionCounts)
	}
	if len(st.DesktopPurchases) != 1 || st.DesktopPurchases[0].Model != "openai/gpt-5.6-luna" {
		t.Errorf("DesktopPurchases = %+v, want one row", st.DesktopPurchases)
	}
	if len(st.DesktopRefunds) != 1 || st.DesktopRefunds[0].PurchaseID != "p-1" {
		t.Errorf("DesktopRefunds = %+v, want the p-1 receipt", st.DesktopRefunds)
	}
}

// TestSessionParseActiveFreeWindows pins the freeWindows opaque decode on
// an active session, and that older servers omitting every new block leave
// the state zero (never fabricated).
func TestSessionParseActiveFreeWindows(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"i-1","model":"m",`+
				`"freeWindows":{"dayUsed":1,"dayLimit":5,"weekUsed":2,"weekLimit":20,`+
				`"monthUsed":3,"monthLimit":80,"dayResetAt":"2026-09-29T00:00:00Z",`+
				`"monthResetAt":"2026-10-01T00:00:00Z"}}`)
		}
		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		st, err := client.CreateSession(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(st.FreeWindows, `"dayLimit":5`) {
			t.Errorf("FreeWindows = %q, want the raw block", st.FreeWindows)
		}
	})
	t.Run("absent stays zero", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"i-1","model":"m"}`)
		}
		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		st, err := client.CreateSession(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if st.FreeWindows != "" || st.CurrentInstanceID != "" || st.Concurrency != "" {
			t.Errorf("new string fields = %q/%q/%q, want all empty", st.FreeWindows, st.CurrentInstanceID, st.Concurrency)
		}
		if st.SlotLimit != nil || st.DesktopSessionCounts != nil {
			t.Errorf("new pointer fields non-nil: slot=%+v counts=%+v", st.SlotLimit, st.DesktopSessionCounts)
		}
		if len(st.DesktopPurchases) != 0 || len(st.DesktopRefunds) != 0 {
			t.Errorf("new slice fields non-empty: %+v %+v", st.DesktopPurchases, st.DesktopRefunds)
		}
		if st.Message != "" {
			t.Errorf("Message = %q, want empty (no enrichment without a holder)", st.Message)
		}
	})
}
