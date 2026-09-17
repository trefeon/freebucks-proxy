package pool

// MaturityHistoryEvent is one pool lifecycle fact (ADR-0016) for the
// history store. The sink runs on pool goroutines (maintain tick, admin
// handlers), so it must never block, fail open, or call back into the
// pool: the CLI adapter does a single synchronous SQLite insert. The
// maturity automation is excised (Fase E), so nothing emits yet — the
// boundary stays for the next lifecycle-fact producer.
type MaturityHistoryEvent struct {
	TS       int64
	TokenIdx int
	Kind     string
	Detail   string
}

// HistorySink persists pool lifecycle facts. Implementations must be
// goroutine-safe and must not call back into the pool.
type HistorySink interface {
	RecordMaturity(MaturityHistoryEvent)
}

// SetHistorySink installs the maturity history consumer (nil clears it).
func (p *Pool) SetHistorySink(s HistorySink) {
	if s == nil {
		p.histSink.Store(nil)
		return
	}
	p.histSink.Store(&s)
}
