package dashboard

import (
	"strings"
	"testing"
)

func TestRepoCardPRBandsStaticWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"const info = entry.pr;",
		"function groupedRepoPRs(openPrs, heldPrs)",
		"const prPills = groupedRepoPRs(r.openPrs || [], r.heldPrs || []).map(g => {",
		"repo-pr-band-title",
		"PR_BAND_ORDER",
		"prUpdatedAt",
		"prReviewClassRank",
		"prSignalHTML(bandInfo)",
		"✗ CI",
		"⑂",
		"🕒",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	if strings.Contains(html, "heldPrPills") {
		t.Error("held PRs are still rendered as a trailing append instead of joining PR bands")
	}
}
