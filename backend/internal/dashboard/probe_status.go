package dashboard

import (
	"freebuff-proxy/backend/internal/pool"
	"time"
)

// probe_status.go — GET /admin/api/probe-status: the smart-probe
// scheduler's per-token due state. Cheap predicate reads only (no upstream
// traffic, safe to poll or curl during an incident): whether each token is
// due for a probe and, when it is not, the machine-readable suppression
// reason (same vocabulary smartProbeDueToken returns: locked,
// quarantined, banned, cooling, country-blocked, inflight, fresh,
// debounced, reset-pending, idle, no-entry). This is the observability
// behind the silent-tick incident: a never-probed token always answers WHY
// here, instead of staying idle with no dispatch and no error.

// ProbeStatusResponse is the GET /admin/api/probe-status answer.
type ProbeStatusResponse struct {
	Enabled bool                  `json:"enabled"`
	Tokens  []pool.ProbeStatusRow `json:"tokens"`
}

// probeStatusData synthesizes the probe-status view model.
func (d *Dashboard) probeStatusData() ProbeStatusResponse {
	res := ProbeStatusResponse{}
	if d == nil || d.pool == nil {
		return res
	}
	if cfg := d.cfg(); cfg != nil {
		res.Enabled = cfg.SmartProbeEnabled
	}
	res.Tokens = d.pool.ProbeStatus(time.Now())
	return res
}
