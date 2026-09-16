package server_test

// Trace-continuity regression tests: the minted req_id is echoed to callers
// as X-Request-Id (never the spoofable inbound value), and the chat access
// line carries the serving token's label for abusive-key triage.

import (
	"freebuff-proxy/backend/internal/testutil"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

var reqIDEchoUUIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestAccessResponseEchoesMintedReqID verifies the trace-continuity addition:
// every response through the access wrapper carries the MINTER req_id as
// X-Request-Id, and the header equals the access line's req_id. The inbound
// X-Request-Id is still only logged as client_request_id, never adopted.
func TestAccessResponseEchoesMintedReqID(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	_, ring, logger := debugRing(t)
	ts, _ := newTestServerWithLogger(t, nil, logger, ring, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA),
		map[string]string{"X-Request-Id": "abc"})
	if !strings.Contains(string(data), "Hello") {
		t.Fatalf("chat stream unexpected: %s", truncate(string(data), 200))
	}
	hdr := resp.Header.Get("X-Request-Id")
	if hdr == "" {
		t.Fatal("response missing X-Request-Id header")
	}
	if hdr == "abc" {
		t.Fatalf("X-Request-Id = %q: inbound client id was adopted, want the minted req_id", hdr)
	}
	if !reqIDEchoUUIDRe.MatchString(hdr) {
		t.Errorf("X-Request-Id = %q, want UUIDv4 shape", hdr)
	}
	var clientReqID string
	found := false
	for _, e := range ring.Recent(100) {
		if e.Message != "access" {
			continue
		}
		if entryField(e, "req_id") == hdr {
			clientReqID = entryField(e, "client_request_id")
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no access entry with req_id = header %q", hdr)
	}
	if clientReqID != "abc" {
		t.Errorf("access client_request_id = %q, want abc (inbound preserved, not adopted)", clientReqID)
	}
	// Same inbound id on a second request mints a FRESH req_id.
	resp2, data2 := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA),
		map[string]string{"X-Request-Id": "abc"})
	if !strings.Contains(string(data2), "Hello") {
		t.Fatalf("second chat stream unexpected: %s", truncate(string(data2), 200))
	}
	if hdr2 := resp2.Header.Get("X-Request-Id"); hdr2 == hdr || hdr2 == "abc" {
		t.Errorf("second X-Request-Id = %q, want a fresh minted id distinct from %q", hdr2, hdr)
	}

	// Silent paths (healthz emits no access line) still echo the header:
	// the echo is set before the silent-path return in the wrapper.
	hresp, _ := doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if h := hresp.Header.Get("X-Request-Id"); h == "" || !reqIDEchoUUIDRe.MatchString(h) {
		t.Errorf("healthz X-Request-Id = %q, want a minted UUID on silent paths too", h)
	}
}

// TestChatAccessLineCarriesTokenLabel verifies the access-log field addition:
// a served chat's access line carries token=<label> matching the chat
// routing line's label (1-based index or "bridge"), never the raw key.
func TestChatAccessLineCarriesTokenLabel(t *testing.T) {
	const rawKey = "sk-test-access-label-key"
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	_, ring, logger := debugRing(t)
	ts, _ := newTestServerWithLogger(t, []string{rawKey}, logger, ring, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA),
		map[string]string{"Authorization": "Bearer " + rawKey})
	if !strings.Contains(string(data), "Hello") {
		t.Fatalf("chat stream unexpected: %s", truncate(string(data), 200))
	}
	hdr := resp.Header.Get("X-Request-Id")
	if hdr == "" {
		t.Fatal("response missing X-Request-Id header")
	}
	entries := ring.Recent(100)
	var accessTok, routingTok string
	foundAccess, foundRouting := false, false
	for i := range entries {
		e := &entries[i]
		if entryField(*e, "req_id") != hdr {
			continue
		}
		switch e.Message {
		case "access":
			foundAccess = true
			accessTok = entryField(*e, "token")
		case "chat routing":
			foundRouting = true
			routingTok = entryField(*e, "token")
		}
	}
	if !foundAccess {
		t.Fatalf("no access entry for req_id %q", hdr)
	}
	if !foundRouting {
		t.Fatalf("no chat routing entry for req_id %q", hdr)
	}
	if accessTok == "" {
		t.Fatal("access entry missing token field (want the serving lease label)")
	}
	if accessTok != routingTok {
		t.Errorf("access token = %q, routing token = %q: want the same label", accessTok, routingTok)
	}
	if strings.Contains(accessTok, rawKey) {
		t.Errorf("access token = %q: raw key leaked into the access line", accessTok)
	}
	if ok, _ := regexp.MatchString(`^(bridge|[1-9][0-9]*)$`, accessTok); !ok {
		t.Errorf("access token = %q, want 1-based index or \"bridge\"", accessTok)
	}
}
