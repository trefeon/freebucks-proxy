package server

import "testing"

// TestStrictToolsFromBodyStrictPlacements pins the response-side lookup to
// the same strict-field contract the closers enforce (isStrictFlag):
// function.strict and the top-level marker the chat closer accepts both
// count; absent, null, "true" and 0 never mark a tool strict.
func TestStrictToolsFromBodyStrictPlacements(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tools string
		want  bool
	}{
		{"function-strict", `{"function":{"name":"run_it","strict":true}}`, true},
		{"top-level-strict", `{"type":"function","strict":true,"function":{"name":"run_it"}}`, true},
		{"both", `{"type":"function","strict":true,"function":{"name":"run_it","strict":true}}`, true},
		{"absent", `{"type":"function","function":{"name":"run_it"}}`, false},
		{"false", `{"type":"function","strict":false,"function":{"name":"run_it","strict":false}}`, false},
		{"null", `{"type":"function","strict":null,"function":{"name":"run_it"}}`, false},
		{"string", `{"type":"function","strict":"true","function":{"name":"run_it"}}`, false},
		{"zero", `{"type":"function","strict":0,"function":{"name":"run_it"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","messages":[],"tools":[` + tc.tools + `]}`
			if got := strictToolsFromBody([]byte(body))["run_it"]; got != tc.want {
				t.Errorf("strictToolsFromBody strict = %v, want %v", got, tc.want)
			}
		})
	}
}
