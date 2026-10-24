package logscrub

import "testing"

func TestRedactPaneTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain text untouched", "running tests\nall green", "running tests\nall green"},
		{"github token", "token ghp_abcdefghij1234567890 end", "token ghp_abc*** end"},
		{"github pat token", "github_pat_ABCDEFGHIJ1234567890", "github_***"},
		{"anthropic key", "key sk-ant-abc123def456", "key sk-***REDACTED***"},
		{"hive virtual key at line start", "sk-hive-qa", "sk-***REDACTED***"},
		{"key in quotes", "ANTHROPIC_API_KEY='sk-hive-qa1234'", "ANTHROPIC_API_KEY='sk-***REDACTED***'"},
		{"sk inside identifier preserved", "module task-sk-flow ok", "module task-sk-flow ok"},
		{"short sk prefix preserved", "sk- alone", "sk- alone"},
		{"device code inline", "one-time code: ABCD-1234", "[auth flow redacted]"},
		{"device code case insensitive", "ONE-TIME CODE: ab12-cd34", "ONE-TIME CODE: ****-****"},
		{"device login url line", "before\nVisit https://github.com/login/device now\nafter", "before\n[auth flow redacted]\nafter"},
		{"waiting for authorization", "Waiting for authorization...\n", "[auth flow redacted]\n"},
		{"press any key", "Press any key to copy code\ndone", "[auth flow redacted]\ndone"},
		{"multiple secrets across lines", "a ghs_abcdefghij1234\nb sk-hive-qa1234\n", "a ghs_abc***\nb sk-***REDACTED***\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactPaneTokens(tc.in); got != tc.want {
				t.Fatalf("RedactPaneTokens(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
