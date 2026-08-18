package dashboard

import "testing"

func TestRedactSecret(t *testing.T) {
	tests := []struct {
		name   string
		msg    string
		secret string
		want   string
	}{
		{"empty_secret", "gateway returned 401: sk-abc", "", "gateway returned 401: sk-abc"},
		{"no_match", "connection refused", "sk-abc", "connection refused"},
		{"single_match", "error: sk-abc is invalid", "sk-abc", "error: [redacted] is invalid"},
		{"multiple_matches", "key sk-abc sent to sk-abc endpoint", "sk-abc", "key [redacted] sent to [redacted] endpoint"},
		{"empty_msg", "", "sk-abc", ""},
		{"secret_is_entire_msg", "sk-abc", "sk-abc", "[redacted]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactSecret(tc.msg, tc.secret); got != tc.want {
				t.Errorf("redactSecret(%q, %q) = %q, want %q", tc.msg, tc.secret, got, tc.want)
			}
		})
	}
}
