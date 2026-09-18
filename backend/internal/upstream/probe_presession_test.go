package upstream

import (
	"context"
	"errors"
	"freebuff-proxy/backend/internal/testutil"
	"io"
	"net/http"
	"testing"
	"time"
)

// idleMeterBody is a pre-join "none" response carrying the full live meter:
// Freebucks, referral, per-model quota, and standing — the idle-with-balance
// shape the CLI renders its picker from without holding a slot.
const idleMeterBody = `{"status":"none","accessTier":"free",` +
	`"rateLimitsByModel":{"deepseek/deepseek-v4-flash":{"model":"deepseek/deepseek-v4-flash","limit":6,"recentCount":2,"period":"pacific_day","resetAt":"2026-09-01T07:00:00Z"}},` +
	`"referral":{"code":"ABC123","qualifiedCount":1},` +
	`"standing":{"level":"trusted","label":"Trusted","score":80},` +
	`"freebucks":{"balance":17.5,"daily":{"limit":20,"spent":5,"remaining":15,"resetAt":"2026-09-01T07:00:00Z"},"prices":{"deepseek/deepseek-v4-flash":2}}}`

// probeWire records what the probe actually sent on the wire.
type probeWire struct {
	method        string
	path          string
	instance      string
	timezone      string
	firstTab      string
	posts         int
	gets          int
	sawSessionGET bool
}

func captureProbeWire(mock *testutil.MockUpstream, w *probeWire, body string, status int) {
	mock.SessionHandler = func(rw http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.posts++
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/freebuff/session" {
			w.gets++
			w.sawSessionGET = true
			w.method = r.Method
			w.path = r.URL.Path
			w.instance = r.Header.Get("x-freebuff-instance-id")
			w.timezone = r.Header.Get(FreebucksTimezoneHeader)
			w.firstTab = r.Header.Get(FirstTabDiscountHeader)
		}
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(status)
		_, _ = io.WriteString(rw, body)
	}
}

// TestProbeAccountIdleMeterPopulatesState pins the pre-session populate
// contract: a 200 none-with-balance returns the decoded meter ALONGSIDE
// ErrNoActiveSession (old callers still branch on the sentinel) without
// claiming a slot.
func TestProbeAccountIdleMeterPopulatesState(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var wire probeWire
	captureProbeWire(mock, &wire, idleMeterBody, http.StatusOK)

	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.ProbeAccount(context.Background())
	if !errors.Is(err, ErrNoActiveSession) {
		t.Fatalf("err = %v, want ErrNoActiveSession (state rides alongside)", err)
	}
	if st == nil {
		t.Fatal("state = nil, want decoded idle meter alongside the sentinel")
	}
	if st.Status != "none" {
		t.Errorf("Status = %q, want none", st.Status)
	}
	if st.Freebucks == nil || st.Freebucks.Balance != 17.5 {
		t.Errorf("Freebucks = %+v, want balance 17.5", st.Freebucks)
	}
	if st.Referral == nil || st.Referral.Code != "ABC123" {
		t.Errorf("Referral = %+v, want code ABC123", st.Referral)
	}
	if q, ok := st.RateLimitsByModel["deepseek/deepseek-v4-flash"]; !ok || q.Limit != 6 {
		t.Errorf("RateLimitsByModel = %+v, want flash quota limit 6", st.RateLimitsByModel)
	}
	if st.Standing == nil || st.Standing.Level != "trusted" {
		t.Errorf("Standing = %+v, want level trusted", st.Standing)
	}
	// Zero-cost: exactly one session GET, no admission POST, no instance
	// header (no slot claimed).
	if !wire.sawSessionGET || wire.gets != 1 {
		t.Errorf("session GETs = %d, want exactly 1", wire.gets)
	}
	if wire.posts != 0 {
		t.Errorf("POSTs = %d, want 0 (probe never admits)", wire.posts)
	}
	if wire.instance != "" {
		t.Errorf("x-freebuff-instance-id = %q, want absent", wire.instance)
	}
	if got := mock.StreakHitsSnapshot(); got != 0 {
		t.Errorf("streak hits = %d, want 0 (streak stays on GetStreak)", got)
	}
}

// TestProbeAccount404IsBareNone pins the CLI bare-none mapping: a probe 404
// (no body) returns a "none" state with a nil meter, still carrying the
// ErrNoActiveSession sentinel.
func TestProbeAccount404IsBareNone(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var wire probeWire
	captureProbeWire(mock, &wire, `{"error":"session not found"}`, http.StatusNotFound)

	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.ProbeAccount(context.Background())
	if !errors.Is(err, ErrNoActiveSession) {
		t.Fatalf("err = %v, want ErrNoActiveSession", err)
	}
	if st == nil {
		t.Fatal("state = nil, want bare-none state alongside the sentinel")
	}
	if st.Status != "none" {
		t.Errorf("Status = %q, want none (404 normalizes to bare-none)", st.Status)
	}
	if st.Freebucks != nil {
		t.Errorf("Freebucks = %+v, want nil meter on bare-none", st.Freebucks)
	}
	if st.RateLimitsByModel != nil {
		t.Errorf("RateLimitsByModel = %+v, want nil on bare-none", st.RateLimitsByModel)
	}
	if st.Referral != nil {
		t.Errorf("Referral = %+v, want nil on bare-none", st.Referral)
	}
	if wire.instance != "" {
		t.Errorf("x-freebuff-instance-id = %q, want absent", wire.instance)
	}
}

// TestProbeAccountEndedKeepsMeter pins that a 200 "ended" carrying a meter
// keeps it alongside the sentinel (same contract as "none").
func TestProbeAccountEndedKeepsMeter(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var wire probeWire
	captureProbeWire(mock, &wire, `{"status":"ended","freebucks":{"balance":3,"daily":{"limit":20,"spent":17,"remaining":3,"resetAt":"2026-09-01T07:00:00Z"},"prices":{"deepseek/deepseek-v4-flash":2}}}`, http.StatusOK)

	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.ProbeAccount(context.Background())
	if !errors.Is(err, ErrNoActiveSession) {
		t.Fatalf("err = %v, want ErrNoActiveSession", err)
	}
	if st == nil {
		t.Fatal("state = nil, want ended meter alongside the sentinel")
	}
	if st.Status != "ended" {
		t.Errorf("Status = %q, want ended (200 ended keeps its status)", st.Status)
	}
	if st.Freebucks == nil || st.Freebucks.Balance != 3 {
		t.Errorf("Freebucks = %+v, want balance 3", st.Freebucks)
	}
}

// TestProbeAccountSendsCLIParityHeaders pins the read headers on the probe
// GET: host IANA timezone + boring first-tab "0", and never an instance id.
func TestProbeAccountSendsCLIParityHeaders(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var wire probeWire
	captureProbeWire(mock, &wire, `{"status":"active","instanceId":"inst-abc-123","expiresAt":"2030-01-01T00:00:00Z"}`, http.StatusOK)

	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ProbeAccount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !wire.sawSessionGET {
		t.Fatal("no session GET recorded")
	}
	if wire.method != http.MethodGet || wire.path != "/api/v1/freebuff/session" {
		t.Errorf("probe = %s %s, want GET /api/v1/freebuff/session", wire.method, wire.path)
	}
	if want := localIANATimezone(); wire.timezone != want {
		t.Errorf("x-fb-timezone = %q, want %q (host zone)", wire.timezone, want)
	}
	if _, err := time.LoadLocation(wire.timezone); err != nil {
		t.Errorf("x-fb-timezone = %q, not a valid IANA zone: %v", wire.timezone, err)
	}
	if wire.firstTab != "0" {
		t.Errorf("x-freebuff-first-tab-discount = %q, want boring \"0\"", wire.firstTab)
	}
	if wire.instance != "" {
		t.Errorf("x-freebuff-instance-id = %q, want absent (zero-cost)", wire.instance)
	}
}

// TestLocalIANATimezoneBoring pins the fallback: always a loadable zone,
// never empty or bare "Local".
func TestLocalIANATimezoneBoring(t *testing.T) {
	got := localIANATimezone()
	if got == "" || got == "Local" {
		t.Errorf("localIANATimezone() = %q, want real zone or UTC", got)
	}
	if _, err := time.LoadLocation(got); err != nil {
		t.Errorf("localIANATimezone() = %q, not loadable: %v", got, err)
	}
}
