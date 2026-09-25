package server_test

import (
	"net/http"
	"strings"
	"testing"

	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/testutil"
)

// Token order mutations (swap/move) persist the reordered AUTH_TOKENS list
// to the overlay (unified-store); the .env seed is never rewritten.
func TestAdminTokensSwap(t *testing.T) {
	ts, _, srv, st := newStoreBackedServer(t, nil, func(c *config.Config) { c.AdminToken = "secret" },
		testutil.NewMock(), testutil.NewMock(), testutil.NewMock())
	cookie := authedCookie(t, ts)
	overlayPool := func(want string) {
		t.Helper()
		srv.FlushSettingsSpill()
		row, ok, err := st.GetSetting(config.OverlayRowKey("AUTH_TOKENS"))
		if err != nil || !ok || row != want {
			t.Errorf("overlay AUTH_TOKENS = %q,%v,%v, want %q", row, ok, err, want)
		}
	}

	// Swap token 0 and 1 (promote tok-1 to index 0)
	resp := postJSON(t, ts.URL, cookie, "/admin/tokens/swap", `{"from":0,"to":1}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("swap status = %d, want 200", resp.StatusCode)
	}
	body := bodyOf(t, resp)
	if !strings.Contains(body, "swapped") {
		t.Errorf("swap response = %q, want mention of swapped", body)
	}

	overlayPool("tok-1,tok-0,tok-2")

	// Directional swap: move token 2 up (swap 2 and 1)
	resp = postJSON(t, ts.URL, cookie, "/admin/tokens/swap", `{"index":2,"direction":"up"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("swap up status = %d, want 200", resp.StatusCode)
	}

	overlayPool("tok-1,tok-2,tok-0")

	// Move action: move token at index 2 to index 0: [tok-1, tok-2, tok-0] -> [tok-0, tok-1, tok-2]
	resp = postJSON(t, ts.URL, cookie, "/admin/tokens/swap", `{"from":2,"to":0,"action":"move"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("move status = %d, want 200", resp.StatusCode)
	}
	overlayPool("tok-0,tok-1,tok-2")

	// Out of bounds check
	resp = postJSON(t, ts.URL, cookie, "/admin/tokens/swap", `{"from":0,"to":10}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("out-of-bounds status = %d, want 400", resp.StatusCode)
	}
}
