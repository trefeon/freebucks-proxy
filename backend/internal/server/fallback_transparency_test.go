// fallback_transparency_test.go — issue #164: fallback transparency.
//
// The gateway must tell clients what model actually served a request:
//   - x-freebuff-served-model on every successful response naming the
//     requested model when served directly;
//   - x-freebuff-fallback ABSENT when no fallback fired;
//   - the response body's model field (chat.completion chunks/body and the
//     Anthropic message_start/message object) reflecting the served model.
package server_test

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// chunkModel renders one OpenAI-style SSE chat chunk with a custom model id
// (the shared chunk() helper bakes modelA; these tests need chunks that echo
// a DIFFERENT model to prove the relay rewrites them).
func chunkModel(id string, model string, payload string) string {
	return `{"id":"` + id + `","object":"chat.completion.chunk","created":1,"model":"` + model + `",` + payload + `}`
}

// chatSSE returns a minimal two-chunk streaming chat body (content + stop)
// whose chunks echo the given model id.
func chatSSE(model string) string {
	return testutil.SSEEvent(chunkModel("chatcmpl-t1", model,
		`"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunkModel("chatcmpl-t1", model,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`))
}

// parseChunkModels extracts the "model" field of every data line in an SSE
// body (assumes chat.completion.chunk objects).
func parseChunkModels(t *testing.T, body string) []string {
	t.Helper()
	var models []string
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			continue
		}
		var chunk struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("unmarshal chunk: %v: %s", err, data)
		}
		models = append(models, chunk.Model)
	}
	return models
}

// TestFallbackTransparencyDirectOpenAI pins the direct-serving contract: no
// fallback config, the requested model serves, so x-freebuff-served-model
// equals the requested model, x-freebuff-fallback is ABSENT, and the body
// model field — streaming chunks and the non-streaming body — reflects the
// served model even when the upstream echo lies.
func TestFallbackTransparencyDirectOpenAI(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// The upstream echoes a DIFFERENT model id than the one it was asked to
	// serve: the relay must stamp the proxy's served model regardless.
	mock.ChatBody = chatSSE("upstream/echo-model")
	srv, _ := newTestServer(t, nil, mock)

	// Streaming path.
	resp, data := doJSON(t, http.MethodPost, srv.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200: %s", resp.StatusCode, data)
	}
	if got := resp.Header.Get("x-freebuff-served-model"); got != modelA {
		t.Errorf("x-freebuff-served-model = %q, want %q (direct serving)", got, modelA)
	}
	if got := resp.Header.Get("x-freebuff-fallback"); got != "" {
		t.Errorf("x-freebuff-fallback = %q, want absent (direct serving)", got)
	}
	for i, m := range parseChunkModels(t, string(data)) {
		if m != modelA {
			t.Errorf("stream chunk %d model = %q, want %q (served model stamped)", i, m, modelA)
		}
	}

	// Non-streaming path.
	resp2, data2 := doJSON(t, http.MethodPost, srv.URL+"/v1/chat/completions",
		[]byte(`{"model":"`+modelA+`","messages":[{"role":"user","content":"ping"}],"stream":false}`), nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("non-stream status = %d, want 200: %s", resp2.StatusCode, data2)
	}
	var comp struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(data2, &comp); err != nil {
		t.Fatalf("unmarshal body: %v: %s", err, data2)
	}
	if comp.Model != modelA {
		t.Errorf("non-stream body model = %q, want %q (served model stamped)", comp.Model, modelA)
	}
	if got := resp2.Header.Get("x-freebuff-served-model"); got != modelA {
		t.Errorf("non-stream x-freebuff-served-model = %q, want %q", got, modelA)
	}
	if got := resp2.Header.Get("x-freebuff-fallback"); got != "" {
		t.Errorf("non-stream x-freebuff-fallback = %q, want absent", got)
	}
}

// anthropicHeaders sends one /v1/messages request with the given headers and
// returns the response, the raw body, and the parsed SSE events when stream.
type anthropicMessageEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Model string `json:"model"`
	} `json:"message"`
}

func parseAnthropicEvents(t *testing.T, body string) []anthropicMessageEvent {
	t.Helper()
	var events []anthropicMessageEvent
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev anthropicMessageEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("unmarshal anthropic event: %v: %s", err, line)
		}
		events = append(events, ev)
	}
	return events
}

// TestFallbackTransparencyAnthropic pins the direct-serving Anthropic
// surface: message_start (streaming) and the message object (non-streaming)
// name the served model, and the fallback header stays absent.
func TestFallbackTransparencyAnthropic(t *testing.T) {
	direct := testutil.NewMock()
	defer direct.Close()
	direct.ChatBody = chatSSE("upstream/echo-model")
	srvDirect, _ := newTestServer(t, []string{"anthropic-key"}, direct)

	// Direct streaming: message_start names the requested model.
	reqBody := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, data := doJSON(t, http.MethodPost, srvDirect.URL+"/v1/messages", []byte(reqBody),
		map[string]string{"anthropic-api-key": "anthropic-key", "anthropic-version": "2023-06-01"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("direct stream status = %d, want 200: %s", resp.StatusCode, data)
	}
	if got := resp.Header.Get("x-freebuff-served-model"); got != modelA {
		t.Errorf("direct x-freebuff-served-model = %q, want %q", got, modelA)
	}
	if got := resp.Header.Get("x-freebuff-fallback"); got != "" {
		t.Errorf("direct x-freebuff-fallback = %q, want absent", got)
	}
	foundStart := false
	for _, ev := range parseAnthropicEvents(t, string(data)) {
		if ev.Type == "message_start" && ev.Message != nil {
			foundStart = true
			if ev.Message.Model != modelA {
				t.Errorf("message_start model = %q, want %q (served model)", ev.Message.Model, modelA)
			}
		}
	}
	if !foundStart {
		t.Error("message_start event not found in direct stream")
	}

	// Direct non-streaming: message object model names the served model.
	reqBodyNS := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":false}`
	respNS, dataNS := doJSON(t, http.MethodPost, srvDirect.URL+"/v1/messages", []byte(reqBodyNS),
		map[string]string{"anthropic-api-key": "anthropic-key", "anthropic-version": "2023-06-01"})
	if respNS.StatusCode != http.StatusOK {
		t.Fatalf("direct non-stream status = %d, want 200: %s", respNS.StatusCode, dataNS)
	}
	var msg struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(dataNS, &msg); err != nil {
		t.Fatalf("unmarshal message: %v: %s", err, dataNS)
	}
	if msg.Model != modelA {
		t.Errorf("non-stream message model = %q, want %q", msg.Model, modelA)
	}
}
