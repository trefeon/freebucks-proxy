package pool

import (
	"slices"
	"testing"
	"time"
)

func TestKeyPoolDrawRotates(t *testing.T) {
	k := &KeyPool{}
	first := k.Draw("m", 3)
	second := k.Draw("m", 3)
	third := k.Draw("m", 3)
	if !slices.Equal(first, []int{0, 1, 2}) {
		t.Errorf("first Draw = %v, want [0 1 2]", first)
	}
	if !slices.Equal(second, []int{1, 2, 0}) {
		t.Errorf("second Draw = %v, want [1 2 0] (cursor advanced)", second)
	}
	if !slices.Equal(third, []int{2, 0, 1}) {
		t.Errorf("third Draw = %v, want [2 0 1]", third)
	}
}

func TestKeyPoolSkipsCooling(t *testing.T) {
	now := time.Now()
	cur := now
	k := &KeyPool{now: func() time.Time { return cur }}
	k.NoteRateLimited("m", 0, time.Minute)
	if got := k.Draw("m", 3); !slices.Equal(got, []int{1, 2}) {
		t.Errorf("Draw with lane 0 cooling = %v, want [1 2]", got)
	}
	if !k.Cooling("m", 0) {
		t.Error("Cooling(lane 0) = false, want true inside the window")
	}
	if k.Cooling("m", 1) {
		t.Error("Cooling(lane 1) = true, want false (never parked)")
	}
	cur = now.Add(2 * time.Minute)
	if k.Cooling("m", 0) {
		t.Error("Cooling(lane 0) = true after the window, want false (re-admit)")
	}
	if got := k.Draw("m", 3); len(got) != 3 {
		t.Errorf("Draw after expiry = %v, want all 3 lanes back", got)
	}
}

func TestKeyPoolAllCoolingFallsBack(t *testing.T) {
	k := &KeyPool{}
	k.NoteRateLimited("m", 0, time.Minute)
	k.NoteRateLimited("m", 1, time.Minute)
	if got := k.Draw("m", 2); got != nil {
		t.Errorf("Draw(all cooling) = %v, want nil (caller falls back to strict order)", got)
	}
}

func TestKeyPoolModelsAreIndependent(t *testing.T) {
	k := &KeyPool{}
	k.NoteRateLimited("a", 0, time.Minute)
	if got := k.Draw("b", 2); !slices.Equal(got, []int{0, 1}) {
		t.Errorf("Draw(other model) = %v, want [0 1] (cooldown is per model)", got)
	}
}

func TestKeyPoolDefaultCooldown(t *testing.T) {
	now := time.Now()
	cur := now
	k := &KeyPool{now: func() time.Time { return cur }}
	k.NoteRateLimited("m", 1, 0)
	if !k.Cooling("m", 1) {
		t.Fatal("zero-duration 429 did not park the lane (want the default window)")
	}
	cur = now.Add(keyCooldownDefault + time.Second)
	if k.Cooling("m", 1) {
		t.Error("lane still cooling past the default window, want re-admit")
	}
}

func TestKeyPoolEmptyDraw(t *testing.T) {
	k := &KeyPool{}
	if got := k.Draw("m", 0); got != nil {
		t.Errorf("Draw(0) = %v, want nil", got)
	}
}
