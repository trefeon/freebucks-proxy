package pool

// Released-purchase dashboard surfacing (token-1 deepseek wedge, live
// 2026-10-01): after live 409 purchase_claim_released admissions, the token
// snapshot names the released model so the operator sees which purchase is
// gone.

import (
	"context"
	"freebuff-proxy/backend/internal/testutil"
	"io"
	"net/http"
	"testing"
)

func TestReleasedModelsSurfaceInTokenSnapshot(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"status":"purchase_claim_released","message":"claim released"}`)
	}
	p := newTestPool(t, mock)

	entry := (*p.roster.Load())[0]
	if _, err := entry.session.EnsureSessionForModel(context.Background(), modelB); err == nil {
		t.Fatal("want the released 409, got nil")
	}
	snap := p.Snapshot()[0]
	if len(snap.ReleasedModels) != 1 || snap.ReleasedModels[0] != modelB {
		t.Fatalf("ReleasedModels = %q, want [%q]", snap.ReleasedModels, modelB)
	}
}
