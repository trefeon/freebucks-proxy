package upstream

import (
	"testing"
)

func TestAdapterRegistryDefaults(t *testing.T) {
	r := DefaultAdapterRegistry()
	if got := r.Families(); len(got) != 3 {
		t.Fatalf("Families() = %v, want all three surfaces", got)
	}
	for _, fam := range []Family{FamilyChat, FamilyAnthropic, FamilyResponses} {
		a, ok := r.Lookup(fam)
		if !ok {
			t.Errorf("Lookup(%q) missing", fam)
			continue
		}
		if a.Family() != fam {
			t.Errorf("adapter Family() = %q, want %q", a.Family(), fam)
		}
		if a.Endpoint() == "" {
			t.Errorf("adapter %q has empty endpoint", fam)
		}
	}
	if _, ok := r.Lookup(Family("gemini")); ok {
		t.Error("Lookup(gemini) hit, want miss (no second upstream dialect)")
	}
}

func TestAdapterRegistryRejectsDuplicateFamily(t *testing.T) {
	r := NewAdapterRegistry()
	if err := r.Register(staticAdapter{family: FamilyChat, endpoint: "/x"}); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := r.Register(staticAdapter{family: FamilyChat, endpoint: "/y"}); err == nil {
		t.Error("second Register for one family succeeded, want error (one surface per family)")
	}
}

func TestAdapterRegistryRejectsUnknownFamily(t *testing.T) {
	r := NewAdapterRegistry()
	if err := r.Register(staticAdapter{family: Family("openclaw"), endpoint: "/x"}); err == nil {
		t.Error("Register(unknown family) succeeded, want error")
	}
	if err := r.Register(nil); err == nil {
		t.Error("Register(nil) succeeded, want error")
	}
}

func TestAdaptersShareOneWireTarget(t *testing.T) {
	// All three surfaces drive the same upstream wire: the family selects
	// the client envelope, never the wire target.
	seen := map[string]bool{}
	for _, a := range DefaultAdapters() {
		seen[a.Endpoint()] = true
	}
	if len(seen) != 1 {
		t.Errorf("adapters drive %d wire endpoints, want exactly one", len(seen))
	}
}
