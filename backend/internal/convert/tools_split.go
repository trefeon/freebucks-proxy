package convert

import (
	"encoding/json"
	"strings"
)

// SplitConcatenatedJSONValues is the defensive normalizer for glued tool-call
// arguments: some models emit two complete JSON objects back to back in one
// arguments string (live: {"path":A}{"path":B} in a single call's arguments).
// The response leg otherwise decodes the first value and silently drops the
// rest, so the second operation never runs.
//
// It consumes consecutive top-level JSON values with encoding/json.Decoder
// and returns the exact source slice per value. Parts are returned only when
// two or more complete values consume the whole string ignoring whitespace;
// a single valid JSON value, empty input, and malformed/trailing-garbage
// input all return (nil, false) so the caller keeps the args untouched.
// Stdlib only; no allocation beyond the part substrings.
func SplitConcatenatedJSONValues(args string) ([]string, bool) {
	s := strings.TrimSpace(args)
	if s == "" {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(s))
	var parts []string
	start := 0
	for {
		rest := strings.TrimLeft(s[start:], " \t\r\n")
		if rest == "" {
			break
		}
		start = len(s) - len(rest)
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		end := int(dec.InputOffset())
		if end < start || end > len(s) {
			return nil, false
		}
		parts = append(parts, s[start:end])
		start = end
	}
	if len(parts) < 2 {
		return nil, false
	}
	return parts, true
}
