package review

import (
	"strings"
	"testing"
	"time"
)

// A push after a changes_requested or requires_human verdict is the author's
// answer to it. A docs or changelog edit addressing review feedback touches
// no file a finding named and stays under delta_trivial_lines, so the old
// trivial rule re-stamped the old verdict onto the new head and the PR sat
// at "review not started" with nobody re-reading the answer. Such a head
// now gets a delta re-review; a prior approve and an identical tree still
// re-stamp.

func docsOnlyAnswerDelta(prev string) *DeltaDiff {
	return knownDelta(prev,
		DeltaFile{Path: "src/docs/sandbox-isolation.md", Additions: 4, Deletions: 2},
		DeltaFile{Path: "changelog.d/security-19457-sandbox-containment.md", Additions: 1, Deletions: 1},
	)
}

func TestPlanDeltaNonApproveVerdictIsReReviewedNotRestamped(t *testing.T) {
	for _, verdict := range []Verdict{VerdictRequiresHuman, VerdictChangesRequested} {
		t.Run(string(verdict), func(t *testing.T) {
			art := Artifact{Items: []Aggregate{priorAggregate("sha1", verdict, deltaNow.Add(-time.Hour), flagged("src/crux/sandbox/sandbox.go", 178, "strict mode not propagated"))}}
			pr := dispatchPR("sha2")
			pr.DeltaDiff = docsOnlyAnswerDelta("sha1")
			plan := PlanDispatch([]PullRequest{pr}, art, DispatchState{}, deltaOpts(reviewer("r1")))
			if len(plan.DeltaRestamps) != 0 {
				t.Fatalf("a %s verdict was re-stamped onto the head that answers it: %+v", verdict, plan.DeltaRestamps)
			}
			if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].DeltaPreviousHead != "sha1" {
				t.Fatalf("want one delta re-review kick from sha1, got %+v", plan.ReviewKicks)
			}
			msg := plan.ReviewKicks[0].Message
			for _, want := range []string{"DELTA RE-REVIEW", "documentation and changelog files only", "docs-currency and intent-alignment", "src/crux/sandbox/sandbox.go:178"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("delta prompt lacks %q:\n%s", want, msg)
				}
			}
		})
	}
}

func TestPlanDeltaNonApproveSmallCodeDeltaIsReReviewed(t *testing.T) {
	art := Artifact{Items: []Aggregate{priorAggregate("sha1", VerdictChangesRequested, deltaNow.Add(-time.Hour), flagged("pkg/a/a.go", 12, "bug"))}}
	pr := dispatchPR("sha2")
	pr.DeltaDiff = knownDelta("sha1", DeltaFile{Path: "pkg/b/b.go", Additions: 3})
	plan := PlanDispatch([]PullRequest{pr}, art, DispatchState{}, deltaOpts(reviewer("r1")))
	if len(plan.ReviewKicks) != 1 || len(plan.DeltaRestamps) != 0 {
		t.Fatalf("kicks=%d restamps=%d, want 1/0", len(plan.ReviewKicks), len(plan.DeltaRestamps))
	}
	if strings.Contains(plan.ReviewKicks[0].Message, "documentation and changelog files only") {
		t.Fatal("a code delta was described as docs-only")
	}
}

func TestPlanDeltaRestampStillCoversApproveAndIdenticalTree(t *testing.T) {
	approve := Artifact{Items: []Aggregate{priorAggregate("sha1", VerdictApprove, deltaNow.Add(-time.Hour))}}
	pr := dispatchPR("sha2")
	pr.DeltaDiff = docsOnlyAnswerDelta("sha1")
	if plan := PlanDispatch([]PullRequest{pr}, approve, DispatchState{}, deltaOpts(reviewer("r1"))); len(plan.DeltaRestamps) != 1 || len(plan.ReviewKicks) != 0 {
		t.Fatalf("approve + small docs delta: kicks=%d restamps=%d, want 0/1", len(plan.ReviewKicks), len(plan.DeltaRestamps))
	}
	human := Artifact{Items: []Aggregate{priorAggregate("sha1", VerdictRequiresHuman, deltaNow.Add(-time.Hour))}}
	pr.DeltaDiff = knownDelta("sha1")
	if plan := PlanDispatch([]PullRequest{pr}, human, DispatchState{}, deltaOpts(reviewer("r1"))); len(plan.DeltaRestamps) != 1 || len(plan.ReviewKicks) != 0 {
		t.Fatalf("requires_human + identical tree: kicks=%d restamps=%d, want 0/1", len(plan.ReviewKicks), len(plan.DeltaRestamps))
	}
}

func TestDeltaDocsOnly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []DeltaFile
		want  bool
	}{
		{"empty", nil, false},
		{"markdown and fragment", []DeltaFile{{Path: "README.md"}, {Path: "changelog.d/fixed-x.md"}}, true},
		{"docs dir asset", []DeltaFile{{Path: "src/docs/img/flow.svg"}}, true},
		{"rst upper case", []DeltaFile{{Path: "Guide/INSTALL.RST"}}, true},
		{"code mixed in", []DeltaFile{{Path: "docs/a.md"}, {Path: "pkg/a/a.go"}}, false},
		{"docs-like file name", []DeltaFile{{Path: "pkg/docs.go"}}, false},
		{"blank path", []DeltaFile{{Path: " "}}, false},
	} {
		if got := deltaDocsOnly(tc.files); got != tc.want {
			t.Fatalf("%s: deltaDocsOnly = %v, want %v", tc.name, got, tc.want)
		}
	}
}
