package pool

import (
	"context"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// TestStuckQueueRowStrikesOut pins the wedged-row recovery behind the live
// 2026-09-29 storm: token 2's deepseek row polled active yet refused 7
// straight chats with waiting_room_queued. Three consecutive queue refusals
// against one session instance mark the row stuck (NoteWaitingRoomQueue
// returns true); a new instance, a served chat, or a missing entry resets or
// never counts.
func TestStuckQueueRowStrikesOut(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	defer p.LeaseRelease(lease)
	if lease.SessionInstanceID == "" {
		t.Fatal("lease has no session instance id")
	}

	if lease.NoteWaitingRoomQueue() {
		t.Error("strike 1 returned stuck, want false")
	}
	if lease.NoteWaitingRoomQueue() {
		t.Error("strike 2 returned stuck, want false")
	}
	if !lease.NoteWaitingRoomQueue() {
		t.Error("strike 3 returned not-stuck, want true (row is wedged)")
	}

	// A served chat clears the count: the next queue starts over.
	p.roster.recordChatEntry(lease.entry)
	if lease.NoteWaitingRoomQueue() {
		t.Error("post-success strike 1 returned stuck, want false")
	}
}

func TestStuckQueueNewInstanceResets(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	defer p.LeaseRelease(lease)

	lease.NoteWaitingRoomQueue()
	lease.NoteWaitingRoomQueue()
	lease.SessionInstanceID = "inst-fresh-admit"
	if lease.NoteWaitingRoomQueue() {
		t.Error("fresh-instance strike returned stuck, want false (count restarted at 1)")
	}
	if lease.NoteWaitingRoomQueue() {
		t.Error("second fresh-instance strike returned stuck, want false")
	}
	if !lease.NoteWaitingRoomQueue() {
		t.Error("third fresh-instance strike returned not-stuck, want true")
	}
}

func TestStuckQueueNilSafe(t *testing.T) {
	var nilLease *Lease
	if nilLease.NoteWaitingRoomQueue() {
		t.Error("nil lease returned stuck, want false")
	}
	if (&Lease{}).NoteWaitingRoomQueue() {
		t.Error("entry-less lease returned stuck, want false")
	}
}
