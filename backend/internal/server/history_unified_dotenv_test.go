package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assertNoDotenvRef pins the I4 file contract for one history-plane file:
// no dotenv/envfile import and no .env path reference. The legacy-carry
// trio staging in store/history.go copies a legacy DB file (never a .env)
// and stays the only filesystem access in that file.
func assertNoDotenvRef(t *testing.T, rel string) {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "..", rel))
	if err != nil {
		t.Fatalf("Abs(%s): %v", rel, err)
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", rel, err)
	}
	body := string(raw)
	for _, bad := range []string{"godotenv", "envfile", "dotenv", ".env"} {
		if strings.Contains(body, bad) {
			t.Fatalf("%s references %q (dotenv leg forbidden in the history plane)", rel, bad)
		}
	}
}
