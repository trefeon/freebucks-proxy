package server

import (
	"testing"
)

func TestSurfaceForResolvesEachFamily(t *testing.T) {
	for _, want := range []struct {
		fam   SurfaceFamily
		route string
	}{
		{SurfaceChat, "/v1/chat/completions"},
		{SurfaceAnthropic, "/v1/messages"},
		{SurfaceResponses, "/v1/responses"},
	} {
		s, err := SurfaceFor(want.fam)
		if err != nil {
			t.Errorf("SurfaceFor(%q): %v", want.fam, err)
			continue
		}
		if s.Route() != want.route {
			t.Errorf("SurfaceFor(%q).Route() = %q, want %q", want.fam, s.Route(), want.route)
		}
		if s.Family() != want.fam {
			t.Errorf("Surface.Family() = %q, want %q", s.Family(), want.fam)
		}
	}
}

func TestSurfaceForRejectsUnknown(t *testing.T) {
	if _, err := SurfaceFor(SurfaceFamily("gemini")); err == nil {
		t.Error("SurfaceFor(gemini) succeeded, want error (no generic fallback surface)")
	}
	if _, err := SurfaceFor(SurfaceFamily("")); err == nil {
		t.Error("SurfaceFor(\"\") succeeded, want error")
	}
}

func TestSurfacesAreDistinctRoutes(t *testing.T) {
	seen := map[string]SurfaceFamily{}
	for _, s := range Surfaces() {
		if prev, dup := seen[s.Route()]; dup {
			t.Errorf("route %q shared by %q and %q, want one surface per route", s.Route(), prev, s.Family())
		}
		seen[s.Route()] = s.Family()
	}
	if len(seen) != 3 {
		t.Errorf("Surfaces() = %d routes, want 3", len(seen))
	}
}

func TestSurfacesMatchUpstreamFamilies(t *testing.T) {
	// The server envelope set and the upstream adapter family set agree
	// one-to-one: every surface has exactly one wire adapter behind it, so
	// no surface can route family-to-family.
	want := map[SurfaceFamily]bool{
		SurfaceChat: true, SurfaceAnthropic: true, SurfaceResponses: true,
	}
	for _, s := range Surfaces() {
		if !want[s.Family()] {
			t.Errorf("surface %q has no upstream adapter family", s.Family())
		}
		delete(want, s.Family())
	}
	for fam := range want {
		t.Errorf("upstream family %q has no server surface", fam)
	}
}
