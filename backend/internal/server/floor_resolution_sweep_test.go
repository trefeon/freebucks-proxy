package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// Full resolution sweep for the OMP floor path: for every tool the upstream
// model can call (the 16 canonical CLI floor definitions) the client must
// receive either a name OMP can dispatch or no call at all — never a
// wire-only name, and never a call that vanishes without its payload being
// rendered. Runs the real handler + relay against a scripted upstream, so it
// resolves names exactly as production does (no live account burn).

// Every floor wire name the model can call, with its canonical CLI arg shape
// and the client name it must resolve to ("" = suppressed to assistant text).
var floorResolution = []struct {
	wire string
	args string
	want string // "" means the call is suppressed, not relayed
}{
	{"run_terminal_command", `{"command":"ls","timeout_seconds":30}`, "bash"},
	{"read_files", `{"paths":["a.go"]}`, "read"},
	{"str_replace", `{"path":"f","replacements":[{"oldString":"a","newString":"b"}]}`, "edit"},
	{"write_file", `{"path":"f","content":"hi"}`, "write"},
	{"code_search", `{"pattern":"x","cwd":"src"}`, "grep"},
	{"glob", `{"pattern":"*.go"}`, "glob"},
	{"list_directory", `{"path":"."}`, "read"},
	{"write_todos", `{"todos":[{"task":"a","completed":false}]}`, "todo"},
	{"web_search", `{"query":"q","depth":"deep"}`, "web_search"},
	{"read_url", `{"url":"https://example.test/x"}`, "read"},
	{"ask_user", `{"questions":[{"question":"q?","options":[{"label":"a"}]}]}`, "ask"},
	{"skill", `{"name":"demo-skill"}`, "read"},
	{"gravity_index", `{"action":"search","query":"kv store"}`, "web_search"},
	// No OMP equivalent: suppressed + rendered as text (or absorbed).
	{"suggest_followups", `{"followups":[{"prompt":"Ship it"}]}`, ""},
	{"render_ui", `{"widget":{"type":"button","text":"Open","link":"https://example.test/r"}}`, ""},
	{"report_project_profile", `{"status":"unchanged"}`, ""},
}

// ompDispatchable is the set OMP's dispatcher accepts (builtins + hidden +
// the mcp__ namespace), mirrored in floor_omp_surface_test.go.
func ompDispatchable() map[string]bool {
	set := map[string]bool{}
	for _, n := range ompDispatchableToolNames {
		set[n] = true
	}
	return set
}

func TestFloorEveryToolResolves(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	dispatchable := ompDispatchable()
	for _, tc := range floorResolution {
		t.Run(tc.wire, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
				tfMockTurn(w,
					fmt.Sprintf(`{"index":0,"id":"call_%s","type":"function","function":{"name":%q,"arguments":%q}}`, tc.wire, tc.wire, tc.args),
					"tool_calls")
			}
			ts, _ := newTestServer(t, nil, mock)
			body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"go"}],"stream":false,"tools":` + ompFloorTools() + `}`
			resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
			}
			var comp map[string]any
			if err := json.Unmarshal(data, &comp); err != nil {
				t.Fatalf("response not JSON: %v", err)
			}
			choice := comp["choices"].([]any)[0].(map[string]any)
			msg := choice["message"].(map[string]any)
			tcs, _ := msg["tool_calls"].([]any)

			if tc.want == "" {
				if len(tcs) > 0 {
					t.Fatalf("%s resolved to a call (%v) but must be suppressed", tc.wire, tcs)
				}
				if strings.Contains(string(data), `"`+tc.wire+`"`) {
					t.Errorf("%s name leaked to the client: %s", tc.wire, truncate(string(data), 200))
				}
				if choice["finish_reason"] != "stop" {
					t.Errorf("finish_reason = %v, want stop on a suppressed turn", choice["finish_reason"])
				}
				return
			}
			if len(tcs) == 0 {
				t.Fatalf("%s must resolve to %q, got no call", tc.wire, tc.want)
			}
			for _, raw := range tcs {
				fn, _ := raw.(map[string]any)["function"].(map[string]any)
				name, _ := fn["name"].(string)
				if !dispatchable[name] {
					t.Errorf("%s resolved to %q which OMP cannot dispatch", tc.wire, name)
				}
				if name != tc.want {
					t.Errorf("%s resolved to %q, want %q", tc.wire, name, tc.want)
				}
			}
		})
	}
}

// The inverse direction: a model that emits an OMP name from its own prompt
// vocabulary (instead of the wire schema) must reach the client unchanged.
func TestFloorOMPVocabularyNamesResolve(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	dispatchable := ompDispatchable()
	names := make([]string, 0, len(dispatchable))
	for n := range dispatchable {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
				tfMockTurn(w,
					fmt.Sprintf(`{"index":0,"id":"call_x","type":"function","function":{"name":%q,"arguments":"{}"}}`, name),
					"tool_calls")
			}
			ts, _ := newTestServer(t, nil, mock)
			body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"go"}],"stream":false,"tools":` + ompFloorTools() + `}`
			resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
			}
			var comp map[string]any
			if err := json.Unmarshal(data, &comp); err != nil {
				t.Fatalf("response not JSON: %v", err)
			}
			msg, _ := comp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
			tcs, _ := msg["tool_calls"].([]any)
			if len(tcs) == 0 {
				t.Fatalf("OMP name %q was suppressed; every OMP-dispatchable name must pass through", name)
			}
			got, _ := tcs[0].(map[string]any)["function"].(map[string]any)["name"].(string)
			if got != name {
				t.Errorf("OMP name %q resolved to %q, want identity", name, got)
			}
		})
	}
}
