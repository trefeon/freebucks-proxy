package pool

// concurrency_ladder_test.go — hermetic concurrency-ladder simulation of the
// pool (the operator's "full simulation").
//
// Goal: prove per-account FIFO distribution and zero errors across
// concurrency levels for all 4 rotation modes — with NO real tokens, NO
// upstream traffic, NO freebucks spend (testutil mocks only).
//
// Hermetic boundary (stated, not relitigated): no account tokens were
// supplied, so live fire is impossible without inventing credentials. The
// pool-side logic under test (slot caps, FIFO parking, rotation order, lease
// release) is fully exercised by mocks; upstream admission/entitlement is NOT
// exercised — a live-confirmation rung needs an owner device-login (blocked,
// not skipped).
//
// Matrix (5-account pool unless noted; TOKEN_MAX_CONCURRENT=2,
// ROUTING_SMART=true, QUEUE_WAIT/QUEUE_DEPTH at the documented Load defaults
// 30s/16, RATE_LIMIT_FAILOVER=true; rotation set per round, live-apply via
// SetConfig — no pool rebuild):
//
//	Base:  ladder C=1..6 x drain | round_robin | least_used | random
//	A:      10-account pool, ladder C=1..10 x 4 modes
//	B:      10-account pool, 25 concurrent (20 live + 5 parked FIFO)
//	C:      5-account pool, 2 tripped to window cooldown; ladder 1..6 + C=8
//	D:      5-account pool, 1 quarantined; ladder 1..6 + C=8 + C=10
//	E:      RAMP 5-pool 1-2-3-4-5-6-5-4-3-2-1, 10-pool 2-4-6-8-10-8-6-4-2
//	F:      1-account pool, 5 arrivals, 2 live + 3 parked perfect FIFO
//	G..J:   affinity rounds (drain, 5-pool) — session stickiness
//
// Arrival discipline (deliberate): sequential starts — each worker's Acquire
// begins only after the previous worker granted or provably parked. A burst
// start would collapse same-model contenders onto the leader's token via the
// per-model election gate (followers try ONLY the leader's token,
// acquire_route.go follower branch), making the distribution a scheduling
// artifact instead of a routing measurement. Overlap is still real: granted
// leases are HELD until the round's peak is measured (all-granted barrier,
// channel-gated — never slept), so C turns are live simultaneously by the
// pool's own live-turn definition (routeSlotLive), plus a barrier-gated
// concurrent-chat rendezvous at the mocks in the headline round.
//
// Measured-truth notes (probes, not assumptions):
//
//   - Drain fills account PAIRS on even indexes ([0 0 2 2 4 4]), not
//     0,1,2: the drain cold tier fans out from the round-robin start, and the
//     smart rank sorts slot-full tokens behind free ones while the smooth
//     pick breaks free-way ties by base position. Ceil(C/2) accounts holds.
//   - round_robin spreads strictly by start rotation ([0 1 2 3 4 0]).
//   - least_used piles onto the head account (index order on ties — the mocks
//     serve identical quota) and queues FIFO on it past cap 2 instead of
//     spreading: non-drain strategies keep the legacy base order untouched in
//     step 1 (routeSmartRank early return) and the failover loop parks on the
//     head token's queue. Strategy reduction is step-2 work per the file
//     header. All parked still succeed on release — zero errors.
//   - The #583 live trip (mock 429, body model deepseek-v4-flash) records
//     per-model-exempt cooldown memory (#178 tagging): tripped tokens stay
//     eligible for OTHER models with a -200 backoff. The cooldown ladder
//     therefore trips AND fires on modelB (the body's model), where the
//     tripped accounts go fully dark.
import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

const (
	ladCap   = 2
	ladDepth = 16
	ladWait  = 30 * time.Second

	// ladParkDetect is the park-detection heuristic bound: an instant-mock
	// acquire returns in milliseconds, so silence past this bound with a
	// grown FIFO queue means genuinely parked (verified via routeSlotQueued,
	// never assumed). It is detection only — holds stay channel-gated.
	ladParkDetect = 100 * time.Millisecond
	ladStepTO     = 5 * time.Second
)

// newLadderPool builds a fresh n-account mock pool with the control knobs set
// explicitly and asserted: TOKEN_MAX_CONCURRENT=2, ROUTING_SMART=true,
// QUEUE_WAIT/QUEUE_DEPTH at the documented defaults (overridable for the
// generous-wait queue rounds), RATE_LIMIT_FAILOVER=true.
// setLadderRotation switches rotation live (no pool rebuild) and proves the
// roster survived: same entries, sessions intact (no admission churn).
func setLadderRotation(t *testing.T, p *Pool, mocks []*testutil.MockUpstream, rotation string) {
	t.Helper()
	before := len(*p.roster.Load())
	creates := 0
	for _, m := range mocks {
		creates += m.SessionCreatesSnapshot()
	}
	cfg := p.cfg.Load()
	next := *cfg
	next.TokenRotation = rotation
	p.SetConfig(&next)
	if got := len(*p.roster.Load()); got != before {
		t.Fatalf("SetConfig(%s) rebuilt the roster: %d -> %d tokens", rotation, before, got)
	}
	after := 0
	for _, m := range mocks {
		after += m.SessionCreatesSnapshot()
	}
	if after != creates {
		t.Fatalf("SetConfig(%s) churned sessions: %d -> %d creates", rotation, creates, after)
	}
}

func newLadderPool(t *testing.T, n int, rotation string, wait time.Duration) (*Pool, []*testutil.MockUpstream) {
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
		c.RoutingSmart = true
		c.TokenMaxConcurrent = ladCap
		c.QueueWait = wait
		c.QueueDepth = ladDepth
		c.RateLimitFailover = true
		c.TokenRotation = rotation
	}, mocks...)
	cfg := p.cfg.Load()
	if !cfg.RoutingSmart {
		t.Fatal("ROUTING_SMART = false, want true (control)")
	}
	if cfg.TokenMaxConcurrent != ladCap {
		t.Fatalf("TOKEN_MAX_CONCURRENT = %d, want %d (control)", cfg.TokenMaxConcurrent, ladCap)
	}
	if cfg.QueueDepth != ladDepth {
		t.Fatalf("QUEUE_DEPTH = %d, want %d (control)", cfg.QueueDepth, ladDepth)
	}
	if !cfg.RateLimitFailover {
		t.Fatal("RATE_LIMIT_FAILOVER = false, want true (control)")
	}
	if cap, depth, wt := routeSlotParams(cfg); cap != ladCap || depth != ladDepth || wt != wait {
		t.Fatalf("routeSlotParams = %d/%d/%v, want %d/%d/%v (control)", cap, depth, wt, ladCap, ladDepth, wait)
	}
	if rotation == "random" {
		p.randMu.Lock()
		p.randGen = rand.New(rand.NewPCG(1, 1))
		p.randMu.Unlock()
	}
	return p, mocks
}

// ladSlots snapshots per-token live turns and parked waiters.
func ladSlots(p *Pool) (live, queued []int) {
	toks := p.roster.Load()
	live = make([]int, len(*toks))
	queued = make([]int, len(*toks))
	for i := range *toks {
		live[i] = p.routeSlotLive((*toks)[i])
		queued[i] = p.routeSlotQueued((*toks)[i])
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

// acquireHeld sequentially acquires n leases (solo leaders — deterministic
// routing, no election-gate followers) and holds them: all n turns are live
// simultaneously on return.
func acquireHeld(t *testing.T, ctx context.Context, p *Pool, model string, n int) []*Lease {
	t.Helper()
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
	t.Helper()
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
					t.Fatalf("wave(%d): worker %d parked on token %d with no holder to release", c, i, where)
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

// drainExact pins the measured drain fill: account pairs on even indexes
// (cold tier fans out from the round-robin start; slot-full tokens sort
// behind free ones). The operator hypothesis "accounts 1,2,3" does not hold;
// Ceil(C/2) accounts does. peak mirrors live at the all-held barrier.
var ladDrainAssign = map[int][]int{
	1: {0},
	2: {0, 0},
	3: {0, 0, 2},
	4: {0, 0, 2, 2},
	5: {0, 0, 2, 2, 4},
	6: {0, 0, 2, 2, 4, 4},
}

var ladDrainPeak = map[int][]int{
	1: {1, 0, 0, 0, 0},
	2: {2, 0, 0, 0, 0},
	3: {2, 0, 1, 0, 0},
	4: {2, 0, 2, 0, 0},
	5: {2, 0, 2, 0, 1},
	6: {2, 0, 2, 0, 2},
}

// ladRRLevel pins the round_robin spread: strict start rotation, the doubled
// account at C=6 is token 0.
func ladRRLevel(c int) (assign, peak []int) {
	assign = make([]int, c)
	peak = make([]int, 5)
	for i := range c {
		assign[i] = i % 5
		peak[i%5]++
	}
	return assign, peak
}

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
// ladder C=1..6 x drain | round_robin | least_used | random, 12 requests per
// level. Fresh pool per wave (history-free routing); holds to the all-held
// barrier so C turns overlap live; peak measured, then release.
func TestConcurrencyLadderBase(t *testing.T) {
	ctx := context.Background()

	t.Run("drain", func(t *testing.T) {
		for c := 1; c <= 6; c++ {
			waves := (12 + c - 1) / c
			total := make([]int, 5)
			for w := range waves {
				p, mocks := newLadderPool(t, 5, "drain", 0)
				held := acquireHeld(t, ctx, p, modelA, c)
				assign := ladAssign(held)
				if !ladEqualInts(assign, ladDrainAssign[c]) {
					t.Fatalf("drain C=%d wave %d assign=%v, want %v", c, w, assign, ladDrainAssign[c])
				}
				live, _ := ladSlots(p)
				if !ladEqualInts(live, ladDrainPeak[c]) {
					t.Fatalf("drain C=%d wave %d live=%v, want %v", c, w, live, ladDrainPeak[c])
				}
				for _, l := range held {
					if l.QueueWait != 0 {
						t.Fatalf("drain C=%d wave %d: parked (QueueWait=%v), want zero parks", c, w, l.QueueWait)
					}
				}
				if c == 6 && w == 0 {
					ladChatRendezvous(t, p, mocks, held, modelA)
				}
				releaseHeld(p, held)
				assertLadQuiescent(t, p, "drain")
				for _, tok := range assign {
					total[tok]++
				}
				t.Logf("LADDER mode=drain C=%d wave=%d assign=%v live=%v parked=0 errors=0", c, w, assign, live)
			}
			t.Logf("LADDER mode=drain C=%d total turns=%v (12+ requests, zero errors, zero parks)", c, total)
		}
	})

	t.Run("round_robin", func(t *testing.T) {
		for c := 1; c <= 6; c++ {
			wantAssign, wantPeak := ladRRLevel(c)
			waves := (12 + c - 1) / c
			total := make([]int, 5)
			for w := range waves {
				p, _ := newLadderPool(t, 5, "round_robin", 0)
				held := acquireHeld(t, ctx, p, modelA, c)
				assign := ladAssign(held)
				if !ladEqualInts(assign, wantAssign) {
					t.Fatalf("round_robin C=%d wave %d assign=%v, want %v", c, w, assign, wantAssign)
				}
				live, _ := ladSlots(p)
				if !ladEqualInts(live, wantPeak) {
					t.Fatalf("round_robin C=%d wave %d live=%v, want %v", c, w, live, wantPeak)
				}
				for _, l := range held {
					if l.QueueWait != 0 {
						t.Fatalf("round_robin C=%d wave %d: parked, want zero parks", c, w)
					}
				}
				releaseHeld(p, held)
				assertLadQuiescent(t, p, "round_robin")
				for _, tok := range assign {
					total[tok]++
				}
				t.Logf("LADDER mode=round_robin C=%d wave=%d assign=%v live=%v parked=0 errors=0", c, w, assign, live)
			}
			t.Logf("LADDER mode=round_robin C=%d total turns=%v (zero errors, zero parks)", c, total)
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

	t.Run("random", func(t *testing.T) {
		for c := 1; c <= 6; c++ {
			p, _ := newLadderPool(t, 5, "random", 0)
			waves := (12 + c - 1) / c
			turns := make([]int, 5)
			parkedTotal := 0
			for w := range waves {
				r := runLadWave(t, p, modelA, c)
				assertLadCaps(t, r.peak, "random")
				assertLadNoTimeouts(t, r, "random")
				for _, tok := range r.assign {
					turns[tok]++
				}
				for _, pk := range r.parked {
					if pk {
						parkedTotal++
					}
				}
				t.Logf("LADDER mode=random C=%d wave=%d assign=%v peak=%v errors=0", c, w, r.assign, r.peak)
			}
			for i, n := range turns {
				if n == 0 {
					t.Fatalf("random C=%d: token %d took 0 of %d turns (seeded spread broke)", c, i, ladSum(turns))
				}
			}
			t.Logf("LADDER mode=random C=%d total turns=%v parked=%d errors=0", c, turns, parkedTotal)
		}
	})
}

// TestConcurrencyLadderRotationLiveApply proves rotation is live-apply: the
// same pool routes drain, then round_robin, then drain again with no rebuild
// and no admission churn.
func TestConcurrencyLadderRotationLiveApply(t *testing.T) {
	ctx := context.Background()
	p, mocks := newLadderPool(t, 5, "drain", 0)

	w1 := acquireHeld(t, ctx, p, modelA, 2)
	if a := ladAssign(w1); !ladEqualInts(a, []int{0, 0}) {
		t.Fatalf("drain wave assign=%v, want [0 0]", a)
	}
	releaseHeld(p, w1)

	setLadderRotation(t, p, mocks, "round_robin")
	w2 := acquireHeld(t, ctx, p, modelA, 2)
	if a := ladAssign(w2); !ladEqualInts(a, []int{2, 3}) {
		t.Fatalf("round_robin wave assign=%v, want [2 3] (rr continues, strict rotation)", a)
	}
	releaseHeld(p, w2)

	setLadderRotation(t, p, mocks, "drain")
	w3 := acquireHeld(t, ctx, p, modelA, 2)
	a3 := ladAssign(w3)
	if a3[0] != a3[1] {
		t.Fatalf("drain wave assign=%v, want drain stickiness (same account twice)", a3)
	}
	releaseHeld(p, w3)
	assertLadQuiescent(t, p, "rotation-live-apply")
	t.Logf("LADDER live-apply: drain %v -> round_robin [2 3] -> drain %v, no rebuild, no churn", []int{0, 0}, a3)
}

// ladDrainAssign10 pins the measured 10-account drain fill: pairs on even
// indexes, Ceil(C/2) accounts.
func ladDrainAssign10(c int) []int {
	assign := make([]int, 0, c)
	for tok := 0; len(assign) < c; tok += 2 {
		assign = append(assign, tok, tok)
	}
	return assign[:c]
}

// TestConcurrencyLadderScale is round A: 10-account pool, full ladder 1..10
// x 4 modes, one wave per level (the file runs under -count=2 for the
// statistical modes).
func TestConcurrencyLadderScale(t *testing.T) {
	ctx := context.Background()

	t.Run("drain", func(t *testing.T) {
		for c := 1; c <= 10; c++ {
			p, _ := newLadderPool(t, 10, "drain", 0)
			held := acquireHeld(t, ctx, p, modelA, c)
			assign := ladAssign(held)
			if want := ladDrainAssign10(c); !ladEqualInts(assign, want) {
				t.Fatalf("scale drain C=%d assign=%v, want %v", c, assign, want)
			}
			live, _ := ladSlots(p)
			assertLadCaps(t, live, "scale drain")
			if got := ladSum(live); got != c {
				t.Fatalf("scale drain C=%d live sum=%d, want %d", c, got, c)
			}
			used := 0
			for _, v := range live {
				if v > 0 {
					used++
				}
			}
			if want := (c + 1) / 2; used != want {
				t.Fatalf("scale drain C=%d accounts used=%d, want Ceil(C/2)=%d", c, used, want)
			}
			for _, l := range held {
				if l.QueueWait != 0 {
					t.Fatalf("scale drain C=%d: parked, want zero parks", c)
				}
			}
			releaseHeld(p, held)
			assertLadQuiescent(t, p, "scale drain")
			t.Logf("LADDER scale mode=drain C=%d assign=%v live=%v parked=0 errors=0", c, assign, live)
		}
	})

	t.Run("round_robin", func(t *testing.T) {
		for c := 1; c <= 10; c++ {
			p, _ := newLadderPool(t, 10, "round_robin", 0)
			held := acquireHeld(t, ctx, p, modelA, c)
			assign := ladAssign(held)
			want := make([]int, c)
			for i := range c {
				want[i] = i
			}
			if !ladEqualInts(assign, want) {
				t.Fatalf("scale round_robin C=%d assign=%v, want %v", c, assign, want)
			}
			live, _ := ladSlots(p)
			releaseHeld(p, held)
			assertLadQuiescent(t, p, "scale round_robin")
			t.Logf("LADDER scale mode=round_robin C=%d assign=%v live=%v parked=0 errors=0", c, assign, live)
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
// arrivals against 20 slots (QUEUE_WAIT generous at 5m, QUEUE_DEPTH at the
// repo default 16 — the operator's 1024 is the Drain preset, not the Load
// default; max observed depth is recorded). All 25 succeed; the 5 parked
// admit in exact arrival order; peak live <= 2 everywhere; zero errors and
// zero queue-timeouts (every parked turn is queue-granted, never
// timeout-failed-over).
func TestConcurrencyLadderQueueFull(t *testing.T) {
	p, _ := newLadderPool(t, 10, "drain", 5*time.Minute)
	r := runLadWave(t, p, modelA, 25)
	assertLadCaps(t, r.peak, "queue-full")
	if got := ladSum(r.peak); got != 20 {
		t.Fatalf("queue-full: peak live total = %d, want 20 (full capacity)", got)
	}
	parked := 0
	for _, pk := range r.parked {
		if pk {
			parked++
		}
	}
	if parked != 5 {
		t.Fatalf("queue-full: parked = %d, want 5 (25 arrivals - 20 slots)", parked)
	}
	assertLadNoTimeouts(t, r, "queue-full")
	assertLadFIFOOrder(t, r, "queue-full")
	maxDepth, maxDepthTok := 0, -1
	for i, q := range r.peakQueued {
		if q > maxDepth {
			maxDepth, maxDepthTok = q, i
		}
	}
	// Settle discipline admits each parked turn before the next arrival, so
	// at most one waiter is ever queued at once (peak total 1) while 5 park
	// in total — depth headroom 1 << 16, nothing near refusal.
	if got := ladSum(r.peakQueued); got != 1 {
		t.Fatalf("queue-full: peak queued total = %d, want 1 (settle admits each park before the next arrival)", got)
	}
	t.Logf("LADDER queue-full: 25 arrivals, peak live=%v (total 20), ever-parked=5, max observed queue depth=%d (token %d, headroom to 16), grant order == arrival order, zero errors, zero timeouts", r.peak, maxDepth, maxDepthTok)
}

// ladTripWindow trips target into the vendor 24h freebucks-window cooldown
// via the #583 keeper's live path (locks-pinned healthy admission, run-only
// invalidate, mock 429 with retryAfterMs=71766000), then restores open
// routing. Model is the 429 body's model so the remembered cooldown is
// same-model (no per-model exemption): the account goes fully dark for it.
func ladTripWindow(t *testing.T, ctx context.Context, p *Pool, mocks []*testutil.MockUpstream, target int, model, agent string) string {
	t.Helper()
	other := modelA
	if model == modelA {
		other = modelB
	}
	cfg := p.cfg.Load()
	next := *cfg
	next.ModelLocks = map[int][]string{}
	for i := range mocks {
		if i != target {
			next.ModelLocks[i] = []string{other}
		}
	}
	p.SetConfig(&next)
	lease, err := p.Acquire(ctx, model)
	if err != nil {
		t.Fatalf("trip setup token %d: %v", target, err)
	}
	if lease.Token != target {
		t.Fatalf("trip setup token = %d, want pinned %d", lease.Token, target)
	}
	heldInstance := lease.SessionInstanceID
	p.LeaseRelease(lease)
	p.InvalidateLeaseRun(lease, agent)
	mocks[target].RateLimit = true
	mocks[target].RateLimitRetryAfterMs = 71766000
	_, err = p.Acquire(ctx, model)
	if !errors.Is(err, upstream.ErrRateLimited) {
		t.Fatalf("trip token %d: want ErrRateLimited, got %v", target, err)
	}
	mocks[target].RateLimit = false
	cfg2 := p.cfg.Load()
	next2 := *cfg2
	next2.ModelLocks = nil
	p.SetConfig(&next2)

	entry := (*p.roster.Load())[target]
	rle := entry.runs.RateLimitError()
	if rle == nil {
		t.Fatalf("trip token %d: no remembered rate-limit error", target)
	} else if rle.RetryAfter != 71766*time.Second {
		t.Fatalf("trip token %d: remembered retry_after = %v, want 71766s", target, rle.RetryAfter)
	}
	if until := entry.runs.CooldownUntil(); time.Until(until) < 19*time.Hour {
		t.Fatalf("trip token %d: cooldown window too short: %v", target, time.Until(until))
	}
	snap := entry.session.Snapshot()
	if !snap.Usable() {
		t.Fatalf("trip token %d: session dropped (status %q)", target, snap.Status)
	}
	if snap.InstanceID != heldInstance {
		t.Fatalf("trip token %d: session instance churned (%q -> %q)", target, heldInstance, snap.InstanceID)
	}
	return heldInstance
}

// TestConcurrencyLadderCooldown is round C: 5-account pool, accounts 4-5
// tripped to window cooldown, ladder 1..6 (3 healthy x 2 = 6 slots, no
// queue) plus C=8 (2 park FIFO, all succeed). Cooling accounts take ZERO
// turns; their sessions stay Usable (#585, light touch via SessionEnds ==
// 0 during the ladder); cooldown memory intact; zero caller-visible errors.
func TestConcurrencyLadderCooldown(t *testing.T) {
	ctx := context.Background()
	// C=6 exact on healthy {0,1,2} after the fixed 4-acquire trip recipe.
	want6 := []int{0, 0, 1, 1, 2, 2}
	for _, c := range []int{1, 2, 3, 4, 5, 6, 8} {
		p, mocks := newLadderPool(t, 5, "drain", 0)
		ladTripWindow(t, ctx, p, mocks, 3, modelB, agentB)
		ladTripWindow(t, ctx, p, mocks, 4, modelB, agentB)
		endsBefore := [2]int{mocks[3].SessionEndsSnapshot(), mocks[4].SessionEndsSnapshot()}
		if c <= 6 {
			held := acquireHeld(t, ctx, p, modelB, c)
			assign := ladAssign(held)
			if !ladEqualInts(assign, want6[:c]) {
				t.Fatalf("cooldown C=%d assign=%v, want %v", c, assign, want6[:c])
			}
			for _, l := range held {
				if l.QueueWait != 0 {
					t.Fatalf("cooldown C=%d: parked, want zero parks (6 healthy slots)", c)
				}
			}
			releaseHeld(p, held)
			assertLadQuiescent(t, p, "cooldown")
			t.Logf("LADDER cooldown C=%d assign=%v cooling-turns=0 parked=0 errors=0", c, assign)
		} else {
			r := runLadWave(t, p, modelB, c)
			counts := ladTurnCounts(r.assign, 5)
			if counts[3] != 0 || counts[4] != 0 {
				t.Fatalf("cooldown C=%d turns=%v, want cooling accounts dark", c, counts)
			}
			assertLadCaps(t, r.peak, "cooldown")
			parked := 0
			for _, pk := range r.parked {
				if pk {
					parked++
				}
			}
			if parked != 2 {
				t.Fatalf("cooldown C=%d parked=%d, want 2 (8 arrivals - 6 slots)", c, parked)
			}
			assertLadNoTimeouts(t, r, "cooldown")
			assertLadFIFOOrder(t, r, "cooldown")
			t.Logf("LADDER cooldown C=%d assign=%v peak=%v cooling-turns=0 parked=2 errors=0", c, r.assign, r.peak)
		}
		for k, target := range []int{3, 4} {
			if got := mocks[target].SessionEndsSnapshot(); got != endsBefore[k] {
				t.Fatalf("cooldown C=%d: token %d SessionEnds %d -> %d (surviving session ended, #585)", c, target, endsBefore[k], got)
			}
			entry := (*p.roster.Load())[target]
			if !entry.session.Snapshot().Usable() {
				t.Fatalf("cooldown C=%d: token %d session unusable after ladder", c, target)
			}
			if got := entry.runs.RateLimitError().RetryAfter; got != 71766*time.Second {
				t.Fatalf("cooldown C=%d: token %d retry_after = %v, want intact 71766s", c, target, got)
			}
		}
	}
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
	next.ModelLocks = map[int][]string{}
	for i := range mocks {
		if i != target {
			next.ModelLocks[i] = []string{other}
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
	next2.ModelLocks = nil
	p.SetConfig(&next2)
}

// TestConcurrencyLadderBanned is round D: 5-account pool, account 5
// quarantined via the real ban path; ladder 1..6 + C=8 (4x2 = 8 slots, no
// queue) + C=10 (2 park FIFO). Banned takes ZERO turns; drain
// redistribution exact; all succeed.
func TestConcurrencyLadderBanned(t *testing.T) {
	ctx := context.Background()
	// Post-trip exacts after the fixed 1-acquire ban recipe (rr offset 1).
	want := map[int][]int{
		1: {1},
		2: {1, 1},
		3: {1, 1, 3},
		4: {1, 1, 3, 3},
		5: {1, 1, 3, 3, 0},
		6: {1, 1, 3, 3, 0, 0},
		8: {1, 1, 3, 3, 0, 0, 2, 2},
	}
	for _, c := range []int{1, 2, 3, 4, 5, 6, 8} {
		p, mocks := newLadderPool(t, 5, "drain", 0)
		ladQuarantineViaBan(t, ctx, p, mocks, 4, modelA)
		held := acquireHeld(t, ctx, p, modelA, c)
		assign := ladAssign(held)
		if !ladEqualInts(assign, want[c]) {
			t.Fatalf("banned C=%d assign=%v, want %v", c, assign, want[c])
		}
		for _, l := range held {
			if l.QueueWait != 0 {
				t.Fatalf("banned C=%d: parked, want zero parks (8 healthy slots)", c)
			}
		}
		releaseHeld(p, held)
		assertLadQuiescent(t, p, "banned")
		t.Logf("LADDER banned C=%d assign=%v banned-turns=0 parked=0 errors=0", c, assign)
	}
	p10, mocks10 := newLadderPool(t, 5, "drain", 0)
	ladQuarantineViaBan(t, ctx, p10, mocks10, 4, modelA)
	r := runLadWave(t, p10, modelA, 10)
	counts := ladTurnCounts(r.assign, 5)
	if counts[4] != 0 {
		t.Fatalf("banned C=10 turns=%v, want banned account dark", counts)
	}
	assertLadCaps(t, r.peak, "banned")
	parked := 0
	for _, pk := range r.parked {
		if pk {
			parked++
		}
	}
	if parked != 2 {
		t.Fatalf("banned C=10 parked=%d, want 2 (10 arrivals - 8 slots)", parked)
	}
	assertLadNoTimeouts(t, r, "banned")
	assertLadFIFOOrder(t, r, "banned")
	t.Logf("LADDER banned C=10 assign=%v peak=%v banned-turns=0 parked=2 errors=0", r.assign, r.peak)
}

// TestConcurrencyLadderRamp is round E: deterministic ramps (no random
// walk — non-determinism is not evidence). Every step fits capacity, so zero
// parks are expected; caps + zero errors + quiescence asserted per step.
func TestConcurrencyLadderRamp(t *testing.T) {
	ctx := context.Background()
	ramp := func(t *testing.T, p *Pool, model string, steps []int, what string) {
		t.Helper()
		for _, n := range steps {
			held := acquireHeld(t, ctx, p, model, n)
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
	t.Skip("CONFLICT (operator call pending): same-model overflow must SPILL per the ladder's own acceptance pins (drain C=3 [0 0 2] zero-park) but must QUEUE per this pin — identical pool state (T0 hot+full, same-model arrival), no model-gated fix fits (route_smart.go:517-537, acquire_route.go:279-320). Remove this Skip to re-arm.")
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

// TestAffinityExpired is round I: after account 1's M-session ends, 5
// staggered M-requests must create exactly ONE replacement session
// (single-flight, no double-create storm); all 5 served on it, zero errors.
// The landing index is reported, not pinned.
func TestAffinityExpired(t *testing.T) {
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

// TestAffinityCrossModel is round J (the #178/#132 guardrail made explicit):
// while account 1 holds 2 live M-turns, M2-requests (different model) ARE
// served elsewhere — per-model quota isolation (#178: a quota cap on one
// model never blocks the token's other models) and instance-guarded
// invalidation (#132: a foreign admission must not churn the held session)
// together mean the new model routes around the busy holder while M-turns
// ride undisturbed.
func TestAffinityCrossModel(t *testing.T) {
	ctx := context.Background()
	p, mocks := newLadderPool(t, 5, "drain", 0)
	m1, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatal(err)
	}
	if m1.Token != 0 || m2.Token != 0 {
		t.Fatalf("cross-model setup: M-turns on %d,%d, want both on account 1", m1.Token, m2.Token)
	}
	mInst := m1.SessionInstanceID
	createsBefore := mocks[0].SessionCreatesSnapshot()
	b1, err := p.Acquire(ctx, modelB)
	if err != nil {
		t.Fatalf("cross-model M2 first: %v (bypass broken?)", err)
	}
	if b1.Token == 0 {
		t.Fatal("cross-model M2 first landed on the busy holder (want elsewhere)")
	}
	b2, err := p.Acquire(ctx, modelB)
	if err != nil {
		t.Fatalf("cross-model M2 second: %v", err)
	}
	if b2.Token == 0 {
		t.Fatal("cross-model M2 second landed on the busy holder (want elsewhere)")
	}
	for _, l := range []*Lease{m1, m2, b1, b2} {
		if l.QueueWait != 0 {
			t.Fatal("cross-model: unexpected park (capacity free everywhere)")
		}
	}
	if got := mocks[0].SessionCreatesSnapshot(); got != createsBefore {
		t.Fatalf("cross-model: holder mock creates %d -> %d (M-session churned by the foreign admission, #132)", createsBefore, got)
	}
	entry0 := (*p.roster.Load())[0]
	if snap := entry0.session.Snapshot(); !snap.Usable() || snap.InstanceID != mInst {
		t.Fatalf("cross-model: M-session disturbed (usable=%v instance %q vs %q)", snap.Usable(), snap.InstanceID, mInst)
	}
	t.Logf("AFFIN cross-model: M x2 live on account 1 undisturbed (instance=%s), M2 served on accounts %d,%d, errors=0",
		mInst, b1.Token+1, b2.Token+1)
	p.LeaseRelease(b2)
	p.LeaseRelease(b1)
	p.LeaseRelease(m2)
	p.LeaseRelease(m1)
	assertLadQuiescent(t, p, "cross-model")
}
