package review

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A contributor PR got changes_requested with one finding anchored on the
// workflow file, asking for line references in a config file to be updated.
// The author's fix touched the config file and a doc (2 files, 13 lines):
// under delta_trivial_lines and in no file a finding was anchored on, so the
// old planner re-stamped changes_requested onto the fix head with no review
// session, published nothing for it, and the PR went back to its author with
// nothing new to fix. A changes_requested or requires_human is never carried
// across a real change; only an identical tree (or an empty base diff) is.
func fixInOtherFileDelta(prev string) *DeltaDiff {
	return knownDelta(prev,
		DeltaFile{Path: ".github/lint-config.yml", Additions: 8, Deletions: 3},
		DeltaFile{Path: "docs/ci.md", Additions: 1, Deletions: 1},
	)
}

func TestPlanDeltaNeverCarriesChangesRequestedAcrossAFixInAnotherFile(t *testing.T) {
	finding := flagged(".github/workflows/build.yml", 40, "update the line references in .github/lint-config.yml")
	for _, verdict := range []Verdict{VerdictChangesRequested, VerdictRequiresHuman} {
		t.Run(string(verdict), func(t *testing.T) {
			prior := priorAggregate("sha1", verdict, deltaNow.Add(-time.Hour), finding)
			prior.DecisionQuestion = "is the pinned action acceptable?"
			pr := dispatchPR("sha2")
			pr.DeltaDiff = fixInOtherFileDelta("sha1")
			if pr.DeltaDiff.ChangedLines() != 13 || pr.DeltaDiff.ChangedLines() >= DefaultDeltaTrivialLines {
				t.Fatalf("fixture must sit under the trivial threshold, got %d lines", pr.DeltaDiff.ChangedLines())
			}
			plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{prior}}, DispatchState{}, deltaOpts(reviewer("r1")))
			if len(plan.DeltaRestamps) != 0 {
				t.Fatalf("%s carried across the author's fix without a review: %+v", verdict, plan.DeltaRestamps)
			}
			if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].DeltaPreviousHead != "sha1" {
				t.Fatalf("want one delta review of the fix, got %+v", plan.ReviewKicks)
			}
			msg := plan.ReviewKicks[0].Message
			for _, want := range []string{".github/lint-config.yml", "[correctness] .github/workflows/build.yml:40"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("delta prompt lacks %q:\n%s", want, msg)
				}
			}
		})
	}
}

func TestPlanDeltaStillCarriesChangesRequestedAcrossAnIdenticalTree(t *testing.T) {
	prior := priorAggregate("sha1", VerdictChangesRequested, deltaNow.Add(-time.Hour), flagged(".github/workflows/build.yml", 40, "stale line refs"))
	pr := dispatchPR("sha2")
	pr.DeltaDiff = knownDelta("sha1")
	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{prior}}, DispatchState{}, deltaOpts(reviewer("r1")))
	if len(plan.ReviewKicks) != 0 || len(plan.DeltaRestamps) != 1 {
		t.Fatalf("identical tree: kicks=%d restamps=%d, want 0/1", len(plan.ReviewKicks), len(plan.DeltaRestamps))
	}
	agg := plan.DeltaRestamps[0].Aggregate
	if agg.Verdict != VerdictChangesRequested || agg.Delta == nil || !strings.Contains(agg.Delta.Note, "identical tree") || len(agg.Findings) != 1 {
		t.Fatalf("restamped aggregate = %+v delta=%+v", agg, agg.Delta)
	}
	if agg.CarriedWithoutReading() {
		t.Fatal("an identical-tree carry must survive LoadArtifact")
	}
}

func TestPlanDeltaApproveTrivialCarryUnchanged(t *testing.T) {
	prior := priorAggregate("sha1", VerdictApprove, deltaNow.Add(-time.Hour))
	pr := dispatchPR("sha2")
	pr.DeltaDiff = fixInOtherFileDelta("sha1")
	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{prior}}, DispatchState{}, deltaOpts(reviewer("r1")))
	if len(plan.ReviewKicks) != 0 || len(plan.DeltaRestamps) != 1 {
		t.Fatalf("approve, small unflagged delta: kicks=%d restamps=%d, want 0/1", len(plan.ReviewKicks), len(plan.DeltaRestamps))
	}
	if agg := plan.DeltaRestamps[0].Aggregate; agg.Verdict != VerdictApprove || agg.CarriedWithoutReading() {
		t.Fatalf("approve carry changed: %+v", agg)
	}
	// Base-sync approve: unchanged, at any size.
	diff := baseSyncDelta("sha1")
	plan = planBaseSync(t, priorAggregate("sha1", VerdictApprove, deltaNow.Add(-2*time.Hour)), diff, deltaOpts(reviewer("r1")))
	if len(plan.ReviewKicks) != 0 || len(plan.DeltaRestamps) != 1 || plan.DeltaRestamps[0].Aggregate.Delta == nil || !plan.DeltaRestamps[0].Aggregate.Delta.BaseSync {
		t.Fatalf("base-sync approve: kicks=%d restamps=%+v, want one base-sync restamp", len(plan.ReviewKicks), plan.DeltaRestamps)
	}
}

// A changes_requested with nothing the author can read is not carried even
// across an identical tree: a re-stamp publishes nothing, so the PR would go
// back to its author with no ask.
func TestPlanDeltaUnreadableChangesRequestedIsReviewedNotCarried(t *testing.T) {
	prior := priorAggregate("sha1", VerdictChangesRequested, deltaNow.Add(-time.Hour))
	for name, mutate := range map[string]func(*PullRequest){
		"identical tree":  func(pr *PullRequest) { pr.DeltaDiff = knownDelta("sha1") },
		"empty base diff": func(pr *PullRequest) { pr.BaseDiffEmpty = true; pr.DeltaDiff = knownDelta("sha1") },
	} {
		t.Run(name, func(t *testing.T) {
			pr := dispatchPR("sha2")
			mutate(&pr)
			plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{prior}}, DispatchState{}, deltaOpts(reviewer("r1")))
			if len(plan.DeltaRestamps) != 0 || len(plan.ReviewKicks) != 1 {
				t.Fatalf("unreadable changes_requested: kicks=%d restamps=%+v, want a review", len(plan.ReviewKicks), plan.DeltaRestamps)
			}
		})
	}
}

func TestDeltaTrivialNeverCarriesBlockingVerdictsAcrossAChange(t *testing.T) {
	small := DeltaDiff{Known: true, Files: []DeltaFile{{Path: "x.yml", Additions: 3}}}
	for _, v := range []Verdict{VerdictChangesRequested, VerdictRequiresHuman} {
		prior := Aggregate{Verdict: v, Findings: []PerspectiveFinding{flagged("y.yml", 1, "bug")}}
		if deltaTrivial(small, prior, 20) {
			t.Fatalf("%s carried across a small change in an unflagged file", v)
		}
		if !deltaTrivial(DeltaDiff{Known: true}, prior, 20) {
			t.Fatalf("%s not carried across an identical tree", v)
		}
	}
	if !deltaTrivial(small, Aggregate{Verdict: VerdictApprove}, 20) {
		t.Fatal("approve no longer carried across a small unflagged change")
	}
}

// review-verdicts.json written by the old planner holds the carried verdict
// for the fix head. Reading it drops that item, so the head reads as not yet
// judged and the planner reviews the fix.
func TestLoadArtifactDropsChangesRequestedCarriedAcrossARealChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review-verdicts.json")
	prior := priorAggregate("sha1", VerdictChangesRequested, deltaNow.Add(-time.Hour), flagged(".github/workflows/build.yml", 40, "stale line refs"))
	legacy := prior
	legacy.HeadSHA = "sha2"
	legacy.RecordedAt = deltaNow.Add(-30 * time.Minute)
	legacy.Delta = &DeltaReviewInfo{PreviousHead: "sha1", Trivial: true, ChangedLines: 13, Note: "re-stamped from sha1: delta touches 2 file(s), 13 line(s), none previously flagged"}
	identical := prior
	identical.HeadSHA = "sha3"
	identical.RecordedAt = deltaNow.Add(-2 * time.Hour)
	identical.Delta = &DeltaReviewInfo{PreviousHead: "sha1", Trivial: true, Note: "re-stamped from sha1: identical tree"}
	approve := priorAggregate("sha4", VerdictApprove, deltaNow)
	approve.Number = 8
	approve.Delta = &DeltaReviewInfo{PreviousHead: "sha0", Trivial: true, ChangedLines: 4}
	if err := WriteArtifact(path, Artifact{Items: []Aggregate{prior, legacy, identical, approve}}); err != nil {
		t.Fatal(err)
	}
	art, err := LoadArtifact(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := art.AggregateFor("acme/hive", 7, "sha2"); ok {
		t.Fatal("changes_requested carried across a 13-line change still reads as the fix head's verdict")
	}
	for _, keep := range []struct {
		number int
		head   string
	}{{7, "sha1"}, {7, "sha3"}, {8, "sha4"}} {
		if _, ok := art.AggregateFor("acme/hive", keep.number, keep.head); !ok {
			t.Fatalf("#%d@%s dropped; only an unread carry may go", keep.number, keep.head)
		}
	}
	pr := dispatchPR("sha2")
	pr.DeltaDiff = fixInOtherFileDelta("sha1")
	plan := PlanDispatch([]PullRequest{pr}, art, DispatchState{}, deltaOpts(reviewer("r1")))
	if len(plan.DeltaRestamps) != 0 || len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].DeltaPreviousHead != "sha1" {
		t.Fatalf("fix head after load: kicks=%+v restamps=%+v, want one delta review", plan.ReviewKicks, plan.DeltaRestamps)
	}
}

// The withheld-fix record must not claim a review was published for a head
// whose verdict was carried onto it without one.
func TestWithheldFixOnCarriedVerdictNamesTheHeadItCameFrom(t *testing.T) {
	pr := humanPR("sha2")
	carried := changesRequestedFor(pr)
	carried.Findings = []PerspectiveFinding{flagged(".github/workflows/build.yml", 40, "stale line refs")}
	carried.Delta = &DeltaReviewInfo{PreviousHead: "sha1", Trivial: true, Note: "re-stamped from sha1: identical tree"}
	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{carried}}, DispatchState{}, fixGateOpts(false))
	if len(plan.WithheldFixes) != 1 {
		t.Fatalf("withheld = %+v, want one", plan.WithheldFixes)
	}
	w := plan.WithheldFixes[0]
	if w.CarriedFrom != "sha1" || strings.Contains(w.Reason, "review published") || !strings.Contains(w.Reason, "nothing published for this head") {
		t.Fatalf("carried withheld entry = %+v", w)
	}
	reviewed := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, DispatchState{}, fixGateOpts(false))
	if len(reviewed.WithheldFixes) != 1 || reviewed.WithheldFixes[0].CarriedFrom != "" || !strings.Contains(reviewed.WithheldFixes[0].Reason, "review published") {
		t.Fatalf("reviewed head's withheld entry changed: %+v", reviewed.WithheldFixes)
	}
}

func TestFindingAsksListsInScopeFindings(t *testing.T) {
	agg := Aggregate{Findings: []PerspectiveFinding{
		flagged(".github/workflows/build.yml", 40, "update the line\nreferences"),
		{Perspective: PerspectiveSecurity, Finding: flagged("", 0, "").Finding},
		{Perspective: PerspectiveSecurity, Finding: func() PerspectiveFinding {
			f := flagged("pkg/a.go", 3, "out of scope")
			f.Finding.ReviewScope = "out-of-scope"
			return f
		}().Finding},
		{Perspective: PerspectiveCorrectness, Finding: func() PerspectiveFinding {
			f := flagged("", 0, "")
			f.Finding.Summary = "summary only"
			return f
		}().Finding},
	}}
	got := agg.FindingAsks(0)
	want := []string{"`.github/workflows/build.yml:40` update the line references", "summary only"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("FindingAsks = %q, want %q", got, want)
	}
	if got := agg.FindingAsks(1); len(got) != 2 || got[1] != "and 1 more finding(s) in the review" {
		t.Fatalf("limited FindingAsks = %q", got)
	}
}
