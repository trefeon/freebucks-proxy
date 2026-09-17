package session

// store_adopt_test.go — pins run-id adopt across restarts: the session row
// (sessions_persist, the quota single-writer owner) and the runs blob ride
// the same backend row, and a fresh store over that backend adopts both
// intact (adopt-or-re-START reads the run id verbatim).

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRunAdoptIntactAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	fb := newFakeSessionBackend()
	s := NewStoreWithBackend(path, fb)
	s.Save("tokhash", &cachedState{
		status: "active", instanceID: "inst-1", model: "m",
		expiresAt: time.Now().Add(time.Hour), gracePeriodEndsAt: time.Now().Add(2 * time.Hour),
	})
	s.SaveRun("tokhash", "agent-x", PersistedRun{
		RunID: "run-1", AgentID: "agent-x", TraceSessionID: "trace-1",
		ClientID: "client-1", StartedAt: time.Now().Add(-time.Minute), Requests: 3,
	})

	// Restart: a fresh store over the same backend adopts both blobs intact.
	s2 := NewStoreWithBackend(path, fb)
	cs := s2.Load("tokhash")
	if cs == nil || cs.instanceID != "inst-1" {
		t.Fatalf("session adopt = %+v, want inst-1", cs)
	}
	pr := s2.LoadRun("tokhash", "agent-x")
	if pr == nil {
		t.Fatal("run adopt = nil, want run-1")
	}
	if pr.RunID != "run-1" || pr.TraceSessionID != "trace-1" || pr.ClientID != "client-1" || pr.Requests != 3 {
		t.Fatalf("run adopt = %+v, want intact run-1", pr)
	}
}
