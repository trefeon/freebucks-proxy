package dashboard

import (
	"strconv"
	"strings"
	"time"

	"freebuff-proxy/backend/internal/phasetiming"
	"freebuff-proxy/backend/internal/store"
)

// --- traces ---

type tracesData struct {
	Enabled bool         `json:"enabled"`
	Traces  []traceEntry `json:"traces"`
}

type traceEntry struct {
	Time   string    `json:"time"`
	Token  string    `json:"token"`
	Model  string    `json:"model"`
	Status string    `json:"status"`
	Ms     string    `json:"ms"`
	Error  string    `json:"error"`
	Phases []PhaseKV `json:"phases,omitempty"`
	// ReqID threads the request correlation id the chat trace log has
	// always carried (the display row used to drop it). The token counts
	// ride the Contract UsageRecord keys verbatim when the chat path logs
	// them, omitted otherwise so rows without counts render unchanged.
	ReqID     string `json:"req_id,omitempty"`
	TsMs      int64  `json:"ts_ms,omitempty"`
	Input     int64  `json:"input,omitempty"`
	Output    int64  `json:"output,omitempty"`
	Cached    int64  `json:"cached,omitempty"`
	Reasoning int64  `json:"reasoning,omitempty"`
	Total     int64  `json:"total,omitempty"`
	// RateTokens is the acquire-time rate-limit's limited-token set the
	// chat trace logs verbatim (comma-joined 1-based, e.g. "2" or "1,3").
	// String passthrough like Token: the dashboard never interprets it.
	RateTokens string `json:"rate_tokens,omitempty"`
}

// tracesData merges the live ring with the history store so the Traces
// console view survives restarts: ring traces first, then "chat trace" rows
// spilled to the DB fill up to the cap. Dedupe reuses the log key — a
// trace already spilled never renders twice.
func (d *Dashboard) tracesData() tracesData {
	td := tracesData{Enabled: d.logs != nil || d.hist != nil}
	seen := map[string]bool{}
	if d.logs != nil {
		for _, e := range d.logs.Recent(200) {
			if e.Message != "chat trace" {
				continue
			}
			seen[logDedupeKey(e.Time, e.Level, e.Message, e.Fields)] = true
			td.Traces = append(td.Traces, traceFromFields(e.Time, e.Fields))
		}
	}
	if d.hist != nil && len(td.Traces) < 200 {
		rows, err := d.hist.QueryLogs(store.LogFilter{Contains: "chat trace", Limit: 200})
		if err != nil {
			d.logger.Warn("traces query failed", "err", err)
			return td
		}
		for _, e := range rows {
			if e.Msg != "chat trace" {
				continue
			}
			fields := strings.Split(e.Fields, "\n")
			ts := time.UnixMilli(e.TS).UTC().Format(time.RFC3339)
			if seen[logDedupeKey(ts, e.Level, e.Msg, fields)] {
				continue
			}
			td.Traces = append(td.Traces, traceFromFields(ts, fields))
			if len(td.Traces) >= 200 {
				break
			}
		}
	}
	return td
}

// traceFromFields parses one retained "chat trace" record into its display
// row. Shared by the ring and DB paths so both render identically.
func traceFromFields(timeStr string, fields []string) traceEntry {
	entry := traceEntry{Time: timeStr, Status: "ok"}
	var phaseMap map[string]int64
	for _, f := range fields {
		key, value, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		switch key {
		case "token":
			entry.Token = value
		case "rate_tokens":
			entry.RateTokens = value
		case "model":
			entry.Model = value
		case "status":
			entry.Status = value
		case "req_id":
			entry.ReqID = value
		case "input":
			entry.Input = parseTraceCount(value)
		case "output":
			entry.Output = parseTraceCount(value)
		case "cached":
			entry.Cached = parseTraceCount(value)
		case "reasoning":
			entry.Reasoning = parseTraceCount(value)
		case "total":
			entry.Total = parseTraceCount(value)
		case "ms":
			entry.Ms = value + "ms"
		case "error":
			entry.Error = value
		default:
			if phaseNames[key] {
				if phaseMap == nil {
					phaseMap = make(map[string]int64, 5)
				}
				if v, err := strconv.ParseInt(value, 10, 64); err == nil {
					phaseMap[key] = v
				}
			}
		}
	}
	if phaseMap != nil {
		entry.Phases = PhaseList(phaseMap)
	}
	// TsMs re-derives millis from the display stamp so ring and DB rows
	// carry the same key (both paths format RFC3339); unparseable stamps
	// omit it via omitempty.
	if ts, err := time.Parse(time.RFC3339, timeStr); err == nil {
		entry.TsMs = ts.UnixMilli()
	}
	if entry.Token == "" && entry.RateTokens == "" {
		entry.Token = "—"
	}
	return entry
}

// parseTraceCount parses one logged token count; malformed values read 0
// (omitted downstream via omitempty) and never fail the row.
func parseTraceCount(s string) int64 {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

// phaseNames is the phase-timing field set a trace row renders.
var phaseNames = map[string]bool{
	phasetiming.AcquireMS:        true,
	phasetiming.SessionRefreshMS: true,
	phasetiming.RunAcquireMS:     true,
	phasetiming.QueueWaitMS:      true,
	phasetiming.UpstreamTTFBMS:   true,
	phasetiming.TotalMS:          true,
}
