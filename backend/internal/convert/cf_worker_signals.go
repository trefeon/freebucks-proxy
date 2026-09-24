package convert

import "strings"

// Living mirror of upstream detectCfWorker (vendor a9ef9942d,
// common/src/constants/cf-worker-signals.ts — verified byte-identical
// through tip cf92539c4; deliberately untracked upstream-side per #730,
// observe-only).
//
// What upstream catches with it: reseller Workers (pingmike2/freebuff2api and
// relatives) pooling harvested account tokens behind an OpenAI-compatible
// endpoint. Cloudflare stamps `CF-Worker` (the Worker's zone) onto every
// outbound subrequest a Worker makes AFTER fetch() returns to the runtime, so
// Worker code cannot remove it; `CF-Ray` corroborates the request actually
// traversed our edge (a cf-worker on a request that never touched Cloudflare
// is caller-authored noise and reads not_edge_verified, never detected).
//
// Why this mirror exists alongside the historical one: the deleted
// foreign-client-signals.ts keyed on CLIENT-chosen signals (tool names,
// system prompt, fingerprint_id) — see foreign_signals.go, retained as a
// pinned historical reference. This detector keys on EDGE-stamped
// infrastructure: the offered tools[] NEVER participate in the verdict, so no
// wire shape the proxy emits (not even #729's [test_tool end_turn decide])
// can trip it. That separation is the whole point of mirroring it here: a
// tools-presence 404 (issue #729, reopened #630 symptom) reads CLEAR under
// the live gate by construction, which routes the diagnosis to the
// routing/capability layer (OpenRouter "No endpoints found for <model>")
// instead of back into the injection.
//
// Proxy egress verdict: the proxy is a plain Go process, never a Cloudflare
// Worker, and sends neither header on any path — pinned by
// TestSignalGuardNoProxySignalHeaders (backend/internal/upstream/
// signal_guard_test.go), with CF-Worker listed for outbound stripping in
// backend/internal/stealth/headers.go. ProxyCfWorkerEgressVerdict returns
// that pinned outcome ({Detected:false, Reason:no_header}) so tests can
// assert it without hand-rolling headers.

// CfWorkerNoHeader, CfWorkerNotEdgeVerified and CfWorkerAllowlisted mirror
// the not-detected reasons of upstream CfWorkerVerdict. Empty reason means
// detected (upstream's {detected:true} arm carries no reason field).
const (
	CfWorkerNoHeader        = "no_header"
	CfWorkerNotEdgeVerified = "not_edge_verified"
	CfWorkerAllowlisted     = "allowlisted"
)

// CfWorkerDetectInput mirrors upstream CfWorkerDetectInput: the raw header
// values ("" when absent) plus our own Worker zones.
type CfWorkerDetectInput struct {
	CfWorkerHeader string
	CfRayHeader    string
	AllowedZones   map[string]bool
}

// CfWorkerVerdict mirrors upstream CfWorkerVerdict: either detected with the
// lowercased zone, or not detected with the reason why.
type CfWorkerVerdict struct {
	Detected bool
	Zone     string
	Reason   string
}

// ParseAllowedWorkerZones mirrors upstream parseAllowedWorkerZones: comma
// separated, trimmed, lowercased, empties dropped.
func ParseAllowedWorkerZones(raw string) map[string]bool {
	out := map[string]bool{}
	for _, zone := range strings.Split(raw, ",") {
		zone = strings.ToLower(strings.TrimSpace(zone))
		if zone != "" {
			out[zone] = true
		}
	}
	return out
}

// DetectCfWorker mirrors upstream detectCfWorker exactly: blank (or
// whitespace-only) cf-worker reads no_header without consulting cf-ray; a
// present cf-worker without cf-ray reads not_edge_verified; an allowlisted
// zone reads allowlisted; anything else is detected under the lowercased
// zone. Pure function, no I/O — replayable offline like the original.
func DetectCfWorker(in CfWorkerDetectInput) CfWorkerVerdict {
	raw := strings.TrimSpace(in.CfWorkerHeader)
	if raw == "" {
		return CfWorkerVerdict{Reason: CfWorkerNoHeader}
	}
	if strings.TrimSpace(in.CfRayHeader) == "" {
		return CfWorkerVerdict{Reason: CfWorkerNotEdgeVerified}
	}
	zone := strings.ToLower(raw)
	if in.AllowedZones[zone] {
		return CfWorkerVerdict{Reason: CfWorkerAllowlisted}
	}
	return CfWorkerVerdict{Detected: true, Zone: zone}
}

// ProxyCfWorkerEgressVerdict returns the live-gate verdict for proxy egress:
// the proxy never sends cf-worker (or cf-ray) on any path, so the detector
// reads no_header — CLEAR — for every request shape, tools-bearing or not.
// Diagnostic-only, like the historical mirror: the proxy never enforces.
func ProxyCfWorkerEgressVerdict() CfWorkerVerdict {
	return DetectCfWorker(CfWorkerDetectInput{})
}
