package convert

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The canonical fixture is a copy of backend/internal/upstream/testdata/
// cli-tools.json (convert may not import upstream per the archtest matrix).
// This guard fails on any drift so the copy is refreshed deliberately.
func TestCanonicalFixtureMatchesUpstream(t *testing.T) {
	local, err := os.ReadFile(filepath.Join("testdata", "cli-tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	up, err := os.ReadFile(filepath.Join("..", "upstream", "testdata", "cli-tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(local, up) {
		t.Error("convert/testdata/cli-tools.json drifted from upstream/testdata/cli-tools.json: copy it over")
	}
	if len(canonicalTools) == 0 {
		if _, err := canonicalToolDefs(); err != nil {
			t.Fatal(err)
		}
	}
	if len(canonicalTools) != 16 {
		t.Errorf("canonical tools = %d, want 16", len(canonicalTools))
	}
}
