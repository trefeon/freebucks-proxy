package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
)

// TestSendPostsPayload verifies the webhook POST carries the JSON event
// contract (#48): event name, token index, model, message, RFC3339
// timestamp.
func TestSendPostsPayload(t *testing.T) {
	var got atomic.Value // Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev Event
		if err := json.NewDecoder(r.Body).Decode(&ev); err == nil {
			got.Store(ev)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := New(srv.URL, nil)
	s.Send(Event{Event: "token_banned", TokenIndex: 3, Model: "deepseek/deepseek-v4-pro", Message: "banned"})
	testutil.WaitFor(t, 3*time.Second, func() bool {
		ev, ok := got.Load().(Event)
		return ok && ev.Event != ""
	}, "webhook POST never arrived")
	ev := got.Load().(Event)
	if ev.Event != "token_banned" || ev.TokenIndex != 3 || ev.Model != "deepseek/deepseek-v4-pro" || ev.Message != "banned" {
		t.Fatalf("payload = %+v, want full event contract", ev)
	}
	if _, err := time.Parse(time.RFC3339, ev.Timestamp); err != nil {
		t.Fatalf("timestamp %q not RFC3339: %v", ev.Timestamp, err)
	}
}

// TestSendThrottle verifies at most one POST per event type per 5m window
// (issue #48 dedupe), while distinct event types each get their own slot.
func TestSendThrottle(t *testing.T) {
	var count atomic.Int64
	var mu sync.Mutex
	var events []Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		var ev Event
		if err := json.NewDecoder(r.Body).Decode(&ev); err == nil {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := New(srv.URL, nil)
	s.Send(Event{Event: "pool_exhausted", TokenIndex: 1, Model: "m"})
	s.Send(Event{Event: "pool_exhausted", TokenIndex: 2, Model: "m"}) // throttled
	s.Send(Event{Event: "token_banned", TokenIndex: 1})               // different type: fires
	testutil.WaitFor(t, 3*time.Second, func() bool { return count.Load() >= 2 },
		fmt.Sprintf("webhook POSTs = %d, want 2 (one per event type, 5m throttle)", count.Load()))
	if got := count.Load(); got != 2 {
		t.Fatalf("webhook POSTs = %d, want 2 (one per event type, 5m throttle)", got)
	}

	mu.Lock()
	defer mu.Unlock()
	// The POSTs are fire-and-forget goroutines, so arrival order is
	// nondeterministic — assert set membership, not order.
	byType := make(map[string]Event, len(events))
	for _, ev := range events {
		byType[ev.Event] = ev
	}
	if _, ok := byType["pool_exhausted"]; !ok {
		t.Errorf("events missing pool_exhausted: %+v", events)
	}
	ban, ok := byType["token_banned"]
	if !ok {
		t.Fatalf("events missing token_banned: %+v", events)
	}
	if ban.TokenIndex != 1 || ban.Model != "" {
		t.Errorf("token_banned payload = %+v, want index 1", ban)
	}
	if ban.Timestamp == "" {
		t.Error("timestamp missing from webhook payload")
	}
}

// TestSendDisabledNoOps verifies an empty URL (or nil sender) never POSTs.
func TestSendDisabledNoOps(t *testing.T) {
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	New("", nil).Send(Event{Event: "pool_exhausted"})
	var nilSender *Sender
	nilSender.Send(Event{Event: "pool_exhausted"})
	time.Sleep(50 * time.Millisecond)
	if count.Load() != 0 {
		t.Fatalf("POSTs = %d, want 0 for disabled sender", count.Load())
	}
}

// failRT is a RoundTripper that always fails, for the transport-error path.
type failRT struct{}

func (failRT) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("webhook unreachable")
}

// TestSendFailureLogsWarn verifies T18: a failed webhook delivery logs a
// WARN with the err and the target URL — a non-2xx status and a transport
// error both fire it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestSendFailureLogsWarn(t *testing.T) {
	t.Run("non-2xx status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		var sink lockedBuffer
		s := New(srv.URL, nil)
		s.SetLogger(slog.New(slog.NewTextHandler(&sink, &slog.HandlerOptions{Level: slog.LevelWarn})))
		s.Send(Event{Event: "token_banned"})

		testutil.WaitFor(t, 3*time.Second, func() bool {
			return strings.Contains(sink.String(), "webhook send failed")
		}, "status-failure WARN never logged")
		logs := sink.String()
		for _, want := range []string{"webhook send failed", "webhook returned status 503", "target=" + srv.URL} {
			if !strings.Contains(logs, want) {
				t.Errorf("status-failure WARN missing %q: %s", want, logs)
			}
		}
	})

	t.Run("transport error", func(t *testing.T) {
		var sink lockedBuffer
		s := New("https://webhook.invalid/hook", &http.Client{Transport: failRT{}})
		s.SetLogger(slog.New(slog.NewTextHandler(&sink, &slog.HandlerOptions{Level: slog.LevelWarn})))
		s.Send(Event{Event: "pool_exhausted"})

		testutil.WaitFor(t, 3*time.Second, func() bool {
			return strings.Contains(sink.String(), "webhook send failed")
		}, "transport-failure WARN never logged")
		logs := sink.String()
		for _, want := range []string{"webhook send failed", "webhook unreachable", "target=https://webhook.invalid"} {
			if !strings.Contains(logs, want) {
				t.Errorf("transport-failure WARN missing %q: %s", want, logs)
			}
		}
	})
}

// TestRedactURL pins the log redaction contract: only scheme+host
// survives; userinfo, path, query, and fragment are stripped, and unparseable
// or hostless input yields "<redacted>".
func TestRedactURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://example.com/hook", "https://example.com"},
		{"http://user:pass@host:8080/path?q=1", "http://host:8080"},
		{"https://[::1]:8443/x", "https://[::1]:8443"},
		{"https://webhook.invalid", "https://webhook.invalid"},
		{"", "<redacted>"},
		{"not-a-url", "<redacted>"},
		{"://oops", "<redacted>"},
	}
	for _, tt := range tests {
		if got := RedactURL(tt.in); got != tt.want {
			t.Errorf("RedactURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestSendRejectsRedirect verifies the default client does not follow a
// cross-host (or any) redirect: the redirected endpoint is never hit, and
// the 3xx response is treated as a failed delivery (a WARN).
func TestSendRejectsRedirect(t *testing.T) {
	var finalHits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		finalHits.Add(1)
		w.WriteHeader(200)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var sink lockedBuffer
	s := New(srv.URL+"/redirect", nil)
	s.SetLogger(slog.New(slog.NewTextHandler(&sink, &slog.HandlerOptions{Level: slog.LevelWarn})))
	s.Send(Event{Event: "token_banned"})

	testutil.WaitFor(t, 3*time.Second, func() bool {
		return strings.Contains(sink.String(), "webhook send failed")
	}, "redirect-failure WARN never logged")
	if got := finalHits.Load(); got != 0 {
		t.Fatalf("/final hit %d times, want 0 (redirect must not be followed)", got)
	}
	if !strings.Contains(sink.String(), "webhook returned status 302") {
		t.Errorf("redirect WARN missing status 302: %s", sink.String())
	}
}

// TestSendFailureRedactsURL verifies a WEBHOOK_URL that embeds userinfo or a
// path/query token is never written to the delivery-failure WARN verbatim:
// only the redacted scheme+host appears.
func TestSendFailureRedactsURL(t *testing.T) {
	var sink lockedBuffer
	secret := "https://user:sekret@webhook.invalid/hook?token=abc123"
	s := New(secret, &http.Client{Transport: failRT{}})
	s.SetLogger(slog.New(slog.NewTextHandler(&sink, &slog.HandlerOptions{Level: slog.LevelWarn})))
	s.Send(Event{Event: "pool_exhausted"})

	testutil.WaitFor(t, 3*time.Second, func() bool {
		return strings.Contains(sink.String(), "webhook send failed")
	}, "redaction-failure WARN never logged")
	logs := sink.String()
	if !strings.Contains(logs, "target=https://webhook.invalid") {
		t.Errorf("WARN missing redacted target: %s", logs)
	}
	for _, leak := range []string{"user:sekret", "token=abc123", "/hook?token", "sekret"} {
		if strings.Contains(logs, leak) {
			t.Errorf("WARN leaked %q: %s", leak, logs)
		}
	}
}
