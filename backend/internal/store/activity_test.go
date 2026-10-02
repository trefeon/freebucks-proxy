package store

import (
	"testing"
)

const (
	dayMs  = int64(86400000)
	hourMs = int64(3600000)
)

func seedActivity(t *testing.T) *Store {
	t.Helper()
	s := openTest(t)
	t.Cleanup(func() { _ = s.Close() })
	rows := []RequestRecord{
		{ReqID: "d1a", TS: 1_000, Endpoint: "/v1/chat/completions", Model: "m/a", TokenIdx: 0, Status: "ok"},
		{ReqID: "d1b", TS: 2_000, Endpoint: "/v1/chat/completions", Model: "m/a", TokenIdx: 1, Status: "error", Err: "x"},
		{ReqID: "d1c", TS: hourMs + 1_000, Endpoint: "/v1/messages", Model: "m/b", TokenIdx: -1, Status: "ok"},
		{ReqID: "d2a", TS: dayMs + 1_000, Endpoint: "/v1/chat/completions", Model: "m/a", TokenIdx: 0, Status: "ok"},
		{ReqID: "d2b", TS: dayMs + 2_000, Endpoint: "/v1/responses", Model: "m/b", TokenIdx: 0, Status: "error", Err: "y"},
	}
	for _, rec := range rows {
		if err := s.RecordRequest(rec); err != nil {
			t.Fatalf("RecordRequest %s: %v", rec.ReqID, err)
		}
	}
	return s
}

func TestRequestsByDay(t *testing.T) {
	s := seedActivity(t)
	got, err := s.RequestsByDay(0, 3*dayMs)
	if err != nil {
		t.Fatalf("RequestsByDay: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("days = %v, want 2 buckets", got)
	}
	if got[0].Day != 0 || got[0].Total != 3 || got[0].Errors != 1 {
		t.Errorf("day0 = %+v, want {0 3 1}", got[0])
	}
	if got[1].Day != dayMs || got[1].Total != 2 || got[1].Errors != 1 {
		t.Errorf("day1 = %+v, want {86400000 2 1}", got[1])
	}
}

func TestRequestsByHour(t *testing.T) {
	s := seedActivity(t)
	got, err := s.RequestsByHour(0, 3*dayMs)
	if err != nil {
		t.Fatalf("RequestsByHour: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("hours = %v, want 3 buckets", got)
	}
	if got[0].Hour != 0 || got[0].Total != 2 || got[0].Errors != 1 {
		t.Errorf("hour0 = %+v, want {0 2 1}", got[0])
	}
	if got[1].Hour != hourMs || got[1].Total != 1 || got[1].Errors != 0 {
		t.Errorf("hour1 = %+v, want {3600000 1 0}", got[1])
	}
	if got[2].Hour != 24*hourMs || got[2].Total != 2 || got[2].Errors != 1 {
		t.Errorf("hour24 = %+v, want {86400000 2 1}", got[2])
	}
}

func TestTopModels(t *testing.T) {
	s := seedActivity(t)
	got, err := s.TopModels(0, 3*dayMs, 10)
	if err != nil {
		t.Fatalf("TopModels: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("models = %v, want 2", got)
	}
	if got[0].Name != "m/a" || got[0].Total != 3 || got[0].Errors != 1 {
		t.Errorf("top[0] = %+v, want m/a 3/1", got[0])
	}
	if got[1].Name != "m/b" || got[1].Total != 2 {
		t.Errorf("top[1] = %+v, want m/b 2", got[1])
	}
	one, err := s.TopModels(0, 3*dayMs, 1)
	if err != nil {
		t.Fatalf("TopModels limit 1: %v", err)
	}
	if len(one) != 1 || one[0].Name != "m/a" {
		t.Errorf("limit-1 = %v, want [m/a]", one)
	}
	// Over-cap limits clamp to ActivityTopN instead of scanning unbounded.
	many, err := s.TopModels(0, 3*dayMs, 1<<20)
	if err != nil {
		t.Fatalf("TopModels huge limit: %v", err)
	}
	if len(many) != 2 {
		t.Errorf("huge-limit = %v, want both models (clamped, not error)", many)
	}
}

func TestTopTokens(t *testing.T) {
	s := seedActivity(t)
	got, err := s.TopTokens(0, 3*dayMs, 10)
	if err != nil {
		t.Fatalf("TopTokens: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("tokens = %v, want 3 lanes", got)
	}
	if got[0].Name != "token-0" || got[0].Total != 3 {
		t.Errorf("top[0] = %+v, want token-0 x3", got[0])
	}
	foundBridge := false
	for _, g := range got {
		if g.Name == "token-bridge" && g.Total == 1 {
			foundBridge = true
		}
	}
	if !foundBridge {
		t.Errorf("no token-bridge row in %v (token_idx -1 must render, not vanish)", got)
	}
}

func TestActivityEmptyWindow(t *testing.T) {
	s := openTest(t)
	t.Cleanup(func() { _ = s.Close() })
	if got, err := s.RequestsByDay(0, 1000); err != nil || len(got) != 0 {
		t.Errorf("empty by-day = %v, %v; want empty non-nil, nil error", got, err)
	}
	if got, err := s.TopModels(0, 1000, 5); err != nil || len(got) != 0 {
		t.Errorf("empty top-models = %v, %v; want empty non-nil, nil error", got, err)
	}
	if _, err := s.RequestsByDay(5000, 1000); err == nil {
		t.Error("inverted window succeeded, want the clamp error")
	}
}
