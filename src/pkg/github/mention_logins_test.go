package github

import (
	"slices"
	"testing"
)

// MentionLogins keeps the spelling of the first mention, dedupes
// case-insensitively, marks bot accounts with their "[bot]" suffix and skips
// e-mail addresses, the same rule extractMentionLogins applies.
func TestMentionLoginsKeepsSpellingAndBots(t *testing.T) {
	for _, tc := range []struct {
		body string
		want []string
	}{
		{"@Erin42 should decide whether this can land", []string{"Erin42"}},
		{"@grace: a maintainer must confirm; cc @Grace", []string{"grace"}},
		{"@renovate[bot] and @hive-app[bot]: @carol should decide", []string{"renovate[bot]", "hive-app[bot]", "carol"}},
		{"mail ops@example.com, ask @example-org/maintainers", []string{"example-org/maintainers"}},
		{"nobody is named here", nil},
	} {
		if got := MentionLogins(tc.body); !slices.Equal(got, tc.want) {
			t.Errorf("MentionLogins(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}
