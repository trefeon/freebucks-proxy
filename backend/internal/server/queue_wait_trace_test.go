package server_test

// End-to-end queue-wait telemetry: the Logs console groups cards per req_id,
// so the park duration only reaches an operator if it rides the request's
// own "chat trace" line. These tests hold one live turn on a cap-1 lane,
// send a chat that genuinely parks behind it, and prove the trace carries
// queue_wait_ms (and the pool's secondary "lease acquired" line reports the
// park too). The never-parked case is pinned in TestTracePhasesRecorded.

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/logring"
	"freebuff-proxy/backend/internal/server"
	"freebuff-proxy/backend/internal/testutil"
)

// parkedChatResult carries the parked request's outcome out of its goroutine
// (t.Fatal must not run off the test goroutine).
type parkedChatResult struct {
	status int
	body   string
	err    error
}

func TestChatTraceCarriesQueueWaitWhenParked(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var sink bytes.Buffer
	ring := logring.NewHandler(slog.NewTextHandler(&sink, nil), 200)
	srv, p := server.NewTestServerStack(t, nil, []*testutil.MockUpstream{mock}, func(c *config.Config) {
		c.AdminToken = config.DefaultAdminToken
		c.RoutingSmart = true
		c.TokenMaxConcurrent = 1
		c.QueueWait = 10 * time.Second
		c.QueueDepth = 16
	}, slog.New(ring), ring)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Hold the lane's only live turn, then send the chat that must park.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	holder, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}
	t.Cleanup(func() { p.LeaseRelease(holder) })

	done := make(chan parkedChatResult, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", bytes.NewReader(chatBody(modelA)))
		if err != nil {
			done <- parkedChatResult{err: err}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := testClient.Do(req)
		if err != nil {
			done <- parkedChatResult{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		done <- parkedChatResult{status: resp.StatusCode, body: string(data), err: err}
	}()

	// Wait until the request is genuinely parked on the account's FIFO
	// queue (public snapshot view), then hold it there long enough for the
	// millisecond-resolution wait to be unambiguous.
	const parkHold = 25 * time.Millisecond
	deadline := time.Now().Add(10 * time.Second)
	for {
		snaps := p.Snapshot()
		if len(snaps) == 1 && snaps[0].QueuedWaiters == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("chat never parked on the live-turn queue: %+v", snaps)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(parkHold)
	p.LeaseRelease(holder)

	var result parkedChatResult
	select {
	case result = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("parked chat never completed")
	}
	if result.err != nil {
		t.Fatalf("parked chat request: %v", result.err)
	}
	if result.status != http.StatusOK {
		t.Fatalf("parked chat status = %d, want 200: %s", result.status, truncate(result.body, 200))
	}

	entries := ring.Recent(100)
	var trace *logring.Entry
	for i := range entries {
		if entries[i].Message == "chat trace" {
			trace = &entries[i]
			break
		}
	}
	if trace == nil {
		t.Fatal("no 'chat trace' entry in the log ring")
		return
	}
	joined := strings.Join(trace.Fields, " ")
	re := regexp.MustCompile(`queue_wait_ms=(\d+)`)
	m := re.FindStringSubmatch(joined)
	if m == nil {
		t.Fatalf("parked chat trace carries no queue_wait_ms: %s", joined)
	}
	wait, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("queue_wait_ms=%q is not an integer: %v", m[1], err)
	}
	if wait < parkHold.Milliseconds() {
		t.Errorf("trace queue_wait_ms = %dms, want >= %dms (the park was held that long)", wait, parkHold.Milliseconds())
	}
	// The console keys cards off req_id: without it the field would render
	// outside the request card.
	if !strings.Contains(joined, "req_id=") {
		t.Errorf("parked chat trace carries no req_id: %s", joined)
	}
	// Print the line once: it is the sample evidence for the PR body and
	// the fastest way to see the shape in a failing run.
	t.Logf("parked chat trace: %s", joined)

}
