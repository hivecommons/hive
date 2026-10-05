package dashboard

import (
	"strings"
	"testing"
)

func TestGithubLoginForMention(t *testing.T) {
	accept := []string{"octocat", "@octocat", "a", "a-b", "hive-quality", "dependabot[bot]", strings.Repeat("a", 39)}
	for _, in := range accept {
		if got := githubLoginForMention(in); got != strings.TrimPrefix(in, "@") {
			t.Errorf("%q: got %q, want accepted", in, got)
		}
	}
	reject := []string{"", "-octo", "octo-", "oc--to", "oc to", "oc_to", "@", strings.Repeat("a", 40), "<script>", "a[bot]x"}
	for _, in := range reject {
		if got := githubLoginForMention(in); got != "" {
			t.Errorf("%q: got %q, want rejected", in, got)
		}
	}
}
