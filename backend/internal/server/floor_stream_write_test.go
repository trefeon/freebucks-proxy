package server_test

import (
	"encoding/json"
	"freebuff-proxy/backend/internal/testutil"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// Streaming parity for the write payload. An xd:// device call carries its
// args as a nested `content` object; the streaming leg withholds the CLI
// fragments and injects the reshaped whole on the terminal chunk, and that
// whole must keep the nested payload — a blanked content reaches the device
// as empty args and the device never executes (live: "call the write tool to
// resolve" with no file change for ast_edit/reject/lsp/checkpoint/...).

// writeFloorTools is the floor trigger with a declared `write` client tool so
// the wire `write_file` restores to the OMP dispatch name.
func writeFloorTools() string {
	return strings.TrimSuffix(ompFloorTools(), "]") +
		`,{"type":"function","function":{"name":"write","description":"Write","parameters":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}}}]`
}

func TestFloorStreamWriteDeviceContentPreserved(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	// The nested args arrive split across two fragments, as upstream streams
	// them; only the terminal chunk can carry the reshaped whole.
	frag1 := `{"path":"xd://tts","content":{"text":"hi"`
	frag2 := `,"voice":"af_sky"}}`
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-w", 1,
			`"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_w","type":"function","function":{"name":"write_file","arguments":`+strconv.Quote(frag1)+`}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-w", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":`+strconv.Quote(frag2)+`}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-w", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"speak"}],"stream":true,"tools":` + writeFloorTools() + `}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	assertFloorOnlyWire(t, mock)
	frames, done := collectOpenAIFrames(t, string(data))
	if !done {
		t.Error("stream missing [DONE]")
	}
	if name := toolCallName(frames, 0); name != "write" {
		t.Fatalf("call name = %q, want write (OMP dispatch name)", name)
	}
	args := joinToolArgs(frames, 0)
	var got map[string]any
	if err := json.Unmarshal([]byte(args), &got); err != nil {
		t.Fatalf("injected args invalid JSON %q: %v", args, err)
	}
	if got["path"] != "xd://tts" {
		t.Errorf("path = %v, want the xd:// device path", got["path"])
	}
	if got["content"] != `{"text":"hi","voice":"af_sky"}` {
		t.Errorf("content = %#v, want the marshaled nested payload (a blanked content strands the device)", got["content"])
	}
}
