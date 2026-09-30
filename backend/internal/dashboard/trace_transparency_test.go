package dashboard

import "testing"

// traceFromFields must surface the transparency keys the chat path logs:
// tools count, floor_only marker, and usage=absent — omitted otherwise so
// old rows render unchanged.
func TestTraceFromFieldsTransparencyKeys(t *testing.T) {
	e := traceFromFields("2026-09-30T00:00:00Z", []string{
		"token=1", "model=m", "status=ok", "ms=42", "tools=16", "floor_only=true",
		"input=100", "total=165",
	})
	if e.Tools != 16 {
		t.Errorf("Tools = %d, want 16", e.Tools)
	}
	if !e.FloorOnly {
		t.Error("FloorOnly = false, want true")
	}
	if e.UsageAbsent {
		t.Error("UsageAbsent = true, want false (usage observed)")
	}
	absent := traceFromFields("2026-09-30T00:00:00Z", []string{
		"token=1", "model=m", "status=ok", "ms=42", "tools=2", "usage=absent",
	})
	if !absent.UsageAbsent {
		t.Error("UsageAbsent = false, want true (usage=absent logged)")
	}
	if absent.Tools != 2 {
		t.Errorf("Tools = %d, want 2", absent.Tools)
	}
	if absent.FloorOnly {
		t.Error("FloorOnly = true, want false (not logged)")
	}
	bare := traceFromFields("2026-09-30T00:00:00Z", []string{"token=1", "model=m"})
	if bare.Tools != 0 || bare.FloorOnly || bare.UsageAbsent {
		t.Errorf("absent keys must stay zero, got %+v", bare)
	}
}
