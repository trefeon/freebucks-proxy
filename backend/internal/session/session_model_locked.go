// session_model_locked.go — issue #160 model-lock release metering.
//
// Split from session.go (CI line cap): the model_locked admission branch in
// Manager.refresh releases the old slot and re-admits with the requested
// model, and this counter tracks the switching cost per from → to pair so
// /metrics can surface freebuff_proxy_model_locked_total.
package session

// recordModelLock tallies one model-lock release for the (from, to) model
// pair (issue #160): called from the refresh model_locked branch right where
// the old slot is released and the desired model re-admitted. Own mutex —
// refresh holds no other lock here.
func (m *Manager) recordModelLock(from, to string) {
	m.modelLockedMu.Lock()
	defer m.modelLockedMu.Unlock()
	if m.modelLocked == nil {
		m.modelLocked = make(map[string]map[string]int64)
	}
	tos := m.modelLocked[from]
	if tos == nil {
		tos = make(map[string]int64)
		m.modelLocked[from] = tos
	}
	tos[to]++
}

// ModelLocked returns a copy of the model-lock release counter keyed by
// from → to model pair (empty map when no releases). Fed to /metrics as
// freebuff_proxy_model_locked_total (issue #160).
func (m *Manager) ModelLocked() map[string]map[string]int64 {
	m.modelLockedMu.Lock()
	defer m.modelLockedMu.Unlock()
	if m.modelLocked == nil {
		return map[string]map[string]int64{}
	}
	out := make(map[string]map[string]int64, len(m.modelLocked))
	for from, tos := range m.modelLocked {
		toCopy := make(map[string]int64, len(tos))
		for to, n := range tos {
			toCopy[to] = n
		}
		out[from] = toCopy
	}
	return out
}
