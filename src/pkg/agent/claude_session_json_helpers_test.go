package agent

// Direct tests for the two JSON-shape predicates behind Claude session
// auth-state detection (claude_session_state.go). hasNonEmptyJSONValue decides
// whether a .claude.json oauthAccount key represents a REAL signed-in account
// or one of the empty shapes the settings clobber writes back (null, {}, "");
// misclassifying an empty shape as signed-in would skip the login heal on an
// unauthenticated agent. Previously both helpers were covered only via
// loadClaudeSessionInfo's happy path.

import (
	"encoding/json"
	"testing"
)

func TestHasNonEmptyJSONValue(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"absent key (nil raw)", "", false},
		{"json null", `null`, false},
		{"empty object — the clobber shape", `{}`, false},
		{"empty array", `[]`, false},
		{"empty string", `""`, false},
		{"malformed json", `{oops`, false},
		{"populated object", `{"uuid":"abc"}`, true},
		{"populated array", `[1]`, true},
		{"non-empty string", `"acct"`, true},
		{"number", `42`, true},
		{"boolean false is still a value", `false`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.raw != "" {
				raw = json.RawMessage(tc.raw)
			}
			if got := hasNonEmptyJSONValue(raw); got != tc.want {
				t.Errorf("hasNonEmptyJSONValue(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestJSONValueIsTrue(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"absent key", "", false},
		{"literal true", `true`, true},
		{"literal false", `false`, false},
		{"json null", `null`, false},
		{"truthy-looking string is not the literal", `"true"`, false},
		{"number is not the literal", `1`, false},
		{"malformed json", `tru`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.raw != "" {
				raw = json.RawMessage(tc.raw)
			}
			if got := jsonValueIsTrue(raw); got != tc.want {
				t.Errorf("jsonValueIsTrue(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
