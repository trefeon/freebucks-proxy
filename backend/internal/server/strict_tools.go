package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// --- Strict tool-calling translation layer ---
//
// Clients in strict mode (notably Roo Code, which converts every function
// schema for OpenAI strict mode: strict:true, all props in required,
// additionalProperties:false) need the proxy to enforce — not silently
// downgrade — that contract, while loose clients (Hermes, OpenClaw and
// friends declare no strict flag at all) must see zero behavior change.
//
// The layer has two halves:
//
//  1. Closer (request path): validateChatStrictTools,
//     validateResponsesStrictTools and validateAnthropicStrictTools run in
//     the handlers BEFORE conversion/normalization, on the raw client shape
//     (chat tools[], Responses flat tools, Anthropic input_schema). A tool
//     declaring strict:true must carry parameters.type=object with
//     required[] covering every declared property and
//     additionalProperties=false, else the request fails with 400
//     strict_violation via writeClientError (taxonomy-owned envelope+type).
//     Validation never mutates: strict markers forward verbatim upstream.
//     Roo nests required/additionalProperties as siblings of parameters on
//     the function object (pinned by TestConformanceRooStrictSchemaPreserved),
//     so the closer unions both placements instead of demanding them inside
//     parameters.
//  2. Gate (Anthropic response path): parseJSONArgsForTool threads the
//     per-tool strict lookup (tool name -> declared strict, built from the
//     original request body) into the tool_use input translation. Bad-JSON
//     or empty arguments yield 400 invalid_tool_arguments ONLY when the
//     invoked tool was declared strict:true; loose tools keep the legacy {}
//     fallback byte-identical.
//
// tool_choice re-pointing stays where it is (convert.ToolMapper via
// engine.go); this file owns strictness only.

// strictViolationCode is the client-error code for a strict:true tool whose
// declared schema does not meet the strict contract.
const strictViolationCode = "strict_violation"

// invalidToolArgumentsCode is the client-error code for unusable (bad-JSON
// or empty) arguments returned for a tool declared strict:true.
const invalidToolArgumentsCode = "invalid_tool_arguments"

// validateChatStrictTools enforces the strict contract on chat-completions
// tools[] entries ({type:function, function:{name, strict, parameters}}).
// Returns "" when every strict tool satisfies the contract.
func validateChatStrictTools(raw map[string]any) string {
	tools, ok := raw["tools"].([]any)
	if !ok {
		return ""
	}
	for _, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := tool["type"].(string); typ != "" && typ != "function" {
			continue // non-function shapes are rejected by their own validator
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		if !isStrictFlag(fn["strict"]) && !isStrictFlag(tool["strict"]) {
			continue
		}
		name, _ := fn["name"].(string)
		if msg := checkStrictSchema(name, fn["parameters"], fn); msg != "" {
			return msg
		}
	}
	return ""
}

// validateResponsesStrictTools enforces the strict contract on the flat
// Responses function tools ({type:function, name, strict, parameters}).
// Returns "" when every strict tool satisfies the contract.
func validateResponsesStrictTools(raw map[string]any) string {
	tools, ok := raw["tools"].([]any)
	if !ok {
		return ""
	}
	for _, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := tool["type"].(string); typ != "" && typ != "function" {
			continue // built-in tools are rejected by the converter
		}
		if !isStrictFlag(tool["strict"]) {
			continue
		}
		name, _ := tool["name"].(string)
		if msg := checkStrictSchema(name, tool["parameters"], tool); msg != "" {
			return msg
		}
	}
	return ""
}

// validateAnthropicStrictTools enforces the strict contract on Anthropic
// tools ({name, input_schema}) that opt in with a strict:true marker.
// Tools without the marker (Hermes, OpenClaw, Claude Code, ...) always pass.
// Returns "" when every strict tool satisfies the contract.
func validateAnthropicStrictTools(raw map[string]any) string {
	tools, ok := raw["tools"].([]any)
	if !ok {
		return ""
	}
	for _, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := tool["type"].(string); typ != "" {
			continue // server-side tool types are rejected by the converter
		}
		if !isStrictFlag(tool["strict"]) {
			continue
		}
		name, _ := tool["name"].(string)
		if msg := checkStrictSchema(name, tool["input_schema"], tool); msg != "" {
			return msg
		}
	}
	return ""
}

// isStrictFlag reports whether a decoded strict marker opts into strict mode.
func isStrictFlag(v any) bool {
	b, ok := v.(bool)
	return ok && b
}

// checkStrictSchema verifies one strict:true tool declaration: the schema
// must be an object schema with type object, required[] covering every
// declared property, and additionalProperties false. holder carries the
// sibling placement (Roo-style required/additionalProperties next to the
// schema rather than inside it); both placements are unioned. Returns "" on
// success, otherwise the 400 message.
func checkStrictSchema(toolName string, schema any, holder map[string]any) string {
	display := toolName
	if display == "" {
		display = "(unnamed)"
	}
	params, ok := schema.(map[string]any)
	if !ok || params == nil {
		return fmt.Sprintf("strict tool %q must declare parameters as a JSON object schema with type object, every property listed in required, and additionalProperties false", display)
	}
	if typ, _ := params["type"].(string); typ != "object" {
		return fmt.Sprintf("strict tool %q must set parameters type to \"object\" (strict mode does not support type %q)", display, typ)
	}
	var props []string
	if p, ok := params["properties"].(map[string]any); ok {
		for k := range p {
			props = append(props, k)
		}
	}
	sort.Strings(props)
	required := stringSet(params["required"])
	if hv, ok := holderValue(holder, "required"); ok {
		for k := range stringSet(hv) {
			required[k] = true
		}
	}
	for _, prop := range props {
		if !required[prop] {
			return fmt.Sprintf("strict tool %q: property %q must be listed in required (strict mode requires every property)", display, prop)
		}
	}
	ap, ok := params["additionalProperties"]
	if !ok {
		ap, ok = holderValue(holder, "additionalProperties")
	}
	if !ok {
		return fmt.Sprintf("strict tool %q must set additionalProperties to false", display)
	}
	if b, isBool := ap.(bool); !isBool || b {
		return fmt.Sprintf("strict tool %q must set additionalProperties to false", display)
	}
	return ""
}

// holderValue reads a sibling key off the declaration holder (nil-safe).
func holderValue(holder map[string]any, key string) (any, bool) {
	if holder == nil {
		return nil, false
	}
	v, ok := holder[key]
	return v, ok
}

// stringSet builds a set from a decoded JSON string array ([]any or []string).
func stringSet(v any) map[string]bool {
	out := map[string]bool{}
	switch typed := v.(type) {
	case []any:
		for _, e := range typed {
			if s, ok := e.(string); ok {
				out[s] = true
			}
		}
	case []string:
		for _, s := range typed {
			out[s] = true
		}
	}
	return out
}

// strictToolsFromBody builds the per-tool strict lookup (tool name ->
// declared strict) from a chat-envelope request body (the original body the
// handlers stash in the request context: raw chat JSON, or the converted
// chat params for the Responses/Anthropic surfaces, whose wraps preserve the
// client's strict flag). Unknown/invalid bodies yield an empty map.
func strictToolsFromBody(body []byte) map[string]bool {
	out := map[string]bool{}
	if len(body) == 0 {
		return out
	}
	var payload struct {
		Tools []struct {
			Function struct {
				Name   string `json:"name"`
				Strict bool   `json:"strict"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return out
	}
	for _, t := range payload.Tools {
		if t.Function.Name != "" && t.Function.Strict {
			out[t.Function.Name] = true
		}
	}
	return out
}

// strictToolsFromRequest extracts the per-tool strict lookup for the request
// whose original body is stashed in its context (nil-safe: a missing request
// or body means no tool is strict).
func strictToolsFromRequest(r *http.Request) map[string]bool {
	if r == nil {
		return nil
	}
	return strictToolsFromBody(originalBodyFromContext(r.Context()))
}

// parseJSONArgsForTool parses a tool arguments string into a JSON object with
// per-tool strictness: loose tools keep the legacy {} fallback for bad-JSON
// or empty arguments; a tool declared strict:true turns those into an error
// so the caller can fail the turn with 400 invalid_tool_arguments.
func parseJSONArgsForTool(args, toolName string, strictTools map[string]bool) (map[string]any, error) {
	if !strictTools[toolName] {
		return parseJSONArgs(args), nil
	}
	display := toolName
	if display == "" {
		display = "(unnamed)"
	}
	if strings.TrimSpace(args) == "" {
		return nil, fmt.Errorf("invalid tool arguments for strict tool %q: arguments must be a JSON object", display)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(args), &out); err != nil {
		return nil, fmt.Errorf("invalid tool arguments for strict tool %q: arguments are not valid JSON", display)
	}
	if out == nil {
		return nil, fmt.Errorf("invalid tool arguments for strict tool %q: arguments must be a JSON object", display)
	}
	return out, nil
}
