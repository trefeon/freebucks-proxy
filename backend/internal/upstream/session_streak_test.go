package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"freebucks-proxy/backend/internal/config"
)

// TestGetStreakDecodesFreebucksDailyBonus pins the wire field the
// dashboard's streak perk note needs: freebucksDailyBonus decodes into a
// present value, an explicit 0 stays a present 0 (not "no bonus"), and an
// older server that omits the field leaves it nil so the SPA draws no note.
func TestGetStreakDecodesFreebucksDailyBonus(t *testing.T) {
	ptr := func(v float64) *float64 { return &v }
	cases := []struct {
		name string
		body string
		want *float64
	}{
		{
			name: "present",
			body: `{"streak":7,"todayUsed":true,"lastUsageDate":"2026-09-23","timeZone":"Asia/Jakarta","freebucksDailyBonus":15}`,
			want: ptr(15),
		},
		{
			name: "explicit zero",
			body: `{"streak":7,"todayUsed":true,"lastUsageDate":"2026-09-23","timeZone":"Asia/Jakarta","freebucksDailyBonus":0}`,
			want: ptr(0),
		},
		{
			name: "absent",
			body: `{"streak":7,"todayUsed":true,"lastUsageDate":"2026-09-23","timeZone":"Asia/Jakarta"}`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/freebuff/streak" {
					t.Errorf("path = %q, want /api/v1/freebuff/streak", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			client, err := New("tok-0", &config.Config{UpstreamBaseURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			info, err := client.GetStreak(context.Background())
			if err != nil {
				t.Fatalf("GetStreak: %v", err)
			}
			if info.Streak != 7 || !info.TodayUsed || info.TimeZone != "Asia/Jakarta" {
				t.Fatalf("decoded streak info = %+v, want streak 7/todayUsed/Jakarta", info)
			}
			if tc.want == nil {
				if info.FreebucksDailyBonus != nil {
					t.Errorf("FreebucksDailyBonus = %v, want nil (absent)", *info.FreebucksDailyBonus)
				}
				return
			}
			if info.FreebucksDailyBonus == nil {
				t.Fatalf("FreebucksDailyBonus = nil, want %v", *tc.want)
			}
			if *info.FreebucksDailyBonus != *tc.want {
				t.Errorf("FreebucksDailyBonus = %v, want %v", *info.FreebucksDailyBonus, *tc.want)
			}
		})
	}
}
