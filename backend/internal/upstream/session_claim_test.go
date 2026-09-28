package upstream

import (
	"context"
	"strings"
	"testing"

	"freebucks-proxy/backend/internal/testutil"
)

// TestCreateSessionForModelWithClaim pins the claim-aware admission POST
// (session-slice G4 wire reuse): the manager's persisted per-token claim
// rides x-freebuff-instance-id instead of a fresh mint, with the
// multi-session attempt headers exactly when the claim is cli:-prefixed.
func TestCreateSessionForModelWithClaim(t *testing.T) {
	t.Run("cli claim rides with attempt headers", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		client, err := New("tok-a", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		const claim = "cli:123e4567-e89b-42d3-a456-426614174000"
		if _, err := client.CreateSessionForModelWithClaim(context.Background(), "deepseek/deepseek-v4-flash", claim); err != nil {
			t.Fatal(err)
		}
		if len(mock.RecordedSessionCreates) != 1 {
			t.Fatalf("recorded %d creates, want 1", len(mock.RecordedSessionCreates))
		}
		got := mock.RecordedSessionCreates[0]
		if got.Get("x-freebuff-instance-id") != claim {
			t.Errorf("instance-id = %q, want the held claim %q (no fresh mint)", got.Get("x-freebuff-instance-id"), claim)
		}
		if got.Get("x-freebuff-desktop-attempt-id") != "123e4567-e89b-42d3-a456-426614174000" {
			t.Errorf("attempt-id = %q, want the claim suffix", got.Get("x-freebuff-desktop-attempt-id"))
		}
		if got.Get("x-freebuff-multi-session") != "1" || got.Get("x-freebuff-purchase-continuity") != "1" {
			t.Errorf("multi-session headers = %q/%q, want 1/1", got.Get("x-freebuff-multi-session"), got.Get("x-freebuff-purchase-continuity"))
		}
		if claims := mock.SessionCreateClaimsSnapshot(); len(claims) != 1 || claims[0] != claim {
			t.Errorf("claims snapshot = %q, want [%q]", claims, claim)
		}
	})

	t.Run("empty claim mints fresh attempt identity", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		client, err := New("tok-b", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.CreateSessionForModelWithClaim(context.Background(), "deepseek/deepseek-v4-flash", ""); err != nil {
			t.Fatal(err)
		}
		if len(mock.RecordedSessionCreates) != 1 {
			t.Fatalf("recorded %d creates, want 1", len(mock.RecordedSessionCreates))
		}
		got := mock.RecordedSessionCreates[0]
		inst := got.Get("x-freebuff-instance-id")
		if !strings.HasPrefix(inst, "cli:") || len(inst) <= len("cli:") {
			t.Errorf("instance-id = %q, want a fresh cli:<uuid> mint", inst)
		}
		if got.Get("x-freebuff-desktop-attempt-id") == "" {
			t.Error("attempt-id absent on a minted claim")
		}
	})

	t.Run("legacy non-cli claim rides bare", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		client, err := New("tok-c", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.CreateSessionForModelWithClaim(context.Background(), "deepseek/deepseek-v4-flash", "legacy-trial-id"); err != nil {
			t.Fatal(err)
		}
		if len(mock.RecordedSessionCreates) != 1 {
			t.Fatalf("recorded %d creates, want 1", len(mock.RecordedSessionCreates))
		}
		got := mock.RecordedSessionCreates[0]
		if got.Get("x-freebuff-instance-id") != "legacy-trial-id" {
			t.Errorf("instance-id = %q, want the legacy claim verbatim", got.Get("x-freebuff-instance-id"))
		}
		for _, name := range []string{"x-freebuff-multi-session", "x-freebuff-purchase-continuity", "x-freebuff-desktop-attempt-id"} {
			if v := got.Get(name); v != "" {
				t.Errorf("%s = %q, want absent (attempt headers are cli:-only)", name, v)
			}
		}
	})

	t.Run("offer model keeps legacy identity despite claim", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		client, err := New("tok-d", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.CreateSessionForModelWithClaim(context.Background(), "anthropic/claude-fable-5.1", "cli:123e4567-e89b-42d3-a456-426614174000"); err != nil {
			t.Fatal(err)
		}
		if len(mock.RecordedSessionCreates) != 1 {
			t.Fatalf("recorded %d creates, want 1", len(mock.RecordedSessionCreates))
		}
		got := mock.RecordedSessionCreates[0]
		for _, name := range []string{"x-freebuff-instance-id", "x-freebuff-multi-session", "x-freebuff-purchase-continuity", "x-freebuff-desktop-attempt-id"} {
			if v := got.Get(name); v != "" {
				t.Errorf("%s = %q, want absent (TierOffer keeps the legacy single-session identity)", name, v)
			}
		}
		if claims := mock.SessionCreateClaimsSnapshot(); len(claims) != 1 || claims[0] != "" {
			t.Errorf("claims snapshot = %q, want one empty entry (no claim sent)", claims)
		}
	})

	t.Run("plain admission still mints", func(t *testing.T) {
		// CreateSessionForModel is the empty-claim cutover: no behavior change.
		mock := testutil.NewMock()
		defer mock.Close()
		client, err := New("tok-e", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.CreateSessionForModel(context.Background(), "deepseek/deepseek-v4-flash"); err != nil {
			t.Fatal(err)
		}
		if len(mock.RecordedSessionCreates) != 1 {
			t.Fatalf("recorded %d creates, want 1", len(mock.RecordedSessionCreates))
		}
		if inst := mock.RecordedSessionCreates[0].Get("x-freebuff-instance-id"); !strings.HasPrefix(inst, "cli:") {
			t.Errorf("instance-id = %q, want a fresh cli:<uuid> mint", inst)
		}
	})
}
