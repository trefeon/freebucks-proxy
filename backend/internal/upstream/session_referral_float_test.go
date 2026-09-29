package upstream

import (
	"net/http"
	"testing"
)

// TestSessionParseFractionalReferralSessions pins the live 2026-09-29 shape:
// referral.weeklySessionsRemaining arrives fractional (3.7) and must parse
// (truncated to whole sessions) instead of failing the entire session poll
// with "unparseable session response" — which blanked the pool's sessions,
// models and probe results after every restart.
func TestSessionParseFractionalReferralSessions(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/freebuff/session", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
	client, err := New("tok-a", testConfig("http://127.0.0.1:1", nil))
	if err != nil {
		t.Fatal(err)
	}
	body := `{"status":"active","accessTier":"full","instanceId":"cli:abc","model":"z-ai/glm-5.3-flash",` +
		`"referral":{"code":"ref-1","referrerName":"r","qualifiedCount":0,"weeklySessionsRemaining":3.7,"githubLinked":false},` +
		`"freebucks":{"balance":10,"daily":{"limit":100,"spent":90,"remaining":10}}}`
	st, err := client.parseSessionResponse(req, resp, body)
	if err != nil {
		t.Fatalf("parse fractional referral: %v", err)
	}
	if st.Status != "active" {
		t.Errorf("Status = %q, want active", st.Status)
	}
	if st.Referral == nil {
		t.Fatal("Referral nil, want parsed")
	}
	if st.Referral.WeeklySessionsRemaining != 3 {
		t.Errorf("WeeklySessionsRemaining = %d, want 3 (truncated)", st.Referral.WeeklySessionsRemaining)
	}
	if st.Freebucks == nil || st.Freebucks.Balance != 10 {
		t.Errorf("Freebucks not parsed alongside referral: %+v", st.Freebucks)
	}
}
