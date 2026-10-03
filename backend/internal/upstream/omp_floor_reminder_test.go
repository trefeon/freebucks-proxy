package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// systemStrings returns every text payload of the first system message.
func reminderSystemTexts(t *testing.T, out []byte) []string {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("envelope not JSON: %v", err)
	}
	msgs, _ := payload["messages"].([]any)
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok || msg["role"] != "system" {
			continue
		}
		switch content := msg["content"].(type) {
		case string:
			return []string{content}
		case []any:
			var texts []string
			for _, p := range content {
				if partMap, ok := p.(map[string]any); ok {
					if txt, ok := partMap["text"].(string); ok {
						texts = append(texts, txt)
					}
				}
			}
			return texts
		}
	}
	t.Fatal("no system message in envelope")
	return nil
}

func envelopeTools(t *testing.T, out []byte) []any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("envelope not JSON: %v", err)
	}
	tools, _ := payload["tools"].([]any)
	return tools
}

// OMPFloorOnly appends the harness-only capability reminder to the
// prepended marker: names+shapes ride in prose while the wire keeps the 16
// canonical definitions with zero foreign riders.
func TestInjectEnvelopeOMPFloorReminderPresent(t *testing.T) {
	out, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "r", OMPFloorOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	texts := reminderSystemTexts(t, out)
	joined := strings.Join(texts, "\n")
	if !strings.HasPrefix(texts[0], cliSystemMarkerPhrase) {
		t.Errorf("gate prefix lost: %q", texts[0][:80])
	}
	if !strings.Contains(joined, ompFloorReminderSentinel) {
		t.Fatalf("reminder absent from OMP floor-only envelope: %q", joined)
	}
	for _, want := range []string{"task", `"tasks"`, "todo", "ask", "eval", "wait", "learn", "manage_skill", "non-empty array", "advise", `"severity"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("reminder missing %q: %q", want, joined)
		}
	}
	// Removed from the live 18.5.0 registry: advertising one as callable
	// would route the model into "Tool <name> not found".
	for _, gone := range []string{"hub", "browser", "computer", "inspect_image"} {
		if strings.Contains(joined, gone) {
			t.Errorf("reminder advertises removed tool %q: %q", gone, joined)
		}
	}
	for _, foreign := range []string{`"name":"task"`, `"name":"eval"`, `"name":"wait"`, `"name":"learn"`, `"name":"manage_skill"`, `"name":"context_notes"`, `"name":"new_context"`} {
		if strings.Contains(string(out), foreign) {
			t.Errorf("wire carries foreign rider %s", foreign)
		}
	}
}

// The flag defaults off: pi, unmapped and non-floor clients are
// byte-identical to the pre-reminder envelope.
func TestInjectEnvelopeOMPFloorReminderAbsent(t *testing.T) {
	for _, opts := range []ChatOptions{{RunID: "r"}, {RunID: "r", AgentID: "base3-free-luna", Model: "m"}} {
		out, err := injectEnvelope([]byte(`{"model":"m"}`), "free", opts)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(reminderSystemTexts(t, out), "\n")
		if strings.Contains(joined, ompFloorReminderSentinel) {
			t.Errorf("reminder leaked into non-floor envelope (%+v)", opts)
		}
	}
}

// The reminder rides both canonical markers; the base3 head keeps its own
// opening (never the base2 identity).
func TestInjectEnvelopeOMPFloorReminderBase3(t *testing.T) {
	out, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "r", AgentID: "base3-free-luna", Model: "m", OMPFloorOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	texts := reminderSystemTexts(t, out)
	if !strings.HasPrefix(texts[0], cliSystemMarkerBase3) {
		t.Errorf("base3 opening lost: %q", texts[0][:80])
	}
	if strings.Contains(texts[0], "strategic coding assistant") {
		t.Errorf("base3 run leaked the base2 identity")
	}
	if !strings.Contains(strings.Join(texts, "\n"), ompFloorReminderSentinel) {
		t.Error("reminder absent from base3 floor-only envelope")
	}
}

// Re-injection never duplicates the paragraph, and an already-canonical
// body still gains it exactly once with its prefix intact.
func TestInjectEnvelopeOMPFloorReminderIdempotent(t *testing.T) {
	once, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "r", OMPFloorOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	twice, err := injectEnvelope(once, "free", ChatOptions{RunID: "r", OMPFloorOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(twice), ompFloorReminderSentinel); n != 1 {
		t.Errorf("sentinel count after re-injection = %d, want 1", n)
	}
}

// Structured parts content gains a trailing reminder part; the marker part
// stays first so the gate prefix still holds at the message level.
func TestInjectEnvelopeOMPFloorReminderPartsContent(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"custom instructions"}]}]}`
	out, err := injectEnvelope([]byte(body), "free", ChatOptions{RunID: "r", OMPFloorOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	texts := reminderSystemTexts(t, out)
	if len(texts) == 0 || !strings.HasPrefix(texts[0], cliSystemMarkerPhrase) {
		t.Fatalf("marker part not first: %q", texts)
	}
	if !strings.Contains(texts[len(texts)-1], ompFloorReminderSentinel) {
		t.Errorf("reminder not the trailing part: %q", texts)
	}
}

// The flag moves system text only: the tools array is identical with the
// flag on or off (16 canonical, no foreign riders either way).
func TestInjectEnvelopeOMPFloorReminderWireUntouched(t *testing.T) {
	off, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	on, err := injectEnvelope([]byte(`{"model":"m"}`), "free", ChatOptions{RunID: "r", OMPFloorOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	offTools, _ := json.Marshal(envelopeTools(t, off))
	onTools, _ := json.Marshal(envelopeTools(t, on))
	if string(offTools) != string(onTools) {
		t.Errorf("tools differ with the flag:\noff=%s\non=%s", offTools, onTools)
	}
}
