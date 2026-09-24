package convert

import "testing"

// Live-gate tests for the CF-Worker egress mirror (cf_worker_signals.go,
// vendor a9ef9942d common/src/constants/cf-worker-signals.ts).
//
// The historical mirror (foreign_signals.go) classifies the wire against the
// DELETED foreign-client-signals.ts. These tests classify the proxy's EGRESS
// against the gate that actually ships: edge-stamped CF-Worker detection,
// which never looks at tools[]. A tools-presence 404 (issue #729, reopened
// #630 symptom) therefore reads CLEAR here by construction, routing the
// diagnosis to the routing/capability layer instead of the injection.

// Port of the vendor detector cases
// (common/src/constants/__tests__/cf-worker-signals.test.ts at a9ef9942d):
// the mirror must answer exactly as upstream does, or it is worse than none.
func TestDetectCfWorkerMirrorFidelity(t *testing.T) {
	none := map[string]bool{}
	cases := []struct {
		name string
		in   CfWorkerDetectInput
		want CfWorkerVerdict
	}{
		{
			name: "worker subrequest through edge detects",
			in:   CfWorkerDetectInput{CfWorkerHeader: "freebuff2api.workers.dev", CfRayHeader: "abc123-SJC", AllowedZones: none},
			want: CfWorkerVerdict{Detected: true, Zone: "freebuff2api.workers.dev"},
		},
		{
			name: "ordinary traffic ignores",
			in:   CfWorkerDetectInput{CfWorkerHeader: "", CfRayHeader: "abc123-SJC", AllowedZones: none},
			want: CfWorkerVerdict{Reason: CfWorkerNoHeader},
		},
		{
			name: "cf-worker without edge corroboration refused",
			in:   CfWorkerDetectInput{CfWorkerHeader: "anything.workers.dev", AllowedZones: none},
			want: CfWorkerVerdict{Reason: CfWorkerNotEdgeVerified},
		},
		{
			name: "own workers never flagged",
			in:   CfWorkerDetectInput{CfWorkerHeader: "app-preview-proxy", CfRayHeader: "abc-SJC", AllowedZones: ParseAllowedWorkerZones("app-preview-proxy, vly-sh-router")},
			want: CfWorkerVerdict{Reason: CfWorkerAllowlisted},
		},
		{
			name: "allowlist case-insensitive with padding",
			in:   CfWorkerDetectInput{CfWorkerHeader: "  APP-PREVIEW-PROXY ", CfRayHeader: "abc-SJC", AllowedZones: ParseAllowedWorkerZones("  App-Preview-Proxy  ,,  ")},
			want: CfWorkerVerdict{Reason: CfWorkerAllowlisted},
		},
		{
			name: "whitespace-only header is absent",
			in:   CfWorkerDetectInput{CfWorkerHeader: "   ", CfRayHeader: "abc-SJC", AllowedZones: none},
			want: CfWorkerVerdict{Reason: CfWorkerNoHeader},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectCfWorker(tc.in); got != tc.want {
				t.Errorf("DetectCfWorker = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Issue #729 acceptance: the exact reporter shape emits the known wire and
// the LIVE gate reads it CLEAR. The old mirror clears it too (decide is
// genuine under the deleted rule — pinned in request_tools_630_test.go);
// this clears it for the stronger reason: the live detector never examines
// tools[], so no wire shape can trip it.
func TestIssue729LiveGateClear(t *testing.T) {
	tools := wireToolsOf(t, map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "Say hello in one sentence."}},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "test_tool",
				"description": "A test tool",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		}},
	})
	names := toolNamesOf(tools)
	if len(names) != 3 || names[0] != "test_tool" || names[1] != "end_turn" || names[2] != "decide" {
		t.Fatalf("wire tools = %v, want [test_tool end_turn decide]", names)
	}
	// Proxy egress carries neither header on any path (pinned by
	// TestSignalGuardNoProxySignalHeaders): no_header, not detected.
	if v := ProxyCfWorkerEgressVerdict(); v.Detected || v.Reason != CfWorkerNoHeader {
		t.Errorf("proxy egress verdict = %+v, want {Detected:false Reason:no_header}", v)
	}
	// Even edge-corroborated in transit (Cloudflare adds CF-Ray to requests
	// passing through): still no cf-worker, still CLEAR. The edge can only
	// corroborate a header the proxy never sends.
	if v := DetectCfWorker(CfWorkerDetectInput{CfRayHeader: "abc123-SJC"}); v.Detected {
		t.Errorf("headerless edge-corroborated verdict = %+v, want not detected", v)
	}
}

// Issue #630 bisection matrix under the LIVE gate: the trigger is still
// tools-presence at the wire level (only the tools shape emits a tools
// array), but EVERY shape reads CLEAR — the live verdict is
// shape-independent, which is exactly why the 404 must come from the
// routing/capability layer (OpenRouter "No endpoints found for <model>"),
// not from any gate trip.
func TestIssue630MatrixLiveGateClear(t *testing.T) {
	shapes := map[string]map[string]any{
		"no-tools": {
			"model":    "deepseek/deepseek-v4-flash",
			"messages": []any{map[string]any{"role": "user", "content": "Say hello in one sentence."}},
		},
		"tools": {
			"model":    "deepseek/deepseek-v4-flash",
			"messages": []any{map[string]any{"role": "user", "content": "Say hello in one sentence."}},
			"tools": []any{map[string]any{
				"type":     "function",
				"function": map[string]any{"name": "test_tool", "description": "A test tool", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
			}},
		},
		"tool_choice-only": {
			"model":       "deepseek/deepseek-v4-flash",
			"messages":    []any{map[string]any{"role": "user", "content": "Say hello in one sentence."}},
			"tool_choice": "auto",
		},
		"empty-tools": {
			"model":    "deepseek/deepseek-v4-flash",
			"messages": []any{map[string]any{"role": "user", "content": "Say hello in one sentence."}},
			"tools":    []any{},
		},
	}
	for name, body := range shapes {
		t.Run(name, func(t *testing.T) {
			tools := wireToolsOf(t, body)
			if name == "tools" && len(tools) != 3 {
				t.Fatalf("tools shape emitted %d wire tools, want 3", len(tools))
			}
			if name != "tools" && len(tools) != 0 {
				t.Fatalf("%s emitted %d wire tools, want bare", name, len(tools))
			}
			if v := ProxyCfWorkerEgressVerdict(); v.Detected {
				t.Errorf("%s live-gate verdict = %+v, want CLEAR", name, v)
			}
		})
	}
}
