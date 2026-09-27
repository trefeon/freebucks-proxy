// Live-capture pins (docs/LIVE-CAPTURE.md, official CLI 0.1.0-era): the
// agent-runs START/FINISH wire shapes and the per-run step counter that
// threads llm_step_number 1..N across a run's chat attempts.
//
// START/FINISH marshaling lives in upstream (session.go) — DO NOT duplicate
// it here; these tests pin the shapes black-box through a raw-body capturing
// server, fed by the runs-side payload builders (finishPayload, RecordStep,
// NextStepNumber) exactly as the server wires them (engine_attempt.go stamps
// NextStepNumber once per chatAttempt; engine.go RecordStep after completion).
package runs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
)

// captureServer records raw agent-runs request bodies and answers like the
// mock upstream: {"runId":...} for START, {"success":true} for FINISH.
func captureServer(t *testing.T, bodies *[][]byte, runID string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, body)
		w.Header().Set("Content-Type", "application/json")
		var probe struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(body, &probe)
		if probe.Action == "START" {
			raw, _ := json.Marshal(map[string]string{"runId": runID})
			_, _ = w.Write(raw)
			return
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
}

func decodeBodyKeys(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("FINISH/START body is not JSON: %v\n%s", err, body)
	}
	return m
}

func requireExactKeys(t *testing.T, what string, m map[string]any, want ...string) {
	t.Helper()
	if len(m) != len(want) {
		t.Errorf("%s keys = %v, want exactly %v", what, keysOf(m), want)
		return
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("%s missing key %q (got %v)", what, k, keysOf(m))
		}
	}
}

func keysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// TestStartBodyMatchesLiveCapture pins the capture's START shape:
// {"action":"START","agentId":"base3-free-<model-slug>","ancestorRunIds":[]}
// — exact top-level keys, per-model slug agent id, present-but-empty
// ancestor list. Both capture turns (DeepSeek flash, GLM flash) are covered.
func TestStartBodyMatchesLiveCapture(t *testing.T) {
	for _, agentID := range []string{"base3-free-deepseek-flash", "base3-free-glm-5-3-flash"} {
		t.Run(agentID, func(t *testing.T) {
			var bodies [][]byte
			srv := captureServer(t, &bodies, "run-livecap-1")
			defer srv.Close()
			client, err := upstream.New("tok", &config.Config{
				UpstreamBaseURL:    srv.URL,
				RequestTimeout:     time.Minute,
				SessionCallTimeout: 5 * time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			runID, err := client.StartRun(context.Background(), agentID)
			if err != nil {
				t.Fatal(err)
			}
			if runID != "run-livecap-1" {
				t.Errorf("runId = %q, want run-livecap-1", runID)
			}
			if len(bodies) != 1 {
				t.Fatalf("START requests = %d, want 1", len(bodies))
			}
			m := decodeBodyKeys(t, bodies[0])
			requireExactKeys(t, "START", m, "action", "agentId", "ancestorRunIds")
			if m["action"] != "START" {
				t.Errorf("action = %v, want START", m["action"])
			}
			if m["agentId"] != agentID {
				t.Errorf("agentId = %v, want %q", m["agentId"], agentID)
			}
			anc, ok := m["ancestorRunIds"].([]any)
			if !ok {
				t.Fatalf("ancestorRunIds = %v (%T), want present empty list", m["ancestorRunIds"], m["ancestorRunIds"])
			}
			if len(anc) != 0 {
				t.Errorf("ancestorRunIds has %d entries, want empty", len(anc))
			}
		})
	}
}

// TestFinishBodyMatchesLiveCapture replays the capture's two turns (totalSteps
// 2 and 4, zero credits on the free tier) through the real runs machinery —
// Acquire, per-attempt NextStepNumber stamps, RecordStep after completion,
// finishPayload — and pins the FINISH wire body: exact top-level keys,
// honest status/counts/credits, and step entries whose numbers agree with the
// llm_step_number stamps already sent.
func TestFinishBodyMatchesLiveCapture(t *testing.T) {
	for _, n := range []int{2, 4} {
		t.Run(fmt.Sprintf("%d-steps", n), func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mgr, _ := newTestManager(t, mock, time.Hour)

			run, err := mgr.Acquire(context.Background(), "base3-free-glm-5-3-flash")
			if err != nil {
				t.Fatal(err)
			}
			// Server wiring order per chat attempt: stamp first (the
			// llm_step_number on the wire), record after completion.
			// messageID "" mirrors engine.go (stream carries no id; the
			// CLI step schema allows a null messageId).
			for i := 1; i <= n; i++ {
				if got := run.NextStepNumber(); got != int64(i) {
					t.Fatalf("stamp %d: NextStepNumber = %d", i, got)
				}
				mgr.RecordStep(run, "")
			}
			mgr.Release(run)
			status, steps, totalSteps := mgr.finishPayload(run)

			var bodies [][]byte
			srv := captureServer(t, &bodies, "unused")
			defer srv.Close()
			raw, err := upstream.New("tok", &config.Config{
				UpstreamBaseURL:    srv.URL,
				RequestTimeout:     time.Minute,
				SessionCallTimeout: 5 * time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := raw.FinishRun(context.Background(), run.RunID, status, totalSteps, steps, ""); err != nil {
				t.Fatal(err)
			}
			if len(bodies) != 1 {
				t.Fatalf("FINISH requests = %d, want 1", len(bodies))
			}
			m := decodeBodyKeys(t, bodies[0])
			requireExactKeys(t, "FINISH", m, "action", "runId", "status", "totalSteps", "directCredits", "totalCredits", "steps")
			if m["action"] != "FINISH" {
				t.Errorf("action = %v, want FINISH", m["action"])
			}
			if m["runId"] != run.RunID {
				t.Errorf("runId = %v, want %q", m["runId"], run.RunID)
			}
			if m["status"] != "completed" {
				t.Errorf("status = %v, want completed", m["status"])
			}
			if m["totalSteps"] != float64(n) {
				t.Errorf("totalSteps = %v, want %d", m["totalSteps"], n)
			}
			// Free-tier turns report zero credits (capture: directCredits
			// 0, totalCredits 0 on both the plain and the tool-use turn).
			if m["directCredits"] != float64(0) {
				t.Errorf("directCredits = %v, want 0", m["directCredits"])
			}
			if m["totalCredits"] != float64(0) {
				t.Errorf("totalCredits = %v, want 0", m["totalCredits"])
			}
			got, ok := m["steps"].([]any)
			if !ok || len(got) != n {
				t.Fatalf("steps has %d entries, want %d", len(got), n)
			}
			for i, e := range got {
				entry, ok := e.(map[string]any)
				if !ok {
					t.Fatalf("steps[%d] is not an object: %v", i, e)
				}
				if entry["stepNumber"] != float64(i+1) {
					t.Errorf("steps[%d].stepNumber = %v, want %d (must agree with the llm_step_number stamp)", i, entry["stepNumber"], i+1)
				}
				if id, _ := entry["id"].(string); id == "" {
					t.Errorf("steps[%d].id is empty", i)
				}
				// "" messageID wires as null (CLI schema allows it).
				if mid, present := entry["messageId"]; !present || mid != nil {
					t.Errorf("steps[%d].messageId = %v, want null", i, mid)
				}
				if entry["status"] != "completed" {
					t.Errorf("steps[%d].status = %v, want completed", i, entry["status"])
				}
				st, _ := entry["startTime"].(string)
				if _, err := time.Parse(time.RFC3339Nano, st); err != nil {
					t.Errorf("steps[%d].startTime = %q, want RFC3339Nano: %v", i, st, err)
				}
				// NOTE: credits/childRunIds elision (omitempty) is decided
				// by the upstream RunStep marshaler — sibling slice owns
				// backend/internal/upstream/; free-tier values are zero
				// either way, so nothing here can send a stub value.
			}
			mgr.Shutdown(context.Background())
		})
	}
}

// TestFinishPrefersRecordedStepsOverRequestCount pins that totalSteps comes
// from the recorded steps (the real step count), not the request count: two
// acquires (Requests=2) with one recorded step FINISHes totalSteps=1.
func TestFinishPrefersRecordedStepsOverRequestCount(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	run, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Release(run)
	second, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if second != run {
		t.Fatal("acquires did not share one run")
	}
	mgr.Release(run)
	if n := run.NextStepNumber(); n != 1 {
		t.Fatalf("NextStepNumber = %d, want 1", n)
	}
	mgr.RecordStep(run, "chatcmpl-1")

	status, steps, totalSteps := mgr.finishPayload(run)
	if status != "completed" || len(steps) != 1 || totalSteps != 1 {
		t.Fatalf("finishPayload = (%q, %d steps, totalSteps %d), want (completed, 1, 1) with Requests=%d",
			status, len(steps), totalSteps, run.Requests)
	}
	mgr.Shutdown(context.Background())
}

// TestStepCounterResetsOnRotation pins the counter scope: 1..N threads within
// a run, and a rotated run starts again at 1 (no cross-run carryover).
func TestStepCounterResetsOnRotation(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	const rotInt = 40 * time.Millisecond
	mgr, _ := newTestManager(t, mock, rotInt)

	first, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 3; i++ {
		if got := first.NextStepNumber(); got != i {
			t.Fatalf("first run stamp %d: got %d", i, got)
		}
	}
	mgr.Release(first)

	ageRun(t, mgr, agentA, 2*rotInt)
	second, err := mgr.Acquire(context.Background(), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("rotation did not mint a new run")
	}
	if got := second.NextStepNumber(); got != 1 {
		t.Errorf("rotated run NextStepNumber = %d, want 1", got)
	}
	mgr.Release(second)
	mgr.Shutdown(context.Background())
}
