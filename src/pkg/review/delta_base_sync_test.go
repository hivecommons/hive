package review

import (
	"strings"
	"testing"
	"time"
)

// baseSyncDelta is a large known delta that the hive's own merge of the base
// produced: 400 lines in two base files, neither of them a PR file
// (projectbluefin/fsdk-containers#482).
func baseSyncDelta(prev string) *DeltaDiff {
	d := knownDelta(prev,
		DeltaFile{Path: "elements/valkey/valkey.bst", Additions: 250, Deletions: 50},
		DeltaFile{Path: "docs/skills/index.md", Additions: 100},
	)
	d.BaseSync = true
	d.BaseFiles = []string{"docs/skills/index.md", "elements/valkey/valkey.bst"}
	d.PRFiles = []string{"elements/cups/libcupsfilters.bst", "docs/fsdk.md"}
	return d
}

func planBaseSync(t *testing.T, prior Aggregate, diff *DeltaDiff, opts DispatchOptions) DispatchPlan {
	t.Helper()
	pr := dispatchPR("sha2")
	pr.BodyDigest = "body2"
	pr.DeltaDiff = diff
	return PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{prior}}, DispatchState{}, opts)
}

func TestPlanDeltaBaseSyncDisjointRestampsAtAnySize(t *testing.T) {
	prior := priorAggregate("sha1", VerdictApprove, deltaNow.Add(-2*time.Hour))
	diff := baseSyncDelta("sha1")
	if diff.ChangedLines() < DefaultDeltaTrivialLines {
		t.Fatalf("fixture delta is %d lines; it must be over the trivial threshold to test the base-sync rule", diff.ChangedLines())
	}
	plan := planBaseSync(t, prior, diff, deltaOpts(reviewer("r1")))
	if len(plan.ReviewKicks) != 0 || len(plan.State.Pending) != 0 || len(plan.DeltaRestamps) != 1 {
		t.Fatalf("kicks=%d pending=%d restamps=%d, want 0/0/1", len(plan.ReviewKicks), len(plan.State.Pending), len(plan.DeltaRestamps))
	}
	agg := plan.DeltaRestamps[0].Aggregate
	if agg.HeadSHA != "sha2" || agg.BodyDigest != "body2" || agg.Verdict != VerdictApprove || !agg.MergeEligible {
		t.Fatalf("restamped aggregate = %+v", agg)
	}
	if agg.Delta == nil || !agg.Delta.Trivial || !agg.Delta.BaseSync || agg.Delta.ChangedLines != 400 || !strings.Contains(agg.Delta.Note, "hive base-sync") || !strings.Contains(agg.Delta.Note, "disjoint from the PR's files") {
		t.Fatalf("delta info = %+v", agg.Delta)
	}
}

func TestPlanDeltaBaseSyncFallsThroughToDeltaReview(t *testing.T) {
	approve := priorAggregate("sha1", VerdictApprove, deltaNow.Add(-2*time.Hour))
	cases := map[string]struct {
		prior  Aggregate
		mutate func(*DeltaDiff)
		opts   func(*DispatchOptions)
	}{
		"base changed a PR file": {prior: approve, mutate: func(d *DeltaDiff) {
			d.PRFiles = append(d.PRFiles, "elements/valkey/valkey.bst")
		}},
		"not a hive base-sync (human, other bot or non-merge commit)": {prior: approve, mutate: func(d *DeltaDiff) {
			d.BaseSync = false
		}},
		"delta has a file the base did not change": {prior: approve, mutate: func(d *DeltaDiff) {
			d.Files = append(d.Files, DeltaFile{Path: "elements/cups/extra.bst", Additions: 3})
		}},
		"merge dropped a base change": {prior: approve, mutate: func(d *DeltaDiff) {
			d.BaseFiles = append(d.BaseFiles, "elements/base-only.bst")
		}},
		"unknown compare": {prior: approve, mutate: func(d *DeltaDiff) {
			d.Known = false
		}},
		"prior changes_requested": {prior: priorAggregate("sha1", VerdictChangesRequested, deltaNow.Add(-2*time.Hour), flagged("elements/cups/libcupsfilters.bst", 3, "bug"))},
		"prior requires_human":    {prior: priorAggregate("sha1", VerdictRequiresHuman, deltaNow.Add(-2*time.Hour))},
		"re-stamps opted out":     {prior: approve, opts: func(o *DispatchOptions) { o.DeltaTrivialLines = -1 }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			diff := baseSyncDelta("sha1")
			if tc.mutate != nil {
				tc.mutate(diff)
			}
			opts := deltaOpts(reviewer("r1"))
			if tc.opts != nil {
				tc.opts(&opts)
			}
			plan := planBaseSync(t, tc.prior, diff, opts)
			if len(plan.DeltaRestamps) != 0 || len(plan.ReviewKicks) != 1 {
				t.Fatalf("kicks=%d restamps=%+v, want a delta review and no restamp", len(plan.ReviewKicks), plan.DeltaRestamps)
			}
			if plan.ReviewKicks[0].DeltaPreviousHead != "sha1" {
				t.Fatalf("not a delta kick: %+v", plan.ReviewKicks[0])
			}
		})
	}
}

func TestDeltaDiffBaseSyncOnly(t *testing.T) {
	if !baseSyncDelta("sha1").BaseSyncOnly() {
		t.Fatal("disjoint base-sync not recognised")
	}
	d := baseSyncDelta("sha1")
	d.Files[0].Path = " " + d.Files[0].Path + " "
	if !d.BaseSyncOnly() {
		t.Fatal("whitespace around a path broke the match")
	}
	// An empty delta is the identical-tree rule's, not this one's.
	if (&DeltaDiff{Known: true, BaseSync: true}).BaseSyncOnly() {
		t.Fatal("empty delta counted as a base-sync")
	}
	// The same file count but a different file is not the base's change.
	d = baseSyncDelta("sha1")
	d.BaseFiles = []string{"docs/skills/index.md", "elements/other.bst"}
	if d.BaseSyncOnly() {
		t.Fatal("delta file the base did not change accepted")
	}
}
