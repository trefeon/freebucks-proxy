package pool

// concurrency_ladder_test.go — hermetic concurrency-ladder simulation of the
// pool (the operator's "full simulation").
//
// Goal: prove stick-first distribution (strict positional spill order,
// per-(account,model) slot caps, FIFO parking) and zero errors across
// concurrency levels — with NO real tokens, NO upstream traffic, NO
// freebucks spend (testutil mocks only).
//
// Hermetic boundary (stated, not relitigated): no account tokens were
// supplied, so live fire is impossible without inventing credentials. The
// pool-side logic under test (slot caps, FIFO parking, spill order, lease
// release) is fully exercised by mocks; upstream admission/entitlement is NOT
// exercised — a live-confirmation rung needs an owner device-login (blocked,
// not skipped).
//
// Matrix (5-account pool unless noted; SLOTS_PER_ACCOUNT=2 per
// (account,model) lane, QUEUE_WAIT/QUEUE_DEPTH at the documented Load
// defaults 30s/16; MASQ strict index spill order — the rotation parameter
// on the builders is retained for call-site compatibility but ignored):
//
//	Base:  ladder C=1..6 x drain | least_used
//	A:      10-account pool, ladder C=1..10 x drain | least_used | random
//	B:      10-account pool, 25 concurrent (2 live + 23 parked FIFO)
//	(C:      window-cooldown ladder, excised with cooldown memory — the live
//	 half, refusal without memory plus session survival, lives in
//	 cooldown_session_survive_test.go)
//	D:      5-account pool, 1 quarantined; ladder 1..6 + C=8 + C=10
//	E:      RAMP 5-pool 1-2-3-4-5-6-5-4-3-2-1, 10-pool 2-4-6-8-10-8-6-4-2
//	F:      1-account pool, 5 arrivals, 2 live + 3 parked perfect FIFO
//	G..I:   affinity rounds (drain, 5-pool) — session stickiness
//
// Arrival discipline (deliberate): sequential starts — each worker's Acquire
// begins only after the previous worker granted or provably parked, so
// arrival order == worker order by construction and the distribution is a
// routing measurement instead of a scheduling artifact. Overlap is still
// real: granted leases are HELD until the round's peak is measured
// (all-granted barrier, channel-gated — never slept), so C turns are live
// simultaneously by the pool's own live-turn definition (slotLive), plus a
// barrier-gated concurrent-chat rendezvous at the mocks in the headline round.
//
// Measured-truth notes (probes, not assumptions):
//
//   - Stick-first: same-model arrivals rank the usable holder HEAD even when
//     slot-full and park FIFO on it — no spill while the lane's QUEUE_WAIT
//     holds (settle discipline), no spread. C<=cap fits with zero parks,
//     C>cap holds 2 live + (C-2) parked FIFO on account 1.
//   - least_used piles onto the head account (index order on ties — the mocks
//     serve identical quota) and queues FIFO on it past cap 2 instead of
//     spreading. All parked still succeed on release — zero errors.
//   - Excised with MASQ (strict index spill, no 429 cooldown writes): the
//     round_robin/random spread pins, the window-cooldown ladder, the
//     overflow-assist helper routing and the cross-model bypass. Deleted
//     below, never re-pinned.
import (
	"context"
	"errors"
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	ladCap   = 2
	ladDepth = 16
	ladWait  = 30 * time.Second

	// ladParkDetect is the park-detection heuristic bound: an instant-mock
	// acquire returns in milliseconds, so silence past this bound with a
	// grown model queue (verified via modelQueueDepth, never assumed) means
	// genuinely parked. It is detection only — holds stay channel-gated.
	ladParkDetect = 100 * time.Millisecond
	ladStepTO     = 5 * time.Second
)

// newLadderPool builds a fresh n-account mock pool with the control knobs set
// explicitly and asserted: SLOTS_PER_ACCOUNT=2 with QUEUE_WAIT/QUEUE_DEPTH
// at the documented defaults (overridable for the generous-wait queue
// rounds). The rotation parameter is retained for call-site compatibility
// but ignored: MASQ runs the strict index spill order on every round.
func newLadderPool(t *testing.T, n int, _ string, wait time.Duration) (*Pool, []*testutil.MockUpstream) {
	t.Helper()
	if wait <= 0 {
		wait = ladWait
	}
	mocks := make([]*testutil.MockUpstream, n)
	for i := range mocks {
		mocks[i] = testutil.NewMock()
		t.Cleanup(mocks[i].Close)
	}
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.SlotsPerAccount = ladCap
		c.QueueWait = wait
		c.QueueDepth = ladDepth
	}, mocks...)
	cfg := p.cfg.Load()
	if cfg.SlotsPerAccount != ladCap {
		t.Fatalf("SLOTS_PER_ACCOUNT = %d, want %d (control)", cfg.SlotsPerAccount, ladCap)
	}
	if cfg.QueueDepth != ladDepth {
		t.Fatalf("QUEUE_DEPTH = %d, want %d (control)", cfg.QueueDepth, ladDepth)
	}
	if cap, depth, wt := slotParams(cfg); cap != ladCap || depth != ladDepth || wt != wait {
		t.Fatalf("slotParams = %d/%d/%v, want %d/%d/%v (control)", cap, depth, wt, ladCap, ladDepth, wait)
	}
	return p, mocks
}

// ladWarmLane provisions an already-usable session on one lane without
// taking a slot (direct session-manager admission, no park): the smart
// model queue grants instantly only on a free slot PLUS a usable session,
// so a stone-cold pool would park every wave's first arrival for the full
// QUEUE_WAIT while the scale-out admits it. Cold admission is the
// short-wait tests' business (slot_ledger, spill_queue, queue_wait); the
// ladder measures slot mechanics (caps, FIFO, stick-first distribution),
// so each wave starts with its head lane session-warm. One lane only —
// the creates==1 / creates==0 pins stay meaningful.
func ladWarmLane(t *testing.T, p *Pool, model string, idx int) {
	t.Helper()
	toks := p.roster.Load()
	if idx < 0 || idx >= len(*toks) || (*toks)[idx] == nil {
		t.Fatalf("ladder warm-up lane %d out of range (roster %d)", idx, len(*toks))
	}
	if _, err := (*toks)[idx].session.EnsureSessionForModel(context.Background(), model); err != nil {
		t.Fatalf("ladder warm-up lane %d: %v", idx, err)
	}
}

// ladSlots snapshots per-token live turns and parked waiters.
func ladSlots(p *Pool) (live, queued []int) {
	toks := p.roster.Load()
	live = make([]int, len(*toks))
	queued = make([]int, len(*toks))
	for i := range *toks {
		live[i], queued[i], _ = p.slotEntryStats((*toks)[i])
	}
	return live, queued
}

func ladSum(xs []int) int {
	s := 0
	for _, x := range xs {
		s += x
	}
	return s
}

// assertLadQuiescent pins no leaks: every slot released, every queue empty.
func assertLadQuiescent(t *testing.T, p *Pool, what string) {
	t.Helper()
	live, queued := ladSlots(p)
	if ladSum(live) != 0 || ladSum(queued) != 0 {
		t.Fatalf("%s: not quiescent: live=%v queued=%v (slot/lease leak)", what, live, queued)
	}
}

// assertLadCaps pins the hard wall: no account ever exceeds 2 live turns.
func assertLadCaps(t *testing.T, peak []int, what string) {
	t.Helper()
	for i, v := range peak {
		if v > ladCap {
			t.Fatalf("%s: token %d peak live = %d, want <= %d (slot wall breached)", what, i, v, ladCap)
		}
	}
}

// acquireHeld sequentially acquires n leases on a session-warm head lane
// (deterministic routing) and holds them: all n turns are live
// simultaneously on return.
func acquireHeld(t *testing.T, ctx context.Context, p *Pool, model string, n int) []*Lease {
	t.Helper()
	ladWarmLane(t, p, model, 0)
	held := make([]*Lease, 0, n)
	for range n {
		l, err := p.Acquire(ctx, model)
		if err != nil {
			for _, h := range held {
				p.LeaseRelease(h)
			}
			t.Fatalf("acquireHeld(%d): %v", n, err)
		}
		held = append(held, l)
	}
	return held
}

func releaseHeld(p *Pool, held []*Lease) {
	for _, l := range held {
		p.LeaseRelease(l)
	}
}

func ladAssign(held []*Lease) []int {
	assign := make([]int, len(held))
	for i, l := range held {
		assign[i] = l.Token
	}
	return assign
}

// ladWaveResult is one staggered-start wave: C arrivals in recorded order,
type ladWaveResult struct {
	assign     []int // lease token per worker id (arrival order)
	inst       []string
	parked     []bool
	parkQueue  []int // queue token per worker (-1 when never parked)
	queueWait  []time.Duration
	grantOrder []int // worker ids in grant-completion order
	peak       []int // per-token max live observed
	peakQueued []int // per-token max queued observed
}

// runLadWave fires C staggered arrivals (worker k+1 starts only after worker
// k granted or settled, so arrival order == worker order by construction),
// holds live turns to the measured peak, settles parked FIFO via same-lane
// transfer-releases, then releases all. Zero caller-visible errors; Fatalf
// otherwise. Settled (transfer-released) leases are dead and never released
// twice.
func runLadWave(t *testing.T, p *Pool, model string, c int) ladWaveResult {
	return runLadWaveInner(t, p, model, c, true)
}

func runLadWaveInner(t *testing.T, p *Pool, model string, c int, warm bool) ladWaveResult {
	t.Helper()
	if warm {
		ladWarmLane(t, p, model, 0)
	}
	n := len(*p.roster.Load())
	start := make([]chan struct{}, c)
	resCh := make([]chan ladWorkerRes, c)
	relCh := make([]chan struct{}, c)
	doneCh := make([]chan struct{}, c)
	for i := range c {
		start[i] = make(chan struct{})
		resCh[i] = make(chan ladWorkerRes, 1)
		relCh[i] = make(chan struct{})
		doneCh[i] = make(chan struct{}, 1)
		go func(id int) {
			<-start[id]
			l, err := p.Acquire(context.Background(), model)
			resCh[id] <- ladWorkerRes{lease: l, err: err}
			if err == nil {
				<-relCh[id]
				p.LeaseRelease(l)
			}
			doneCh[id] <- struct{}{}
		}(i)
	}

	r := ladWaveResult{
		assign:     make([]int, c),
		inst:       make([]string, c),
		parked:     make([]bool, c),
		parkQueue:  make([]int, c),
		queueWait:  make([]time.Duration, c),
		peak:       make([]int, n),
		peakQueued: make([]int, n),
	}
	for i := range c {
		r.parkQueue[i] = -1
	}
	sample := func() {
		live, queued := ladSlots(p)
		for i := range live {
			r.peak[i] = max(r.peak[i], live[i])
			r.peakQueued[i] = max(r.peakQueued[i], queued[i])
		}
	}
	heldByToken := make(map[int][]int)
	alive := make(map[int]bool)

	grant := func(id int, l *Lease) {
		r.assign[id] = l.Token
		r.inst[id] = l.SessionInstanceID
		r.queueWait[id] = l.QueueWait
		r.grantOrder = append(r.grantOrder, id)
		alive[id] = true
		heldByToken[l.Token] = append(heldByToken[l.Token], id)
		sample()
	}
	awaitGrant := func(id int, what string) {
		select {
		case wr := <-resCh[id]:
			if wr.err != nil {
				t.Fatalf("wave(%d): worker %d: %v (want zero errors)", c, id, wr.err)
			}
			grant(id, wr.lease)
		case <-time.After(ladStepTO):
			t.Fatalf("wave(%d): worker %d never granted (%s)", c, id, what)
		}
	}

	// Staggered starts with settle: a parked worker is admitted before the
	// next arrival by releasing its lane's earliest holder (the slot
	// transfers directly to the FIFO head, live count unchanged), so no
	// later arrival ever parks on another worker's open election gate and
	// grants complete in arrival order deterministically.
	for i := range c {
		_, prevQueued := ladSlots(p)
		close(start[i])
		select {
		case wr := <-resCh[i]:
			if wr.err != nil {
				t.Fatalf("wave(%d): worker %d: %v (want zero errors)", c, i, wr.err)
			}
			grant(i, wr.lease)
		case <-time.After(ladParkDetect):
			_, queued := ladSlots(p)
			deltas, where := 0, -1
			for k := range queued {
				if d := queued[k] - prevQueued[k]; d > 0 {
					deltas += d
					where = k
				}
			}
			switch deltas {
			case 0:
				awaitGrant(i, "slow acquire")
			case 1:
				r.parked[i] = true
				r.parkQueue[i] = where
				sample()
				victims := heldByToken[where]
				if len(victims) == 0 {
					// Cold head lane (no holders yet — the scale-out
					// admits it after QUEUE_WAIT): nothing to release,
					// so await the scale-out grant. Only the unwarmed
					// admission-storm round parks here.
					awaitGrant(i, "cold admission")
					continue
				}
				rel := victims[0]
				heldByToken[where] = victims[1:]
				delete(alive, rel)
				close(relCh[rel])
				awaitGrant(i, "settle")
			default:
				t.Fatalf("wave(%d): worker %d start grew %d queues (want exactly one)", c, i, deltas)
			}
		}
	}

	for id := range alive {
		close(relCh[id])
	}
	for i := range c {
		select {
		case <-doneCh[i]:
		case <-time.After(ladStepTO):
			t.Fatalf("wave(%d): worker %d never finished", c, i)
		}
	}
	assertLadQuiescent(t, p, "wave")
	return r
}

type ladWorkerRes struct {
	lease *Lease
	err   error
}

// assertLadNoTimeouts pins genuine FIFO admission for parked workers: a
// waiter whose QUEUE_WAIT elapsed would fail over and take an immediate grant
// elsewhere (QueueWait==0 on a foreign token). Same-token + QueueWait>0
// proves the queue granted it.
func assertLadNoTimeouts(t *testing.T, r ladWaveResult, what string) {
	t.Helper()
	for i := range r.assign {
		if !r.parked[i] {
			continue
		}
		if r.queueWait[i] <= 0 {
			t.Fatalf("%s: worker %d parked but QueueWait=%v (queue timeout failover?)", what, i, r.queueWait[i])
		}
		if r.assign[i] != r.parkQueue[i] {
			t.Fatalf("%s: worker %d parked on token %d but granted on %d (queue timeout failover?)",
				what, i, r.parkQueue[i], r.assign[i])
		}
	}
}

// assertLadFIFOOrder pins grant order == arrival order over the parked
// subsequence (immediate grants trivially precede parks).
func assertLadFIFOOrder(t *testing.T, r ladWaveResult, what string) {
	t.Helper()
	var parkedArrival, parkedGrant []int
	for i := range r.assign {
		if r.parked[i] {
			parkedArrival = append(parkedArrival, i)
		}
	}
	for _, id := range r.grantOrder {
		if r.parked[id] {
			parkedGrant = append(parkedGrant, id)
		}
	}
	if len(parkedArrival) != len(parkedGrant) {
		t.Fatalf("%s: parked arrival %v vs grant %v (count mismatch)", what, parkedArrival, parkedGrant)
	}
	for k := range parkedArrival {
		if parkedArrival[k] != parkedGrant[k] {
			t.Fatalf("%s: FIFO violated: arrival %v vs grant %v", what, parkedArrival, parkedGrant)
		}
	}
}

func ladTurnCounts(assign []int, n int) []int {
	counts := make([]int, n)
	for _, tok := range assign {
		counts[tok]++
	}
	return counts
}

// ladChatRendezvous proves simultaneous live turns at mock level: all leases'
// chats start behind one barrier with mock latency, so every chat window
// overlaps; all must reach upstream and succeed.
func ladChatRendezvous(t *testing.T, p *Pool, mocks []*testutil.MockUpstream, held []*Lease, model string) {
	t.Helper()
	sse := testutil.SSEEvent(`{"id":"chatcmpl-lad","object":"chat.completion.chunk","created":1,"model":"` + model + `","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`)
	for _, m := range mocks {
		m.ChatBody = sse
		m.ChatDelay = 300 * time.Millisecond
	}
	before := 0
	for _, m := range mocks {
		before += m.RequestsSnapshot()
	}
	barrier := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, len(held))
	for _, l := range held {
		wg.Add(1)
		go func(lease *Lease) {
			defer wg.Done()
			<-barrier
			opts := upstream.ChatOptions{Model: model, RunID: lease.Run.RunID, SessionInstanceID: lease.SessionInstanceID}
			body := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"ping"}]}`)
			rc, err := p.Chat(context.Background(), lease, opts, body)
			if err != nil {
				errs <- err
				return
			}
			_ = rc.Close()
			errs <- nil
		}(l)
	}
	close(barrier)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("rendezvous chat: %v (want all %d chats live simultaneously)", err, len(held))
		}
	}
	after := 0
	for _, m := range mocks {
		after += m.RequestsSnapshot()
	}
	if after-before < len(held) {
		t.Fatalf("rendezvous chats reaching upstream = %d, want >= %d", after-before, len(held))
	}
	t.Logf("LADDER rendezvous: %d chats overlapped live, all reached upstream", len(held))
	for _, m := range mocks {
		m.ChatDelay = 0
	}
}

// Stick truth (operator ruling 2026-09-16): same-model drain arrivals rank
// the usable holder HEAD even when slot-full and park FIFO on it — no
// spill, no spread-to-all. The old spill pins (account pairs on even
// indexes, Ceil(C/2) accounts) are deleted; each drain round below pins the
// stick distribution directly: C<=cap fits the holder with zero parks,
// C>cap holds 2 live + (C-2) parked FIFO on account 1, one session-create
// total, zero errors.

func ladEqualInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestConcurrencyLadderBase is the operator's full simulation on 5 accounts:
// ladder C=1..6 x drain | least_used, 12 requests per
// level. Fresh pool per wave (history-free routing); holds to the all-held
// barrier so C turns overlap live; peak measured, then release.
func TestConcurrencyLadderBase(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: pool ladder lane excluded; run `go test ./backend/...` for the full tier")
	}
	ctx := context.Background()

	t.Run("drain", func(t *testing.T) {
		// C=1..2 fit the holder's two slots: sequential solo acquires land
		// on account 1 with zero parks (one session-create per wave).
		for _, c := range []int{1, 2} {
			waves := 12 / c
			for w := range waves {
				p, mocks := newLadderPool(t, 5, "drain", 0)
				held := acquireHeld(t, ctx, p, modelA, c)
				assign := ladAssign(held)
				for _, tok := range assign {
					if tok != 0 {
						t.Fatalf("drain C=%d wave %d assign=%v, want all holder (token 0)", c, w, assign)
					}
				}
				live, _ := ladSlots(p)
				if live[0] != c || ladSum(live) != c {
					t.Fatalf("drain C=%d wave %d live=%v, want holder %d", c, w, live, c)
				}
				for _, l := range held {
					if l.QueueWait != 0 {
						t.Fatalf("drain C=%d wave %d: parked (QueueWait=%v), want zero parks", c, w, l.QueueWait)
					}
				}
				if c == 2 && w == 0 {
					ladChatRendezvous(t, p, mocks, held, modelA)
				}
				releaseHeld(p, held)
				assertLadQuiescent(t, p, "drain")
				if got := mocks[0].SessionCreatesSnapshot(); got != 1 {
					t.Fatalf("drain C=%d wave %d: holder creates = %d, want 1", c, w, got)
				}
				t.Logf("LADDER mode=drain C=%d wave=%d assign=%v live=%v parked=0 errors=0", c, w, assign, live)
			}
			t.Logf("LADDER mode=drain C=%d (12 requests, zero errors, zero parks, all holder)", c)
		}
		// C=3..6 stick to the holder past the cap: 2 live + (C-2) parked
		// FIFO on account 1 (settle discipline admits each park before the
		// next arrival, so nothing waits out QUEUE_WAIT and nothing
		// overflows). Same instance, one session-create, zero spread.
		for c := 3; c <= 6; c++ {
			waves := (12 + c - 1) / c
			for w := range waves {
				p, mocks := newLadderPool(t, 5, "drain", 0)
				r := runLadWave(t, p, modelA, c)
				for _, tok := range r.assign {
					if tok != 0 {
						t.Fatalf("drain C=%d wave %d assign=%v, want all holder (token 0, stick-first, no spill)", c, w, r.assign)
					}
				}
				assertLadCaps(t, r.peak, "drain")
				if r.peak[0] != ladCap {
					t.Fatalf("drain C=%d wave %d peak=%v, want holder at cap", c, w, r.peak)
				}
				for i := 1; i < 5; i++ {
					if r.peak[i] != 0 {
						t.Fatalf("drain C=%d wave %d peak=%v, want zero spread past account 1", c, w, r.peak)
					}
				}
				parked := 0
				for _, pk := range r.parked {
					if pk {
						parked++
					}
				}
				if parked != c-ladCap {
					t.Fatalf("drain C=%d wave %d parked=%d, want %d (queue on the holder, never spill)", c, w, parked, c-ladCap)
				}
				assertLadNoTimeouts(t, r, "drain")
				assertLadFIFOOrder(t, r, "drain")
				for i, got := range r.inst {
					if got == "" || got != r.inst[0] {
						t.Fatalf("drain C=%d wave %d: worker %d instance %q, want shared %q", c, w, i, got, r.inst[0])
					}
				}
				if got := mocks[0].SessionCreatesSnapshot(); got != 1 {
					t.Fatalf("drain C=%d wave %d: holder creates = %d, want 1 (one entitlement serves all)", c, w, got)
				}
				for i := 1; i < 5; i++ {
					if got := mocks[i].SessionCreatesSnapshot(); got != 0 {
						t.Fatalf("drain C=%d wave %d: mock%d creates = %d, want 0 (no second entitlement spent)", c, w, i, got)
					}
				}
				t.Logf("LADDER mode=drain C=%d wave=%d assign=%v peak=%v parked=%d errors=0", c, w, r.assign, r.peak, parked)
			}
			t.Logf("LADDER mode=drain C=%d (12+ requests, zero errors, all holder, parked=%d/wave)", c, c-ladCap)
		}
	})

	t.Run("least_used", func(t *testing.T) {
		// C=1..2 pile onto the head account without parking (cap 2).
		for _, c := range []int{1, 2} {
			waves := 12 / c
			for w := range waves {
				p, _ := newLadderPool(t, 5, "least_used", 0)
				held := acquireHeld(t, ctx, p, modelA, c)
				assign := ladAssign(held)
				for _, tok := range assign {
					if tok != 0 {
						t.Fatalf("least_used C=%d wave %d assign=%v, want all head (token 0)", c, w, assign)
					}
				}
				live, _ := ladSlots(p)
				if live[0] != c || ladSum(live) != c {
					t.Fatalf("least_used C=%d wave %d live=%v, want head %d", c, w, live, c)
				}
				releaseHeld(p, held)
				assertLadQuiescent(t, p, "least_used")
				t.Logf("LADDER mode=least_used C=%d wave=%d assign=%v live=%v parked=0 errors=0", c, w, assign, live)
			}
		}
		// C>=3 queues FIFO on the head account past cap 2 (no spread in
		// step 1); every parked turn is granted on release — zero errors.
		for c := 3; c <= 6; c++ {
			waves := (12 + c - 1) / c
			for w := range waves {
				p, _ := newLadderPool(t, 5, "least_used", 0)
				r := runLadWave(t, p, modelA, c)
				for _, tok := range r.assign {
					if tok != 0 {
						t.Fatalf("least_used C=%d wave %d assign=%v, want all head (token 0)", c, w, r.assign)
					}
				}
				assertLadCaps(t, r.peak, "least_used")
				if r.peak[0] != ladCap {
					t.Fatalf("least_used C=%d wave %d peak=%v, want head at cap", c, w, r.peak)
				}
				parked := 0
				for _, pk := range r.parked {
					if pk {
						parked++
					}
				}
				if parked != c-ladCap {
					t.Fatalf("least_used C=%d wave %d parked=%d, want %d", c, w, parked, c-ladCap)
				}
				assertLadNoTimeouts(t, r, "least_used")
				assertLadFIFOOrder(t, r, "least_used")
				t.Logf("LADDER mode=least_used C=%d wave=%d assign=%v peak=%v parked=%d errors=0", c, w, r.assign, r.peak, parked)
			}
		}
	})
}

// TestConcurrencyLadderScale is round A: 10-account pool, full ladder 1..10
// x drain | least_used | random, one wave per level. Drain sticks: every
// level lands wholly on account 1
// (stick-first, no spill) — C<=cap with zero parks, C>cap with 2 live +
// (C-2) parked FIFO.
func TestConcurrencyLadderScale(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: pool ladder lane excluded; run `go test ./backend/...` for the full tier")
	}
	ctx := context.Background()

	t.Run("drain", func(t *testing.T) {
		for c := 1; c <= 10; c++ {
			p, mocks := newLadderPool(t, 10, "drain", 0)
			if c <= 2 {
				held := acquireHeld(t, ctx, p, modelA, c)
				assign := ladAssign(held)
				for _, tok := range assign {
					if tok != 0 {
						t.Fatalf("scale drain C=%d assign=%v, want all holder (token 0)", c, assign)
					}
				}
				live, _ := ladSlots(p)
				if live[0] != c || ladSum(live) != c {
					t.Fatalf("scale drain C=%d live=%v, want holder %d", c, live, c)
				}
				for _, l := range held {
					if l.QueueWait != 0 {
						t.Fatalf("scale drain C=%d: parked, want zero parks", c)
					}
				}
				releaseHeld(p, held)
				assertLadQuiescent(t, p, "scale drain")
				if got := mocks[0].SessionCreatesSnapshot(); got != 1 {
					t.Fatalf("scale drain C=%d: holder creates = %d, want 1", c, got)
				}
				t.Logf("LADDER scale mode=drain C=%d assign=%v live=%v parked=0 errors=0", c, assign, live)
				continue
			}
			r := runLadWave(t, p, modelA, c)
			for _, tok := range r.assign {
				if tok != 0 {
					t.Fatalf("scale drain C=%d assign=%v, want all holder (token 0, stick-first, no spill)", c, r.assign)
				}
			}
			assertLadCaps(t, r.peak, "scale drain")
			if r.peak[0] != ladCap {
				t.Fatalf("scale drain C=%d peak=%v, want holder at cap", c, r.peak)
			}
			for i := 1; i < 10; i++ {
				if r.peak[i] != 0 {
					t.Fatalf("scale drain C=%d peak=%v, want zero spread past account 1", c, r.peak)
				}
			}
			parked := 0
			for _, pk := range r.parked {
				if pk {
					parked++
				}
			}
			if parked != c-ladCap {
				t.Fatalf("scale drain C=%d parked=%d, want %d (queue on the holder, never spill)", c, parked, c-ladCap)
			}
			assertLadNoTimeouts(t, r, "scale drain")
			assertLadFIFOOrder(t, r, "scale drain")
			if got := mocks[0].SessionCreatesSnapshot(); got != 1 {
				t.Fatalf("scale drain C=%d: holder creates = %d, want 1 (one entitlement serves all)", c, got)
			}
			t.Logf("LADDER scale mode=drain C=%d assign=%v peak=%v parked=%d errors=0", c, r.assign, r.peak, parked)
		}
	})

	t.Run("least_used", func(t *testing.T) {
		for c := 1; c <= 10; c++ {
			pool10, _ := newLadderPool(t, 10, "least_used", 0)
			if c <= 2 {
				held := acquireHeld(t, ctx, pool10, modelA, c)
				for _, tok := range ladAssign(held) {
					if tok != 0 {
						t.Fatalf("scale least_used C=%d: spread to %d, want head only", c, tok)
					}
				}
				releaseHeld(pool10, held)
				assertLadQuiescent(t, pool10, "scale least_used")
				t.Logf("LADDER scale mode=least_used C=%d assign=%v parked=0 errors=0", c, ladAssign(held))
				continue
			}
			r := runLadWave(t, pool10, modelA, c)
			for _, tok := range r.assign {
				if tok != 0 {
					t.Fatalf("scale least_used C=%d assign=%v, want all head", c, r.assign)
				}
			}
			assertLadCaps(t, r.peak, "scale least_used")
			parked := 0
			for _, pk := range r.parked {
				if pk {
					parked++
				}
			}
			if parked != c-ladCap {
				t.Fatalf("scale least_used C=%d parked=%d, want %d", c, parked, c-ladCap)
			}
			assertLadNoTimeouts(t, r, "scale least_used")
			assertLadFIFOOrder(t, r, "scale least_used")
			t.Logf("LADDER scale mode=least_used C=%d peak=%v parked=%d errors=0", c, r.peak, parked)
		}
	})

	t.Run("random", func(t *testing.T) {
		for c := 1; c <= 10; c++ {
			p, _ := newLadderPool(t, 10, "random", 0)
			r := runLadWave(t, p, modelA, c)
			assertLadCaps(t, r.peak, "scale random")
			assertLadNoTimeouts(t, r, "scale random")
			t.Logf("LADDER scale mode=random C=%d assign=%v peak=%v errors=0", c, r.assign, r.peak)
		}
	})
}

// TestConcurrencyLadderQueueFull is round B: 10-account pool, 25 staggered
// same-model arrivals (QUEUE_WAIT generous at 5m, QUEUE_DEPTH at the repo
// default 16 — the operator's 1024 is the Drain preset, not the Load
// default; max observed depth is recorded). Stick-first: all 25 land on
// account 1 (2 live + 23 parked FIFO on the holder lane, settled before any
// QUEUE_WAIT elapses so nothing overflows). All succeed on the one shared
// session; peak live <= 2 on the holder and 0 elsewhere; zero errors and
// zero queue-timeouts (every parked turn is queue-granted, never
// timeout-failed-over).
func TestConcurrencyLadderQueueFull(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: pool ladder lane excluded; run `go test ./backend/...` for the full tier")
	}
	p, mocks := newLadderPool(t, 10, "drain", 5*time.Minute)
	r := runLadWave(t, p, modelA, 25)
	assertLadCaps(t, r.peak, "queue-full")
	for _, tok := range r.assign {
		if tok != 0 {
			t.Fatalf("queue-full: worker on token %d, want all holder (token 0, stick-first, no spill)", tok)
		}
	}
	if r.peak[0] != ladCap {
		t.Fatalf("queue-full: holder peak live = %d, want %d", r.peak[0], ladCap)
	}
	for i := 1; i < 10; i++ {
		if r.peak[i] != 0 {
			t.Fatalf("queue-full: token %d peak live = %d, want 0 (zero spread)", i, r.peak[i])
		}
	}
	parked := 0
	for _, pk := range r.parked {
		if pk {
			parked++
		}
	}
	if parked != 23 {
		t.Fatalf("queue-full: parked = %d, want 23 (25 arrivals - 2 holder slots)", parked)
	}
	assertLadNoTimeouts(t, r, "queue-full")
	assertLadFIFOOrder(t, r, "queue-full")
	for i, got := range r.inst {
		if got == "" || got != r.inst[0] {
			t.Fatalf("queue-full: worker %d instance %q, want shared %q", i, got, r.inst[0])
		}
	}
	if got := mocks[0].SessionCreatesSnapshot(); got != 1 {
		t.Fatalf("queue-full: holder creates = %d, want 1 (one entitlement serves all 25)", got)
	}
	maxDepth, maxDepthTok := 0, -1
	for i, q := range r.peakQueued {
		if q > maxDepth {
			maxDepth, maxDepthTok = q, i
		}
	}
	// Settle discipline admits each parked turn before the next arrival, so
	// at most one waiter is ever queued at once (peak total 1) while 23 park
	// in total — depth headroom 1 << 16, nothing near refusal.
	if got := ladSum(r.peakQueued); got != 1 {
		t.Fatalf("queue-full: peak queued total = %d, want 1 (settle admits each park before the next arrival)", got)
	}
	t.Logf("LADDER queue-full: 25 arrivals all holder, peak live=%v (holder 2), ever-parked=23, max observed queue depth=%d (token %d, headroom to 16), grant order == arrival order, zero errors, zero timeouts", r.peak, maxDepth, maxDepthTok)
}

// ladQuarantineViaBan quarantines target through the repo's real ban path
// (locks-pinned live 403 with resumes_at): no test double around it.
func ladQuarantineViaBan(t *testing.T, ctx context.Context, p *Pool, mocks []*testutil.MockUpstream, target int, model string) {
	t.Helper()
	other := modelA
	if model == modelA {
		other = modelB
	}
	cfg := p.cfg.Load()
	next := *cfg
	next.PinModel = map[int]string{}
	for i := range mocks {
		if i != target {
			next.PinModel[i] = other
		}
	}
	p.SetConfig(&next)
	mocks[target].Ban = true
	_, err := p.Acquire(ctx, model)
	mocks[target].Ban = false
	if err == nil {
		t.Fatalf("ban trip token %d: acquire succeeded, want refusal", target)
	}
	snap := p.Snapshot()[target]
	if !snap.Quarantined {
		t.Fatalf("ban trip token %d: not quarantined (err=%v)", target, err)
	}
	t.Logf("LADDER ban trip: token %d quarantined (%s)", target, snap.QuarantineReason)
	cfg2 := p.cfg.Load()
	next2 := *cfg2
	next2.PinModel = nil
	p.SetConfig(&next2)
}

// TestConcurrencyLadderBanned is round D: 5-account pool, account 5
// quarantined via the real ban path (ban-quarantine is kept under MASQ).
// Strict index order: the quarantined lane is filtered from the spill order,
// so healthy same-model arrivals land wholly on account 1 — C<=cap with
// zero parks, C>cap with 2 live + (C-2) parked FIFO. Banned takes ZERO
// turns; all succeed.
func TestConcurrencyLadderBanned(t *testing.T) {
	ctx := context.Background()
	for _, c := range []int{1, 2, 3, 4, 5, 6, 8} {
		p, mocks := newLadderPool(t, 5, "drain", 500*time.Millisecond)
		ladQuarantineViaBan(t, ctx, p, mocks, 4, modelA)
		createsBefore := mocks[0].SessionCreatesSnapshot()
		if c <= 2 {
			held := acquireHeld(t, ctx, p, modelA, c)
			assign := ladAssign(held)
			for _, tok := range assign {
				if tok != 0 {
					t.Fatalf("banned C=%d assign=%v, want all holder (token 0)", c, assign)
				}
			}
			for _, l := range held {
				if l.QueueWait != 0 {
					t.Fatalf("banned C=%d: parked, want zero parks (holder slots free)", c)
				}
			}
			releaseHeld(p, held)
			assertLadQuiescent(t, p, "banned")
			t.Logf("LADDER banned C=%d assign=%v banned-turns=0 parked=0 errors=0", c, assign)
		} else {
			r := runLadWave(t, p, modelA, c)
			counts := ladTurnCounts(r.assign, 5)
			if counts[4] != 0 {
				t.Fatalf("banned C=%d turns=%v, want banned account dark", c, counts)
			}
			for _, tok := range r.assign {
				if tok != 0 {
					t.Fatalf("banned C=%d assign=%v, want all holder (token 0, strict order, banned disqualified)", c, r.assign)
				}
			}
			assertLadCaps(t, r.peak, "banned")
			if r.peak[0] != ladCap {
				t.Fatalf("banned C=%d peak=%v, want holder at cap", c, r.peak)
			}
			parked := 0
			for _, pk := range r.parked {
				if pk {
					parked++
				}
			}
			if parked != c-ladCap {
				t.Fatalf("banned C=%d parked=%d, want %d (queue on the holder, never spill)", c, parked, c-ladCap)
			}
			assertLadNoTimeouts(t, r, "banned")
			assertLadFIFOOrder(t, r, "banned")
			t.Logf("LADDER banned C=%d assign=%v peak=%v banned-turns=0 parked=%d errors=0", c, r.assign, r.peak, parked)
		}
		if got := mocks[0].SessionCreatesSnapshot() - createsBefore; got != 1 {
			t.Fatalf("banned C=%d: holder creates = %d, want 1 (one entitlement serves the wave)", c, got)
		}
	}
	p10, mocks10 := newLadderPool(t, 5, "drain", 500*time.Millisecond)
	ladQuarantineViaBan(t, ctx, p10, mocks10, 4, modelA)
	createsBefore10 := mocks10[0].SessionCreatesSnapshot()
	r := runLadWave(t, p10, modelA, 10)
	counts := ladTurnCounts(r.assign, 5)
	if counts[4] != 0 {
		t.Fatalf("banned C=10 turns=%v, want banned account dark", counts)
	}
	for _, tok := range r.assign {
		if tok != 0 {
			t.Fatalf("banned C=10 assign=%v, want all holder (token 0, strict order)", r.assign)
		}
	}
	assertLadCaps(t, r.peak, "banned")
	if r.peak[0] != ladCap {
		t.Fatalf("banned C=10 peak=%v, want holder at cap", r.peak)
	}
	parked := 0
	for _, pk := range r.parked {
		if pk {
			parked++
		}
	}
	if parked != 8 {
		t.Fatalf("banned C=10 parked=%d, want 8 (10 arrivals - 2 holder slots)", parked)
	}
	assertLadNoTimeouts(t, r, "banned")
	assertLadFIFOOrder(t, r, "banned")
	if got := mocks10[0].SessionCreatesSnapshot() - createsBefore10; got != 1 {
		t.Fatalf("banned C=10: holder creates = %d, want 1 (one entitlement serves all ten)", got)
	}
	t.Logf("LADDER banned C=10 assign=%v peak=%v banned-turns=0 parked=8 errors=0", r.assign, r.peak)
}

// TestConcurrencyLadderRamp is round E: deterministic ramps (no random
// walk — non-determinism is not evidence). Stick-first: every step lands
// wholly on account 1 — steps fitting the holder's two slots with zero
// parks, larger steps with 2 live + (n-2) parked FIFO (settle discipline,
// so nothing waits out QUEUE_WAIT and nothing overflows). Caps + zero
// errors + quiescence asserted per step.
func TestConcurrencyLadderRamp(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: pool ladder lane excluded; run `go test ./backend/...` for the full tier")
	}
	ramp := func(t *testing.T, p *Pool, model string, steps []int, what string) {
		t.Helper()
		for _, n := range steps {
			if n <= 2 {
				held := acquireHeld(t, context.Background(), p, model, n)
				for _, tok := range ladAssign(held) {
					if tok != 0 {
						t.Fatalf("%s step %d assign=%v, want all holder (token 0)", what, n, ladAssign(held))
					}
				}
				live, _ := ladSlots(p)
				assertLadCaps(t, live, what)
				if got := ladSum(live); got != n {
					t.Fatalf("%s step %d: live sum=%d", what, n, got)
				}
				for _, l := range held {
					if l.QueueWait != 0 {
						t.Fatalf("%s step %d: parked, want zero unexpected parks", what, n)
					}
				}
				t.Logf("LADDER ramp %s step=%d assign=%v live=%v errors=0", what, n, ladAssign(held), live)
				releaseHeld(p, held)
				assertLadQuiescent(t, p, what)
				continue
			}
			r := runLadWave(t, p, model, n)
			for _, tok := range r.assign {
				if tok != 0 {
					t.Fatalf("%s step %d assign=%v, want all holder (token 0, stick-first, no spill)", what, n, r.assign)
				}
			}
			assertLadCaps(t, r.peak, what)
			if r.peak[0] != ladCap {
				t.Fatalf("%s step %d peak=%v, want holder at cap", what, n, r.peak)
			}
			parked := 0
			for _, pk := range r.parked {
				if pk {
					parked++
				}
			}
			if parked != n-ladCap {
				t.Fatalf("%s step %d parked=%d, want %d (queue on the holder, never spill)", what, n, parked, n-ladCap)
			}
			assertLadNoTimeouts(t, r, what)
			assertLadFIFOOrder(t, r, what)
			t.Logf("LADDER ramp %s step=%d assign=%v peak=%v parked=%d errors=0", what, n, r.assign, r.peak, parked)
		}
	}
	p5, _ := newLadderPool(t, 5, "drain", 0)
	ramp(t, p5, modelA, []int{1, 2, 3, 4, 5, 6, 5, 4, 3, 2, 1}, "5-pool")
	p10, _ := newLadderPool(t, 10, "drain", 0)
	ramp(t, p10, modelA, []int{2, 4, 6, 8, 10, 8, 6, 4, 2}, "10-pool")
}

// TestConcurrencyLadderSingleFIFO is round F (the operator's "load balance
// disabled" question): 1-account pool, drain, generous QUEUE_WAIT, 5
// staggered arrivals. Exactly 2 live + 3 ever-parked on the single lane;
// admission order == arrival order (perfect FIFO); all 5 succeed on the one
// shared session; peak live <= 2. The order trace below is the headline
// answer to "2 streaming + 3 queue".
func TestConcurrencyLadderSingleFIFO(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: pool ladder lane excluded; run `go test ./backend/...` for the full tier")
	}
	p, mocks := newLadderPool(t, 1, "drain", 5*time.Minute)
	r := runLadWave(t, p, modelA, 5)
	for i, tok := range r.assign {
		if tok != 0 {
			t.Fatalf("single FIFO: worker %d on token %d, want the single account", i, tok)
		}
	}
	if r.peak[0] != 2 {
		t.Fatalf("single FIFO: peak live = %d, want exactly 2", r.peak[0])
	}
	if r.peakQueued[0] != 1 {
		t.Fatalf("single FIFO: peak queued = %d, want 1 (settle admits each park before the next arrival)", r.peakQueued[0])
	}
	parked := 0
	for _, pk := range r.parked {
		if pk {
			parked++
		}
	}
	if parked != 3 {
		t.Fatalf("single FIFO: ever-parked = %d, want 3 (5 arrivals - 2 slots)", parked)
	}
	wantOrder := []int{0, 1, 2, 3, 4}
	if !ladEqualInts(r.grantOrder, wantOrder) {
		t.Fatalf("single FIFO: grant order = %v, want arrival order %v (perfect FIFO)", r.grantOrder, wantOrder)
	}
	assertLadNoTimeouts(t, r, "single FIFO")
	for i, inst := range r.inst {
		if inst == "" || inst != r.inst[0] {
			t.Fatalf("single FIFO: worker %d instance %q, want shared %q", i, inst, r.inst[0])
		}
	}
	if got := mocks[0].SessionCreatesSnapshot(); got != 1 {
		t.Fatalf("single FIFO: session creates = %d, want 1 (one session serves all five)", got)
	}
	t.Logf("LADDER single-FIFO TRACE arrival=[0 1 2 3 4] live-grants=[0 1] parked=[2 3 4] grant-order=[0 1 2 3 4] instance=%s creates=1 errors=0", r.inst[0])
}

// ladAffinitySetup establishes the M-session on account 1 (token 0):
// acquire + release leaves the session cached usable. Returns its instance.
func ladAffinitySetup(t *testing.T, ctx context.Context, p *Pool, model string) string {
	t.Helper()
	lease, err := p.Acquire(ctx, model)
	if err != nil {
		t.Fatalf("affinity setup: %v", err)
	}
	if lease.Token != 0 {
		t.Fatalf("affinity setup token = %d, want account 1 (token 0)", lease.Token)
	}
	inst := lease.SessionInstanceID
	if inst == "" {
		t.Fatal("affinity setup yielded an empty session instance id")
	}
	p.LeaseRelease(lease)
	assertLadQuiescent(t, p, "affinity setup")
	return inst
}

// TestAffinitySequential is round G: with a usable M-session on account 1,
// 10 sequential M-requests must ALL land on account 1 (same instance, one
// session-create total) — serving from the live session beats spending a
// second entitlement elsewhere. Accounts 2-5 take 0 turns.
func TestAffinitySequential(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: pool ladder lane excluded; run `go test ./backend/...` for the full tier")
	}
	ctx := context.Background()
	p, mocks := newLadderPool(t, 5, "drain", 0)
	inst := ladAffinitySetup(t, ctx, p, modelA)
	for i := range 10 {
		lease, err := p.Acquire(ctx, modelA)
		if err != nil {
			t.Fatalf("affinity sequential %d: %v", i, err)
		}
		if lease.Token != 0 {
			// GAP tripwire: same-model spread off a usable session (see the
			// gap-fix authorization in the task brief). Red is evidence —
			// do not force green.
			t.Fatalf("affinity sequential %d: spread to token %d despite a usable M-session on account 1 (gap!)", i, lease.Token)
		}
		if lease.SessionInstanceID != inst {
			t.Fatalf("affinity sequential %d: instance %q, want the held %q", i, lease.SessionInstanceID, inst)
		}
		p.LeaseRelease(lease)
	}
	assertLadQuiescent(t, p, "affinity sequential")
	for i, m := range mocks {
		want := 0
		if i == 0 {
			want = 1
		}
		if got := m.SessionCreatesSnapshot(); got != want {
			t.Fatalf("affinity sequential: mock%d creates = %d, want %d", i, got, want)
		}
	}
	t.Logf("AFFIN sequential: 10/10 on account 1, instance=%s, creates=1, spread=0, errors=0", inst)
}

// TestAffinityConcurrent is round H: same setup, 5 staggered M-requests. ALL
// land on account 1 (2 live + 3 queued FIFO on that lane) — queuing on the
// session-holder beats spending a second entitlement, even though another
// account would admit "faster". Zero spread, order preserved, zero errors.
func TestAffinityConcurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: pool ladder lane excluded; run `go test ./backend/...` for the full tier")
	}
	// Stick-first (operator ruling 2026-09-16): same-model arrivals rank the
	// usable holder HEAD even when slot-full and park FIFO on it — no spill.
	// History: #593 shipped this test with a t.Skip citing the irreconcilable
	// spill-vs-queue conflict ("same-model overflow must SPILL per the
	// ladder's own drain C=3 [0 0 2] zero-park pins but must QUEUE per this
	// pin"). This PR resolves it operator-side (stick, not spill) and
	// re-pins those drain pins to the stick truth.
	ctx := context.Background()
	p, mocks := newLadderPool(t, 5, "drain", 0)
	inst := ladAffinitySetup(t, ctx, p, modelA)
	r := runLadWave(t, p, modelA, 5)
	for i, tok := range r.assign {
		if tok != 0 {
			t.Fatalf("affinity concurrent: worker %d spread to token %d despite a usable M-session on account 1 (gap!)", i, tok)
		}
	}
	if r.peak[0] != 2 {
		t.Fatalf("affinity concurrent: peak live = %d, want 2 on the holder", r.peak[0])
	}
	for i := 1; i < 5; i++ {
		if r.peak[i] != 0 {
			t.Fatalf("affinity concurrent: token %d peak live = %d, want 0 (zero spread)", i, r.peak[i])
		}
	}
	parked := 0
	for _, pk := range r.parked {
		if pk {
			parked++
		}
	}
	if parked != 3 {
		t.Fatalf("affinity concurrent: parked = %d, want 3 (queue on the holder, never spill)", parked)
	}
	assertLadNoTimeouts(t, r, "affinity concurrent")
	assertLadFIFOOrder(t, r, "affinity concurrent")
	for i, got := range r.inst {
		if got != inst {
			t.Fatalf("affinity concurrent: worker %d instance %q, want the held %q", i, got, inst)
		}
	}
	for i := 1; i < 5; i++ {
		if got := mocks[i].SessionCreatesSnapshot(); got != 0 {
			t.Fatalf("affinity concurrent: mock%d creates = %d, want 0 (no second entitlement spent)", i, got)
		}
	}
	t.Logf("AFFIN concurrent: 5/5 on account 1 (2 live + 3 queued FIFO), instance=%s, creates=%d, spread=0, errors=0",
		inst, mocks[0].SessionCreatesSnapshot())
}

// TestAffinityOverflowFailClosed proves the spill chain ends closed: with no
// next lane (single-account pool — every other lane absent), a waiter that
// outlasts QUEUE_WAIT on its holder surfaces the EXISTING queue-timeout
// error unchanged (same 429 shape, queue-wait wording, Retry-After hint),
// never a new error code and never a silent drop.
func TestAffinityOverflowFailClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: pool ladder lane excluded; run `go test ./backend/...` for the full tier")
	}
	ctx := context.Background()
	p, _ := newLadderPool(t, 1, "drain", 2*time.Second)
	ladAffinitySetup(t, ctx, p, modelA)
	// Fill the lone holder to its cap; the waiter below has nowhere to
	// spill to.
	held := acquireHeld(t, ctx, p, modelA, ladCap)
	type outcome struct {
		lease *Lease
		err   error
	}
	waiterCh := make(chan outcome, 1)
	go func() {
		l, err := p.Acquire(context.Background(), modelA)
		waiterCh <- outcome{lease: l, err: err}
	}()
	eventually(t, "waiter parks on the lone holder", func() bool {
		_, queued := ladSlots(p)
		return ladSum(queued) == 1
	})
	select {
	case o := <-waiterCh:
		if o.err == nil {
			p.LeaseRelease(o.lease)
			t.Fatal("fail-closed waiter granted, want the queue-timeout 429 (no helper exists)")
		}
		var rle *upstream.RateLimitError
		if !errors.As(o.err, &rle) {
			t.Fatalf("fail-closed err = %T (%v), want *upstream.RateLimitError unchanged", o.err, o.err)
		}
		if !strings.Contains(rle.Body, "queue wait") {
			t.Fatalf("fail-closed body = %q, want the existing queue-wait wording", rle.Body)
		}
		if rle.RetryAfter <= 0 {
			t.Fatal("fail-closed 429 carries no Retry-After hint")
		}
	case <-time.After(ladStepTO):
		t.Fatal("fail-closed waiter never returned (want the queue-timeout 429)")
	}
	releaseHeld(p, held)
	assertLadQuiescent(t, p, "overflow fail-closed")
	t.Logf("AFFIN overflow fail-closed: lone-holder timeout surfaced the existing queue-wait 429, errors=1 (the waiter)")
}

// TestAffinityExpired is round I: after account 1's M-session ends, 5
// staggered M-requests must create exactly ONE replacement session
// (single-flight, no double-create storm); all 5 served on it, zero errors.
// The landing index is reported, not pinned.
func TestAffinityExpired(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: pool ladder lane excluded; run `go test ./backend/...` for the full tier")
	}
	ctx := context.Background()
	p, mocks := newLadderPool(t, 5, "drain", 0)
	oldInst := ladAffinitySetup(t, ctx, p, modelA)
	(*p.roster.Load())[0].session.Invalidate()
	// The mock serves a fixed instance id per mock: stamp a fresh one so the
	// replacement session is distinguishable from the expired setup session.
	for _, m := range mocks {
		m.InstanceID = "inst-fresh"
	}
	createsBefore := make([]int, 5)
	for i, m := range mocks {
		createsBefore[i] = m.SessionCreatesSnapshot()
	}
	// Burst discipline (deliberate): all five arrive simultaneously, so every
	// path — gate follower or late leader — funnels onto the single admitting
	// token (followers try only the leader's token; late leaders pin the
	// in-flight admission first). Staggered arrivals would solo-lead onto
	// free tokens one by one and never test the admission storm. Landing
	// index depends on which worker leads first, so equality is pinned, not
	// the index.
	const n = 5
	start := make(chan struct{})
	resCh := make([]chan ladWorkerRes, n)
	relAll := make(chan struct{})
	doneCh := make([]chan struct{}, n)
	for i := range n {
		resCh[i] = make(chan ladWorkerRes, 1)
		doneCh[i] = make(chan struct{}, 1)
		go func(id int) {
			<-start
			l, err := p.Acquire(context.Background(), modelA)
			resCh[id] <- ladWorkerRes{lease: l, err: err}
			if err == nil {
				<-relAll
				p.LeaseRelease(l)
			}
			doneCh[id] <- struct{}{}
		}(i)
	}
	close(start)
	// True simultaneous peak: 2 live + 3 queued behind one lane (no releases
	// yet, so nothing settles early).
	eventually(t, "burst parks 2 live + 3 queued", func() bool {
		live, queued := ladSlots(p)
		return ladSum(live) == 2 && ladSum(queued) == 3
	})
	live, queued := ladSlots(p)
	t.Logf("AFFIN expired burst peak: live=%v queued=%v (simultaneous, pre-release)", live, queued)
	close(relAll)
	assign := make([]int, n)
	inst := make([]string, n)
	for i := range n {
		select {
		case wr := <-resCh[i]:
			if wr.err != nil {
				t.Fatalf("affinity expired: worker %d: %v (want all five served)", i, wr.err)
			}
			assign[i] = wr.lease.Token
			inst[i] = wr.lease.SessionInstanceID
		case <-time.After(ladStepTO):
			t.Fatalf("affinity expired: worker %d never granted", i)
		}
	}
	for i := range n {
		select {
		case <-doneCh[i]:
		case <-time.After(ladStepTO):
			t.Fatalf("affinity expired: worker %d never finished", i)
		}
	}
	assertLadQuiescent(t, p, "affinity expired")
	landing := assign[0]
	for _, tok := range assign {
		if tok != landing {
			t.Fatalf("affinity expired: split %v, want all five on the single replacement", assign)
		}
	}
	newCreates := 0
	for i, m := range mocks {
		newCreates += m.SessionCreatesSnapshot() - createsBefore[i]
	}
	if newCreates != 1 {
		t.Fatalf("affinity expired: %d replacement creates, want exactly 1 (single-flight, no double-create storm)", newCreates)
	}
	for i, got := range inst {
		if got == "" || got == oldInst {
			t.Fatalf("affinity expired: worker %d instance %q, want the fresh replacement (old %q)", i, got, oldInst)
		}
		if got != inst[0] {
			t.Fatalf("affinity expired: worker %d instance %q, want shared %q", i, got, inst[0])
		}
	}
	t.Logf("AFFIN expired: replacement landed on account %d, 5/5 served on instance %s, replacement creates=1, errors=0", landing+1, inst[0])
}

// TestConcurrencyLadderAdmissionStorm is round I: six concurrent cold
// arrivals over a 5-account pool with a short wait and NO harness
// warm-up — every admission must come from the wave's own scale-out.
// The storm converges to 2/2/2 live turns on accounts 1-3 with exactly
// one session create per serving lane (concurrent cold admissions
// collapse via the session manager's single-flight), accounts 4-5 see
// zero contact, the two lane-#1 leases grant instantly (QueueWait==0 —
// work-conserving: free slots admit without parking), and the other four
// park behind full lane #1 before scaling out. No arrival order is pinned
// — the counts hold under any interleaving.
func TestConcurrencyLadderAdmissionStorm(t *testing.T) {
	p, mocks := newLadderPool(t, 5, "drain", 400*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const c = 6
	type result struct {
		lease *Lease
		err   error
	}
	resCh := make(chan result, c)
	var wg sync.WaitGroup
	for range c {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := p.Acquire(ctx, modelA)
			resCh <- result{l, err}
		}()
	}
	wg.Wait()
	close(resCh)
	var held []*Lease
	counts := make([]int, 5)
	for r := range resCh {
		if r.err != nil {
			t.Fatalf("storm acquire: %v (want zero errors)", r.err)
		}
		held = append(held, r.lease)
		counts[r.lease.Token]++
		if r.lease.Token == 0 {
			if r.lease.QueueWait != 0 {
				t.Errorf("storm lease on token 0 QueueWait=%v, want 0 (lane #1 slots admit instantly)", r.lease.QueueWait)
			}
		} else if r.lease.QueueWait <= 0 {
			t.Errorf("storm lease on token %d QueueWait=%v, want >0 (overflow parks, then scales out)", r.lease.Token, r.lease.QueueWait)
		}
	}
	if counts[0] != 2 || counts[1] != 2 || counts[2] != 2 || counts[3] != 0 || counts[4] != 0 {
		t.Fatalf("storm split = %v, want [2 2 2 0 0] (scale-out fills in index order)", counts)
	}
	for i := range 3 {
		if got := mocks[i].SessionCreatesSnapshot(); got != 1 {
			t.Errorf("account #%d creates = %d, want 1 (single-flight, one session shared)", i+1, got)
		}
	}
	for i := 3; i < 5; i++ {
		if got := mocks[i].SessionCreatesSnapshot(); got != 0 {
			t.Errorf("account #%d creates = %d, want 0 (storm never reaches it)", i+1, got)
		}
	}
	live, queued := ladSlots(p)
	if ladSum(live) != c || ladSum(queued) != 0 {
		t.Fatalf("storm live/queued = %v/%v, want 6 total live and an empty queue", live, queued)
	}
	releaseHeld(p, held)
	assertLadQuiescent(t, p, "admission-storm")
	t.Logf("LADDER admission-storm: split=%v creates=1/1/1/0/0 lane-#1 instant, overflow parked errors=0", counts)
}
