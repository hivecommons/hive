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
		{"@m2Giles should decide whether this can land", []string{"m2Giles"}},
		{"@kylerankin: a maintainer must confirm; cc @KyleRankin", []string{"kylerankin"}},
		{"@renovate[bot] and @hive-app[bot]: @bketelsen should decide", []string{"renovate[bot]", "hive-app[bot]", "bketelsen"}},
		{"mail ops@example.com, ask @projectbluefin/maintainers", []string{"projectbluefin/maintainers"}},
		{"nobody is named here", nil},
	} {
		if got := MentionLogins(tc.body); !slices.Equal(got, tc.want) {
			t.Errorf("MentionLogins(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}
