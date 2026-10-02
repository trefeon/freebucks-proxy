package server

import (
	"freebuff-proxy/backend/internal/testutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMiddlewareOrderDocumentsChain pins the canonical chain: gzip
// outermost, then access+ratelimit, then CORS, then the mux. A reorder
// must update DocumentedMiddlewareOrder and this test together.
func TestMiddlewareOrderDocumentsChain(t *testing.T) {
	want := []MiddlewareLayer{LayerGzip, LayerAccess, LayerCORS, LayerMux}
	got := DocumentedMiddlewareOrder()
	if len(got) != len(want) {
		t.Fatalf("DocumentedMiddlewareOrder() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("DocumentedMiddlewareOrder() = %v, want %v", got, want)
		}
	}
}

// TestMiddlewareCORSPreflight pins the CORS layer inside the chain: an
// OPTIONS preflight on a /v1/* route answers 204 with allow headers.
func TestMiddlewareCORSPreflight(t *testing.T) {
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	srv := newServer(t, mock, nil)
	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("OPTIONS status = %d, want 204 (preflight answered in-chain)", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Error("preflight missing Access-Control-Allow-Origin (CORS layer not in chain)")
	}
}

// TestMiddlewareNeverAdoptsClientRequestID pins the user-headers guard:
// an inbound X-Request-Id is never adopted as the correlation id — the
// response carries a minted id instead.
func TestMiddlewareNeverAdoptsClientRequestID(t *testing.T) {
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	srv := newServer(t, mock, nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-Id", "spoofed-by-client")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	got := rec.Header().Get("X-Request-Id")
	if got == "" {
		t.Fatal("response missing X-Request-Id (access layer must mint exactly once)")
	}
	if got == "spoofed-by-client" {
		t.Error("response adopted the inbound X-Request-Id (spoofable; must mint)")
	}
}

// TestTerminalEventFor pins one terminal per surface: a new surface
// declares its own terminator instead of inheriting one.
func TestTerminalEventFor(t *testing.T) {
	for fam, want := range map[SurfaceFamily]string{
		SurfaceChat:      "[DONE]",
		SurfaceResponses: "response.completed",
		SurfaceAnthropic: "message_stop",
	} {
		got, err := TerminalEventFor(fam)
		if err != nil {
			t.Errorf("TerminalEventFor(%q): %v", fam, err)
			continue
		}
		if got != want {
			t.Errorf("TerminalEventFor(%q) = %q, want %q", fam, got, want)
		}
	}
	if _, err := TerminalEventFor(SurfaceFamily("gemini")); err == nil {
		t.Error("TerminalEventFor(gemini) succeeded, want error (no generic terminator)")
	}
}

// TestAssertTerminalEvent pins truncation detection: a missing terminal
// (or a terminal buried before trailers) is an error naming the surface,
// the want, and the tail.
func TestAssertTerminalEvent(t *testing.T) {
	if err := AssertTerminalEvent("chat", []string{"a", "[DONE]"}, "[DONE]"); err != nil {
		t.Errorf("clean close: %v", err)
	}
	err := AssertTerminalEvent("chat", []string{"a"}, "[DONE]")
	if err == nil || !strings.Contains(err.Error(), "truncated") && !strings.Contains(err.Error(), "without terminal") {
		t.Errorf("missing terminal err = %v, want truncation naming want+tail", err)
	}
	if err := AssertTerminalEvent("chat", nil, "[DONE]"); err == nil {
		t.Error("empty events succeeded, want truncation")
	}
	if err := AssertTerminalEvent("chat", []string{"[DONE]", "trailer"}, "[DONE]"); err == nil {
		t.Error("terminal-before-trailer succeeded, want error (relay must end on the terminal)")
	}
}

// TestIsTerminalEventLine pins the line classifier both wire shapes use:
// verbatim [DONE] on chat, "type" JSON match on event surfaces.
func TestIsTerminalEventLine(t *testing.T) {
	if !IsTerminalEventLine(SurfaceChat, "data: [DONE]") {
		t.Error("chat [DONE] not terminal")
	}
	if IsTerminalEventLine(SurfaceChat, `data: {"a":1}`) {
		t.Error("chat data frame reads terminal, want false")
	}
	if !IsTerminalEventLine(SurfaceResponses, `data: {"type":"response.completed","x":1}`) {
		t.Error("responses completed not terminal")
	}
	if IsTerminalEventLine(SurfaceResponses, "data: [DONE]") {
		t.Error("responses [DONE] reads terminal, want false (Responses never emits it)")
	}
	if !IsTerminalEventLine(SurfaceAnthropic, `data: {"type":"message_stop"}`) {
		t.Error("anthropic message_stop not terminal")
	}
	if IsTerminalEventLine(SurfaceAnthropic, "") || IsTerminalEventLine(SurfaceFamily("x"), "data: [DONE]") {
		t.Error("empty line / unknown family reads terminal, want false")
	}
}
