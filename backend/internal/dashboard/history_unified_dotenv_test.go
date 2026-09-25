package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnifiedStoreNoDotenvImports pins the I4 file contract for the
// dashboard history plane: no dotenv/envfile import, no .env path reference.
// The plane persists through the store handle only; boot seeding from .env
// (when the DB is empty) lives outside this plane.
func TestUnifiedStoreNoDotenvImports(t *testing.T) {
	for _, rel := range []string{
		"backend/internal/dashboard/dashboard_history.go",
		"backend/internal/dashboard/dashboard_logs.go",
	} {
		abs, err := filepath.Abs(filepath.Join("..", "..", "..", rel))
		if err != nil {
			t.Fatalf("Abs(%s): %v", rel, err)
		}
		raw, err := os.ReadFile(abs)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", rel, err)
		}
		for _, bad := range []string{"godotenv", "envfile", "dotenv", ".env", "os.Getenv", "Getenv"} {
			if strings.Contains(string(raw), bad) {
				t.Fatalf("%s references %q (dotenv leg forbidden in the history plane)", rel, bad)
			}
		}
	}
}
