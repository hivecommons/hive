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
		"## Advice", "Throttle PR-producing lanes", "mode=SURGE",
		"Hive keeps this list until 2026-09-23, so it does not jump around between reports.",
		"If Hive's mode changes from `SURGE`, it picks a new list sooner.",
		"governor actionable queue 3 PRs / 20 issues", "Overview chart 93 open PRs / 295 open issues",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("digest missing %q:\n%s", want, md)
		}
	}
}

// The digest says in plain words that the list is picked once a week and the
// numbers under it are not, instead of "Frozen for governor mode …".
func TestAdviceSectionSaysWhenTheListWasPicked(t *testing.T) {
	start := time.Date(2026, 10, 26, 17, 27, 0, 0, time.UTC)
	advice := &hiveadvisor.Result{
		Epoch:           hiveadvisor.Epoch{Mode: "BUSY", Start: start, End: start.Add(hiveadvisor.EpochLength)},
		Recommendations: []hiveadvisor.Recommendation{{ID: "rebalance-merge-side", Title: "Rebalance toward merge-side agents"}},
	}
	var b strings.Builder
	writeAdviceSection(&b, advice, "")
	want := "> Hive picked this list on 2026-10-26 and keeps it until 2026-11-02, so it does not jump around between reports. " +
		"The numbers, items and links under each one are worked out fresh for every report. " +
		"If Hive's mode changes from `BUSY`, it picks a new list sooner.\n"
	if !strings.Contains(b.String(), want) {
		t.Fatalf("advice header missing\nwant %q\nin:\n%s", want, b.String())
	}
	if strings.Contains(b.String(), "Frozen for") || strings.Contains(b.String(), "next review in") {
		t.Fatalf("advice header still uses the old wording:\n%s", b.String())
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
		"  - First:\n    - [octo/demo#12](https://github.com/octo/demo/pull/12) — fails go-security-analysis\n    - octo/demo#13 — merge conflict\n",
		"  - Numbers behind this: blocked=70\n",
		"  - Full list: [Blocked PRs (CSV)](https://hive.example.com/api/overview/prs.csv?band=blocked)\n",
		"- ~~**Widen HIVE_REPOS**~~ — No longer applies",
		"Bands named above (same rules as the Overview chart):\n- **Blocked** (PRs): blocked sweep verdict, merge conflicts, or failing CI",
		"numbers, items and links under each one are worked out fresh for every report",
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

// The needs-human advice reads as a short list: the headline, why the items
// wait (one line per reason), how to help, then one item per line, with the
// raw signals last instead of trailing the headline.
func TestAdviceSectionRendersWhyAndHowToHelp(t *testing.T) {
	advice := &hiveadvisor.Result{
		Epoch: hiveadvisor.Epoch{Mode: "BUSY", End: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)},
		Recommendations: []hiveadvisor.Recommendation{{
			ID:        "clear-human-gate-prs",
			Title:     "Clear the human gate on PRs",
			Rationale: "59 of 94 open PRs (62%) are waiting on a person; the hive cannot move them on its own.",
			Why: []string{
				"24 parked until a maintainer decides (20 in utah-packages, 3 in utah, 1 in testsuite).",
				"21 waiting for an approving review that branch protection requires (16 in common, 5 in actions).",
			},
			Help:    "answer the parked decisions in utah-packages and utah; approve PRs in common and actions",
			Items:   []hiveadvisor.Item{{Repo: "actions", Number: 675, Note: "REVIEW — Hive's review is parked for a person"}},
			Signals: []hiveadvisor.Signal{{Name: "needs_human", Value: "59"}},
		}},
	}
	md := FormatDigestMarkdown(BuildDigest(nil, "BUSY"), DigestOptions{ShowEmpty: true, Advice: advice})
	want := "- **Clear the human gate on PRs** — 59 of 94 open PRs (62%) are waiting on a person; the hive cannot move them on its own.\n" +
		"  - Why they wait:\n" +
		"    - 24 parked until a maintainer decides (20 in utah-packages, 3 in utah, 1 in testsuite).\n" +
		"    - 21 waiting for an approving review that branch protection requires (16 in common, 5 in actions).\n" +
		"  - **How to help:** answer the parked decisions in utah-packages and utah; approve PRs in common and actions.\n" +
		"  - First:\n" +
		"    - actions#675 — REVIEW — Hive's review is parked for a person\n" +
		"  - Numbers behind this: needs_human=59\n"
	if !strings.Contains(md, want) {
		t.Fatalf("digest advice block:\n%s\nwant:\n%s", md, want)
	}
}
