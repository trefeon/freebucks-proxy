// precious.go — MASQ precious sessions: once an account has served a model,
// its live session is never proactively dropped.
//
// The set is open and keyed by (entry, model): every granted lease marks its
// entry for its effective model, and nothing unmarks it except the entry's
// own teardown (drainRemovedToken / RemoveAllTokens). Load dropping to zero,
// recovery invalidations and operator drops all keep a precious session —
// the next Acquire reuses the same upstream instance with no re-admission.
// Only terminal signals still drop: a superseded session (another instance
// took over the account — the cached row is dead, keeping it would wedge
// every future request on it) and entry teardown (account removal, which
// must release the upstream slot).
//
// Keys hold entry pointers (never indexes): a dashboard reorder mid-flight
// must not keep or drop the wrong account's session. Guarded by its own
// mutex — it never nests with routeMu, the roster or session locks, so no
// lock ordering is introduced.
package pool

// preciousKey is one kept session: the pooled entry plus the model its live
// session is bound to. One account may hold several (one per served model);
// lanes for different models never share marks.
type preciousKey struct {
	entry *tokenEntry
	model string
}

// markPrecious records entry's session for model as precious. Called on
// every granted lease; idempotent.
func (p *Pool) markPrecious(entry *tokenEntry, model string) {
	if entry == nil || model == "" {
		return
	}
	p.preciousMu.Lock()
	defer p.preciousMu.Unlock()
	if p.precious == nil {
		p.precious = make(map[preciousKey]struct{})
	}
	p.precious[preciousKey{entry: entry, model: model}] = struct{}{}
}

// unmarkPreciousEntry drops every mark for entry. Called only from entry
// teardown (drainRemovedToken, RemoveAllTokens) so a retired entry's marks
// neither leak nor outlive the account.
func (p *Pool) unmarkPreciousEntry(entry *tokenEntry) {
	if entry == nil {
		return
	}
	p.preciousMu.Lock()
	defer p.preciousMu.Unlock()
	for k := range p.precious {
		if k.entry == entry {
			delete(p.precious, k)
		}
	}
}

// keepSession reports whether entry's live session must be kept: the
// session is bound to a model the entry was marked precious for. Entries
// with no live session model are never kept (nothing to protect, and the
// drop is a no-op anyway).
func (p *Pool) keepSession(entry *tokenEntry) bool {
	if entry == nil || entry.session == nil {
		return false
	}
	model := entry.session.Snapshot().Model
	if model == "" {
		return false
	}
	p.preciousMu.Lock()
	defer p.preciousMu.Unlock()
	_, ok := p.precious[preciousKey{entry: entry, model: model}]
	return ok
}
