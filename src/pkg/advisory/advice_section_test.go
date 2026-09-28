package advisory

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hiveadvisor"
)

func TestFormatDigestMarkdownIncludesAdviceSection(t *testing.T) {
	end := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	advice := &hiveadvisor.Result{
		Epoch: hiveadvisor.Epoch{Mode: "SURGE", End: end},
		Recommendations: []hiveadvisor.Recommendation{{
			ID:        "throttle-pr-producing-lanes",
			Title:     "Throttle PR-producing lanes",
			Rationale: "Reduce new PR creation until pressure falls.",
			Signals:   []hiveadvisor.Signal{{Name: "mode", Value: "SURGE"}},
		}},
		NextReviewInDays: 3,
		Counts:           hiveadvisor.Counts{GovernorPRs: 3, GovernorIssues: 20, OverviewPRs: 93, OverviewIssues: 295},
	}
	md := FormatDigestMarkdown(BuildDigest(nil, "SURGE"), DigestOptions{ShowEmpty: true, Advice: advice})
	for _, want := range []string{
		"## Advice", "Throttle PR-producing lanes", "mode=SURGE", "next review in 3 day(s)",
		"governor actionable queue 3 PRs / 20 issues", "Overview chart 93 open PRs / 295 open issues",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("digest missing %q:\n%s", want, md)
		}
	}
}

// TestAdviceSectionRendersQueueExplanation (#9103): the blocked-PR advice
// carries its first items, the export list, and the rule text for every band
// it names; a cleared recommendation is struck through without stale numbers.
func TestAdviceSectionRendersQueueExplanation(t *testing.T) {
	end := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	advice := &hiveadvisor.Result{
		Epoch: hiveadvisor.Epoch{Mode: "IDLE", End: end},
		Recommendations: []hiveadvisor.Recommendation{
			{
				ID:        "reduce-blocked-prs",
				Title:     "Reduce the blocked PR queue",
				Rationale: "70 of 93 open PRs (75%) are Blocked. 41 of 70 fail check `go-security-analysis` — fix the check before touching cadence.",
				Signals:   []hiveadvisor.Signal{{Name: "blocked", Value: "70"}},
				Links:     []hiveadvisor.Link{{Label: "Blocked PRs (CSV)", URL: "/api/overview/prs.csv?band=blocked"}},
				Items: []hiveadvisor.Item{
					{Repo: "octo/demo", Number: 12, URL: "https://github.com/octo/demo/pull/12", Note: "fails go-security-analysis"},
					{Repo: "octo/demo", Number: 13, Note: "merge conflict"},
				},
				Bands: []string{"pr/blocked"},
			},
			{ID: "widen-hive-repos", Title: "Widen HIVE_REPOS", Rationale: "No longer applies: the signal that triggered this advice has cleared since the epoch began.", Cleared: true},
		},
		Bands:            []hiveadvisor.BandRule{{Kind: "pr", Key: "blocked", Label: "Blocked", Rule: "blocked sweep verdict, merge conflicts, or failing CI — read the verdict, rebase, or fix the checks"}},
		NextReviewInDays: 6,
	}

	withOrigin := FormatDigestMarkdown(BuildDigest(nil, "IDLE"), DigestOptions{ShowEmpty: true, Advice: advice, DashboardURL: "https://hive.example.com/"})
	for _, want := range []string{
		"- **Reduce the blocked PR queue** — 70 of 93 open PRs (75%) are Blocked.",
		"  - First: [octo/demo#12](https://github.com/octo/demo/pull/12) (fails go-security-analysis), octo/demo#13 (merge conflict)\n",
		"  - Full list: [Blocked PRs (CSV)](https://hive.example.com/api/overview/prs.csv?band=blocked)\n",
		"- ~~**Widen HIVE_REPOS**~~ — No longer applies",
		"Bands named above (same rules as the Overview chart):\n- **Blocked** (PRs): blocked sweep verdict, merge conflicts, or failing CI",
		"numbers, items and links are recomputed on every digest",
	} {
		if !strings.Contains(withOrigin, want) {
			t.Fatalf("digest missing %q:\n%s", want, withOrigin)
		}
	}
	if strings.Contains(withOrigin, "No longer applies: the signal that triggered this advice has cleared since the epoch began. (signals") {
		t.Fatalf("cleared advice must not carry signals:\n%s", withOrigin)
	}

	withoutOrigin := FormatDigestMarkdown(BuildDigest(nil, "IDLE"), DigestOptions{ShowEmpty: true, Advice: advice})
	if !strings.Contains(withoutOrigin, "  - Full list: Blocked PRs (CSV): `/api/overview/prs.csv?band=blocked`\n") {
		t.Fatalf("relative export path should render as code without a dashboard origin:\n%s", withoutOrigin)
	}
}
