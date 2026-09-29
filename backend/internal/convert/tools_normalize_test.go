package convert

import (
	"encoding/json"
	"os"
	"testing"
)

// RED: an OMP-shaped request (foreign tool definitions) must normalize to the
// CLI-shape wire: substituted entries carry the canonical CLI description +
// parameters under the mapped wire name, so the gate sees CLI definitions.
func TestNormalizeOMPToolsToCanonicalDefinitions(t *testing.T) {
	raw, err := os.ReadFile("D:/tmp/ompcap/001-req.json")
	if err != nil {
		t.Skip("no capture")
	}
	var cap struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(raw, &cap); err != nil {
		t.Fatal(err)
	}
	wire, _, err := NormalizeRequestMappedOpts([]byte(cap.Body), "", DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(wire, &p); err != nil {
		t.Fatal(err)
	}
	tools, _ := p["tools"].([]any)
	byName := map[string]map[string]any{}
	for _, x := range tools {
		m, _ := x.(map[string]any)
		fn, _ := m["function"].(map[string]any)
		n, _ := fn["name"].(string)
		byName[n] = fn
	}
	// Substituted entries must carry canonical CLI definitions, not the
	// client's OMP schemas. OMP read has {path}; the canonical read_files
	// has {paths}. OMP bash {command,...}; run_terminal_command has
	// {command,process_type,cwd,timeout_seconds}.
	rt, ok := byName["run_terminal_command"]
	if !ok {
		t.Fatal("wire missing run_terminal_command")
	}
	params, _ := rt["parameters"].(map[string]any)
	props, _ := params["properties"].(map[string]any)
	if _, ok := props["process_type"]; !ok {
		t.Errorf("run_terminal_command schema not canonical (missing process_type): %v", props)
	}
	rf, ok := byName["read_files"]
	if !ok {
		t.Fatal("wire missing read_files")
	}
	rparams, _ := rf["parameters"].(map[string]any)
	rprops, _ := rparams["properties"].(map[string]any)
	if _, ok := rprops["paths"]; !ok {
		t.Errorf("read_files schema not canonical (missing paths): %v", rprops)
	}
	// No OMP-foreign schema may ride under any name.
	for name, fn := range byName {
		blob, merr := json.Marshal(fn["parameters"])
		if merr != nil {
			t.Fatalf("marshal wire tool %q: %v", name, merr)
		}
		if s := string(blob); len(s) > 0 && containsOMPIntent(s) {
			t.Errorf("wire tool %q carries OMP-shaped schema: %s", name, s[:120])
		}
	}
}

func containsOMPIntent(s string) bool {
	return len(s) > 12 && (hasKey(s, `"i":{"type":"string"}`) || hasKey(s, `"async"`))
}

func hasKey(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
