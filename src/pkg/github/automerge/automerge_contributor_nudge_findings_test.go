package automerge

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A PR returned to its author on a review-swarm changes_requested lists what
// the review asked for, not only that it asked: the nudge comment and the
// relay fix-pr task both read these asks.
func TestNudgeListsTheFindingsOfAChangesRequestedVerdict(t *testing.T) {
	d := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Verdict: "changes_requested", FindingAsks: []string{
		"`.github/workflows/build.yml:40` update the line references in .github/lint-config.yml",
		"ask @maintainer before pinning",
	}}}
	want := []string{
		"the review swarm requested changes",
		"review finding: `.github/workflows/build.yml:40` update the line references in .github/lint-config.yml",
		"review finding: ask `maintainer` before pinning",
	}
	for _, reason := range []string{contributorLaneReasonChangesRequested, stagelabels.LaneReasonChangesRequested} {
		_, asks, _ := nudgeDiagnosis(reason, d)
		if strings.Join(asks, "|") != strings.Join(want, "|") {
			t.Fatalf("%s: asks = %q, want %q", reason, asks, want)
		}
	}
	// Another lane reason with the blocking verdict beside it lists them too.
	_, asks, _ := nudgeDiagnosis("label:needs-rebase", d)
	if len(asks) != 4 || asks[1] != want[0] || asks[2] != want[1] {
		t.Fatalf("rebase + verdict asks = %q", asks)
	}
	// The human route's "verdict changes_requested" reason adds them once.
	_, asks, _ = nudgeDiagnosisRouted(contributorLaneReasonChangesRequested, d, &stagelabels.Route{Reasons: []string{"verdict changes_requested @abc1234"}})
	if strings.Join(asks, "|") != strings.Join(want, "|") {
		t.Fatalf("routed asks = %q, want %q", asks, want)
	}
	comment := ContributorNudgeComment(ContributorNudge{Author: "contributor", HeadSHA: "35ebb52", Reason: contributorLaneReasonChangesRequested, ReviewAsks: want, Nudge: 1}, ContributorNudgeOutcome{Mode: "comment"})
	if !strings.Contains(comment, "- review finding: `.github/workflows/build.yml:40` update the line references") {
		t.Fatalf("nudge comment does not list the finding:\n%s", comment)
	}
	// A verdict that does not block adds no findings, and no verdict adds
	// only the lead line.
	_, asks, _ = nudgeDiagnosis(contributorLaneReasonChangesRequested, stageDetail{})
	if len(asks) != 1 || asks[0] != want[0] {
		t.Fatalf("no verdict: asks = %q", asks)
	}
}
