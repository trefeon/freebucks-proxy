package convert

import (
	"encoding/json"
	"os"
	"testing"
)

// Substituted wire names must restore to the client's own names on every
// response path: the model sees canonical CLI definitions, the client sees
// its own tools. Uses the captured OMP envelope end to end.
func TestSubstitutedWireRestoresClientNames(t *testing.T) {
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
	_, mapper, err := NormalizeRequestMappedOpts([]byte(cap.Body), "", DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	// Every mapped OMP name restores from its wire name.
	for client, wire := range map[string]string{
		"bash": "run_terminal_command", "read": "read_files",
		"edit": "str_replace", "write": "write_file",
		"grep": "code_search", "todo": "write_todos",
	} {
		if got := mapper.RestoreName(wire); got != client {
			t.Errorf("RestoreName(%q) = %q, want %q", wire, got, client)
		}
	}
	// Virtualized unmappables restore too.
	for client, wire := range map[string]string{
		"eval": "mcp__eval", "task": "mcp__task",
		"learn": "mcp__learn", "manage_skill": "mcp__manage_skill",
	} {
		if got := mapper.RestoreName(wire); got != client {
			t.Errorf("RestoreName(%q) = %q, want %q", wire, got, client)
		}
	}
	// official names the client used verbatim restore identity.
	for _, name := range []string{"glob", "web_search"} {
		if got := mapper.RestoreName(name); got != name {
			t.Errorf("RestoreName(%q) = %q, want identity", name, got)
		}
	}
}
