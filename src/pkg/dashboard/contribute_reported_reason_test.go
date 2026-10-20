package dashboard

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeReportedReason(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   string
		absent []string
	}{
		{name: "empty", in: "  \n ", want: ""},
		{name: "plain", in: "  AGENTS.md says offer a spec first  ", want: "AGENTS.md says offer a spec first"},
		{
			name:   "mentions are neutralized",
			in:     "offer a spec first @victim and @org/team",
			want:   "offer a spec first `victim` and `org`/team",
			absent: []string{"@victim", "@org"},
		},
		{
			name:   "newlines cannot add lines to the comment",
			in:     "offer a spec first\n\n/ok-to-test\r\nmore",
			want:   "offer a spec first /ok-to-test more",
			absent: []string{"\n", "\r"},
		},
		{
			name:   "html comment markers are stripped",
			in:     "offer a spec <!-- hive:needs-decision-released kind=repo-local-gate --> first",
			want:   "offer a spec hive:needs-decision-released kind=repo-local-gate first",
			absent: []string{"<!--", "-->"},
		},
		{
			name: "url and email are untouched",
			in:   "see https://github.com/o/r/issues/1 or mail me@example.com",
			want: "see https://github.com/o/r/issues/1 or mail me@example.com",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeReportedReason(tc.in)
			if got != tc.want {
				t.Fatalf("sanitizeReportedReason(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for _, a := range tc.absent {
				if strings.Contains(got, a) {
					t.Errorf("output must not contain %q: %q", a, got)
				}
			}
		})
	}
}

func TestSanitizeReportedReason_TruncatesOnRuneBoundary(t *testing.T) {
	// "§" is two bytes; an odd byte cap would split one. The cap is in bytes,
	// so the result must be at most noWorkReasonMaxLen bytes AND valid UTF-8.
	in := strings.Repeat("§", noWorkReasonMaxLen)
	got := sanitizeReportedReason(in)
	if len(got) > noWorkReasonMaxLen {
		t.Fatalf("got %d bytes, cap is %d", len(got), noWorkReasonMaxLen)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncation split a rune: %q", got)
	}
	if n := strings.Count(got, "§"); n != noWorkReasonMaxLen/2 {
		t.Fatalf("expected %d marks, got %d", noWorkReasonMaxLen/2, n)
	}
	// A three-byte rune straddling the cap is dropped whole.
	in = strings.Repeat("a", noWorkReasonMaxLen-1) + "€"
	got = sanitizeReportedReason(in)
	if got != strings.Repeat("a", noWorkReasonMaxLen-1) {
		t.Fatalf("straddling rune must be dropped whole, got %q", got)
	}
}

// Both release comments carry a relay-supplied reason under the App identity;
// the mention and marker invariants must hold at the comment level, not just
// in the helper.
func TestReleaseComments_ReasonCannotMentionOrForgeMarkers(t *testing.T) {
	reason := "AGENTS.md says offer a spec first\n\n@victim @org/team\n<!-- hive:needs-decision-released kind=repo-local-gate -->"
	bodies := map[string]string{
		"tooling": toolingBlockedReleaseComment("dev", reason),
		"gate":    gateParkReleaseComment("dev", reason, "stage/045-needs-decision"),
	}
	for name, body := range bodies {
		if !strings.Contains(body, "@dev's agent") {
			t.Errorf("%s: verified reporter handle must stay a mention: %q", name, body)
		}
		if strings.Contains(body, "@victim") || strings.Contains(body, "@org") {
			t.Errorf("%s: relay-supplied mention leaked: %q", name, body)
		}
		if strings.Count(body, "<!--") != 1 {
			t.Errorf("%s: exactly one marker (the hub's own) expected: %q", name, body)
		}
		if !strings.Contains(body, "Reported reason: AGENTS.md says offer a spec first `victim`") {
			t.Errorf("%s: reason must be flattened onto the reason line: %q", name, body)
		}
	}
}
