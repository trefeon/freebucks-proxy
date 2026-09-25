package session

// store_unified_test.go — Lane B proofs of the four unified-store behavioral
// invariants for the session plane (Store spill: mem-swap first, WAL behind
// via StartSpill batches or explicit Flush/Close):
//
//   - I1 sync-visible: Save/SaveRun/Remove/RemoveRun are visible to the next
//     Load synchronously, before any backend write.
//   - I2 no disk on request path: mutations issue zero backend writes
//     (write-call counting, not timing) until the background spill or an
//     explicit Flush/Close.
//   - I3 restart recovers: Close (the shutdown path) on one store plus a
//     fresh store over the same backend recovers sessions and runs (existing
//     carry/restore tests stay green alongside).
//   - I4 .env untouched/unread post-boot: mutations + spill + restore with a
//     garbage .env in the cwd behave identically and leave every file byte.
//     The legacy JSON path stays import-only (never written).
//
// Revalidation is unchanged: Load drops grace-expired rows, fetchLocked
// reconciles a memory miss against the backend exactly once, and the legacy
// import keeps its store-first collision rule. Slot counters, cooldowns,
// single-flight and ip_capped live outside this store and never cross the
// persistence boundary.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func unifiedSlot(instanceID string) *cachedState {
	expiry := time.Now().Add(time.Hour)
	return &cachedState{
		status:            "active",
		instanceID:        instanceID,
		model:             "m",
		expiresAt:         expiry,
		gracePeriodEndsAt: expiry.Add(graceWindow),
	}
}

// TestSessionUnifiedSyncVisible (I1): mutations are visible to the next read
// synchronously, before any backend write lands.
func TestSessionUnifiedSyncVisible(t *testing.T) {
	fb := newFakeSessionBackend()
	s := NewStoreWithBackend(filepath.Join(t.TempDir(), "state.json"), fb)

	s.Save("k", unifiedSlot("inst-1"))
	if got := s.Load("k"); got == nil || got.instanceID != "inst-1" {
		t.Fatalf("Load right after Save = %+v, want inst-1 (I1)", got)
	}
	s.SaveRun("k", "agent-x", PersistedRun{RunID: "run-1", AgentID: "agent-x"})
	if got := s.LoadRun("k", "agent-x"); got == nil || got.RunID != "run-1" {
		t.Fatalf("LoadRun right after SaveRun = %+v, want run-1 (I1)", got)
	}
	s.RemoveRun("k", "agent-x")
	if got := s.LoadRun("k", "agent-x"); got != nil {
		t.Fatalf("LoadRun right after RemoveRun = %+v, want nil (I1)", got)
	}
	s.Remove("k", "")
	if got := s.Load("k"); got != nil {
		t.Fatalf("Load right after Remove = %+v, want nil (I1)", got)
	}

	fb.mu.Lock()
	n := fb.saves
	fb.mu.Unlock()
	if n != 0 {
		t.Fatalf("backend writes before any flush = %d, want 0 (I1 precedes durability)", n)
	}
}

// TestSessionUnifiedNoDiskOnRequestPath (I2): without StartSpill, the full
// mutation set plus reads issues zero backend writes — the request path
// never touches disk; durability rides the spill loop or explicit Flush.
func TestSessionUnifiedNoDiskOnRequestPath(t *testing.T) {
	fb := newFakeSessionBackend()
	s := NewStoreWithBackend(filepath.Join(t.TempDir(), "state.json"), fb)
	if s.SpillRunning() {
		t.Fatal("spill loop running without StartSpill")
	}

	s.Save("k", unifiedSlot("inst-1"))
	s.SaveRun("k", "agent-x", PersistedRun{RunID: "run-1", AgentID: "agent-x"})
	_ = s.Load("k")
	_ = s.LoadRun("k", "agent-x")
	_ = s.Load("missing")
	s.RemoveRun("k", "agent-x")
	s.Remove("k", "")

	fb.mu.Lock()
	n := fb.saves
	fb.mu.Unlock()
	if n != 0 {
		t.Fatalf("backend writes on the mutation/read path = %d, want 0 (I2)", n)
	}
	if got := s.SpillDropped(); got != 0 {
		t.Fatalf("spill drops without a running loop = %d, want 0 (enqueue is a no-op)", got)
	}
}

// TestSessionUnifiedRestartRecovers (I3): Close (the shutdown path: stops
// the spill consumer with a final flush) on one store plus a fresh store
// over the same backend recovers the session and the run.
func TestSessionUnifiedRestartRecovers(t *testing.T) {
	fb := newFakeSessionBackend()
	dir := t.TempDir()
	s1 := NewStoreWithBackend(filepath.Join(dir, "state.json"), fb)
	s1.StartSpill()

	s1.Save("k", unifiedSlot("inst-restart"))
	s1.SaveRun("k", "agent-x", PersistedRun{RunID: "run-r", AgentID: "agent-x", TraceSessionID: "t"})
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2 := NewStoreWithBackend(filepath.Join(dir, "state.json"), fb)
	if got := s2.Load("k"); got == nil || got.instanceID != "inst-restart" {
		t.Fatalf("reopened Load = %+v, want inst-restart (I3)", got)
	}
	if pr := s2.LoadRun("k", "agent-x"); pr == nil || pr.RunID != "run-r" {
		t.Fatalf("reopened LoadRun = %+v, want run-r (I3)", pr)
	}
}

// TestSessionUnifiedEnvUntouched (I4): with a garbage .env in the cwd, the
// full mutate + spill + restore cycle behaves identically and leaves every
// file byte-identical — .env is never written and never re-read after boot,
// and the legacy JSON path is never written (import-only).
func TestSessionUnifiedEnvUntouched(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	poison := []byte("SESSION_PERSIST=false\nAPI_KEYS=poison\n")
	if err := os.WriteFile(envPath, poison, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	fb := newFakeSessionBackend()
	statePath := filepath.Join(dir, "state.json")
	s1 := NewStoreWithBackend(statePath, fb)
	s1.StartSpill()
	s1.Save("k", unifiedSlot("inst-env"))
	s1.SaveRun("k", "agent-x", PersistedRun{RunID: "run-e", AgentID: "agent-x"})
	if err := s1.Close(); err != nil {
		t.Fatalf("Close with poison .env: %v", err)
	}

	s2 := NewStoreWithBackend(statePath, fb)
	if got := s2.Load("k"); got == nil || got.instanceID != "inst-env" {
		t.Fatalf("Load with poison .env = %+v, want inst-env", got)
	}
	if pr := s2.LoadRun("k", "agent-x"); pr == nil || pr.RunID != "run-e" {
		t.Fatalf("LoadRun with poison .env = %+v, want run-e", pr)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != ".env" {
		t.Fatalf("cwd entries = %v, want exactly [.env] (legacy state file never written)", names)
	}
	after, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(poison) {
		t.Fatalf(".env after mutations = %q, want byte-identical %q", after, poison)
	}
}
