package pool

import (
	"context"
	"freebucks-proxy/backend/internal/testutil"
	"regexp"
	"testing"
	"time"
)

// touchStepUUIDRe is the RFC 4122 v4 shape the vendor's agent-step schema
// requires (upstream/freebuff sdk/src/impl/database.ts pendingAgentStepSchema:
// id: z.string().uuid()).
var touchStepUUIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestMaturityTouchFinishStepCarriesUUID pins the streak touch's FINISH step
// id to a v4 UUID. The previous "maturity-touch-<nanos>" id made upstream
// reject the whole FINISH with 400 {"error":"Invalid request body",
// "details":{"steps":{"0":{"id":{"_errors":["Invalid UUID"]}}}}}, and because
// maturityTouchRun discards FinishRun's error the touch still reported ok.
// The real CLI mints crypto.randomUUID() per step
// (sdk/src/impl/database.ts addAgentStep), so a non-UUID id is also a
// foreign-client tell.
func TestMaturityTouchFinishStepCarriesUUID(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	toks := p.roster.Load()
	if toks == nil || len(*toks) != 1 {
		t.Fatalf("roster = %v, want 1 token", toks)
	}
	if err := p.maturityTouchRun(context.Background(), (*toks)[0], modelA, 0, "tok-0", time.Now()); err != nil {
		t.Fatalf("maturityTouchRun: %v", err)
	}

	fins := mock.FinishedRunsSnapshot()
	if len(fins) != 1 {
		t.Fatalf("FINISH payloads = %d, want 1", len(fins))
	}
	if len(fins[0].Steps) != 1 {
		t.Fatalf("FINISH steps = %d, want 1", len(fins[0].Steps))
	}
	if id := fins[0].Steps[0].ID; !touchStepUUIDRe.MatchString(id) {
		t.Errorf("touch step id = %q, want RFC4122 v4 UUID (upstream rejects non-UUID step ids with 400 Invalid request body)", id)
	}
}

// TestMaturityTouchFailedTurnOmitsStep pins the other half of the wire rule:
// a failed touch turn must ship NO step at all. The vendor step enum allows
// only running|completed|skipped and the CLI records a step only for a real
// LLM step, so the old status:"failed" step 400'd the whole FINISH (live:
// `"status":{"_errors":["Invalid option: expected one of \"running\"|
// \"completed\"|\"skipped\""]}`). The run-level "failed" status carries the
// failure instead.
func TestMaturityTouchFailedTurnOmitsStep(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = 503
	mock.ChatErrorBody = `{"error":{"message":"The model is temporarily unavailable. Please try again later.","code":503}}`
	p := newTestPool(t, mock)

	toks := p.roster.Load()
	if toks == nil || len(*toks) != 1 {
		t.Fatalf("roster = %v, want 1 token", toks)
	}
	err := p.maturityTouchRun(context.Background(), (*toks)[0], modelA, 0, "tok-0", time.Now())
	if err == nil {
		t.Fatal("maturityTouchRun succeeded, want the failed-turn error")
	}

	fins := mock.FinishedRunsSnapshot()
	if len(fins) != 1 {
		t.Fatalf("FINISH payloads = %d, want 1", len(fins))
	}
	if len(fins[0].Steps) != 0 {
		t.Errorf("FINISH steps = %d, want 0 on a failed turn (vendor enum has no \"failed\"; the CLI records only real LLM steps)", len(fins[0].Steps))
	}
	if fins[0].Status != "failed" {
		t.Errorf("FINISH status = %q, want failed", fins[0].Status)
	}
	if fins[0].TotalSteps != 0 {
		t.Errorf("FINISH totalSteps = %d, want 0 (one recorded step per shipped step)", fins[0].TotalSteps)
	}
}
