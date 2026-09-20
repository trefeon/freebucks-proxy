package upstream

import (
	"context"
	"freebucks-proxy/backend/internal/testutil"
	"io"
	"net/http"
	"testing"
)

// TestParseSessionModelUnavailableLimitedOfferReason pins the vendor e2b911eca
// limitedOfferReason passthrough on model_unavailable: the three known
// members survive verbatim, absent stays ”, and an unknown member (a newer
// server value this build does not know) normalizes to ”.
func TestParseSessionModelUnavailableLimitedOfferReason(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantReason string
	}{
		{
			name:       "used",
			body:       `{"status":"model_unavailable","requestedModel":"m","availableHours":"x","limitedOfferReason":"used"}`,
			wantReason: "used",
		},
		{
			name:       "closed",
			body:       `{"status":"model_unavailable","requestedModel":"m","availableHours":"x","limitedOfferReason":"closed"}`,
			wantReason: "closed",
		},
		{
			name:       "exhausted",
			body:       `{"status":"model_unavailable","requestedModel":"m","availableHours":"x","limitedOfferReason":"exhausted"}`,
			wantReason: "exhausted",
		},
		{
			name:       "absent",
			body:       `{"status":"model_unavailable","requestedModel":"m","availableHours":"x"}`,
			wantReason: "",
		},
		{
			name:       "unknown normalizes to empty",
			body:       `{"status":"model_unavailable","requestedModel":"m","availableHours":"x","limitedOfferReason":"paused"}`,
			wantReason: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
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
			if st.Status != "model_unavailable" {
				t.Fatalf("status = %q, want model_unavailable", st.Status)
			}
			if st.LimitedOfferReason != tc.wantReason {
				t.Errorf("LimitedOfferReason = %q, want %q", st.LimitedOfferReason, tc.wantReason)
			}
		})
	}
}

// TestParseSessionModelUnavailableUsedSkipsWindow pins the terminal 'used'
// distinction: a consumed personal trial cannot be replenished by waiting,
// so it carries no UnavailableWindow countdown even when availableHours is
// present, while closed/exhausted still parse it.
func TestParseSessionModelUnavailableUsedSkipsWindow(t *testing.T) {
	serve := func(t *testing.T, body string) *SessionState {
		t.Helper()
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, body)
		}
		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		st, err := client.CreateSession(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return st
	}
	used := serve(t, `{"status":"model_unavailable","requestedModel":"m","availableHours":"9am ET-5pm PT every day","limitedOfferReason":"used"}`)
	if used.UnavailableWindow != nil {
		t.Errorf("used UnavailableWindow = %+v, want nil (terminal, no countdown)", used.UnavailableWindow)
	}
	closed := serve(t, `{"status":"model_unavailable","requestedModel":"m","availableHours":"9am ET-5pm PT every day","limitedOfferReason":"closed"}`)
	if closed.UnavailableWindow == nil {
		t.Error("closed UnavailableWindow = nil, want parsed window")
	}
}

// TestParseLimitedOfferNullReset pins the pre-join (none) limitedModelOffers
// parse: a JSON null userResetAt stays nil (never a zero time), a set
// timestamp survives verbatim, and offers land on state in order.
func TestParseLimitedOfferNullReset(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"none","limitedModelOffers":[{"model":"trial/a","remaining":7,"total":10,"userRemaining":1,"userResetAt":null},{"model":"trial/b","remaining":3,"total":10,"userRemaining":0,"userResetAt":"2030-01-02T00:00:00Z"}]}`)
	}
	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(st.LimitedModelOffers) != 2 {
		t.Fatalf("LimitedModelOffers = %+v, want 2 offers", st.LimitedModelOffers)
	}
	if st.LimitedModelOffers[0].UserResetAt != nil {
		t.Errorf("offers[0].UserResetAt = %q, want nil (null stays null)", *st.LimitedModelOffers[0].UserResetAt)
	}
	got := st.LimitedModelOffers[1].UserResetAt
	if got == nil || *got != "2030-01-02T00:00:00Z" {
		t.Errorf("offers[1].UserResetAt = %v, want 2030-01-02T00:00:00Z verbatim", got)
	}
	if st.LimitedModelOffers[0].Model != "trial/a" || st.LimitedModelOffers[0].Remaining != 7 || st.LimitedModelOffers[0].Total != 10 || st.LimitedModelOffers[0].UserRemaining != 1 {
		t.Errorf("offers[0] = %+v, want trial/a 7/10 user 1", st.LimitedModelOffers[0])
	}
}

// TestOfferJoinableUserRemainingZero pins joinability: userRemaining == 0 is
// "not now" (not joinable) rather than hiding the row; any positive count
// is joinable.
func TestOfferJoinableUserRemainingZero(t *testing.T) {
	if (LimitedModelOffer{Model: "trial/a", UserRemaining: 0}).Joinable() {
		t.Error("Joinable() = true for userRemaining 0, want false")
	}
	if !(LimitedModelOffer{Model: "trial/a", UserRemaining: 1}).Joinable() {
		t.Error("Joinable() = false for userRemaining 1, want true")
	}
}
