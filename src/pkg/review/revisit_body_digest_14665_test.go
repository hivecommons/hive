package review

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// hivecommons/hive#14665: perspective reports may omit body_digest, and the
// planner read the gap as "the PR body changed since the verdict" — so every
// changes_requested verdict with digestless reports went back to the
// reviewers instead of the fixer, every cycle, with fix_kicks: 0.

func revisitFixPR(t *testing.T, sha string) PullRequest {
	t.Helper()
	pr := dispatchPR(sha)
	pr.Title = "fix: guard nil input"
	pr.BodyDigest = PullRequestBodyDigest(pr.Title, "Closes #1")
	pr.UpdatedAt = time.Date(2026, 10, 27, 18, 0, 0, 0, time.UTC)
	return pr
}

// qualifiedRevisitPR mirrors the planner's org-qualification of pr.Repo so
// state lookups keyed by repo match what PlanDispatch recorded.
func qualifiedRevisitPR(pr PullRequest, opts DispatchOptions) PullRequest {
	pr.Repo = fullRepoName(pr.Repo, opts.ProjectOrg)
	return pr
}

func revisitFixOpts(now time.Time) DispatchOptions {
	opts := fixGateOpts(false)
	opts.Now = now
	return opts
}

func revisitFixAggregate(pr PullRequest, bodyDigest string, recordedAt time.Time) Aggregate {
	agg := changesRequestedFor(pr)
	agg.BodyDigest = bodyDigest
	agg.RecordedAt = recordedAt
	agg.Perspectives = map[Perspective]Verdict{PerspectiveCorrectness: VerdictChangesRequested}
	return agg
}

func TestMissingBodyDigestIsUnknownNotEdited(t *testing.T) {
	pr := revisitFixPR(t, "sha1")
	now := pr.UpdatedAt.Add(time.Hour)
	// The verdict predates the PR's updated_at (a label change bumps it), so
	// the timestamp fallback alone would also have called this an edit.
	agg := revisitFixAggregate(pr, "", pr.UpdatedAt.Add(-time.Hour))

	if bodyEditedVerdictRevisitable(pr, agg) {
		t.Fatal("a verdict with no body digest must not read as body-edited")
	}
	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{agg}}, DispatchState{}, revisitFixOpts(now))
	if len(plan.FixKicks) != 1 || len(plan.ReviewKicks) != 0 {
		t.Fatalf("digestless changes_requested: fix=%d review=%d, want 1 / 0 (exclusions %+v)", len(plan.FixKicks), len(plan.ReviewKicks), plan.FixExclusions)
	}
	if len(plan.FixExclusions) != 0 {
		t.Fatalf("fix exclusions = %+v, want none", plan.FixExclusions)
	}
}

func TestMatchingBodyDigestDispatchesFix(t *testing.T) {
	pr := revisitFixPR(t, "sha1")
	now := pr.UpdatedAt.Add(time.Hour)
	agg := revisitFixAggregate(pr, pr.BodyDigest, pr.UpdatedAt.Add(-time.Hour))

	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{agg}}, DispatchState{}, revisitFixOpts(now))
	if len(plan.FixKicks) != 1 || len(plan.ReviewKicks) != 0 {
		t.Fatalf("matching digest: fix=%d review=%d, want 1 / 0", len(plan.FixKicks), len(plan.ReviewKicks))
	}
}

func TestDifferingBodyDigestRevisitsAndIsVisible(t *testing.T) {
	pr := revisitFixPR(t, "sha1")
	now := pr.UpdatedAt.Add(time.Hour)
	agg := revisitFixAggregate(pr, PullRequestBodyDigest(pr.Title, "Refs #1"), pr.UpdatedAt.Add(-time.Hour))

	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{agg}}, DispatchState{}, revisitFixOpts(now))
	if len(plan.FixKicks) != 0 || len(plan.ReviewKicks) == 0 {
		t.Fatalf("edited body: fix=%d review=%d, want 0 / >0", len(plan.FixKicks), len(plan.ReviewKicks))
	}
	// The suppressed fix is not silent: the cycle names the PR and why.
	if len(plan.FixExclusions) != 1 || plan.FixExclusions[0].Reason != FixDispatchExcludedRevisitBodyEdited || plan.FixExclusions[0].Attempts != 1 {
		t.Fatalf("fix exclusions = %+v, want one %s at cycle 1", plan.FixExclusions, FixDispatchExcludedRevisitBodyEdited)
	}
	if len(plan.State.RevisitStarved) != 1 || plan.State.RevisitStarved[0].Cycles != 1 {
		t.Fatalf("revisit starvation = %+v, want one head at 1 cycle", plan.State.RevisitStarved)
	}
}

// After RevisitStarvationCycles consecutive revisit cycles with no fix kick,
// the planner forces the fix path and resets the counter.
func TestRevisitStarvationForcesFixAfterNCycles(t *testing.T) {
	pr := revisitFixPR(t, "sha1")
	now := pr.UpdatedAt.Add(time.Hour)
	agg := revisitFixAggregate(pr, PullRequestBodyDigest(pr.Title, "Refs #1"), pr.UpdatedAt.Add(-time.Hour))
	artifact := Artifact{Items: []Aggregate{agg}}

	state := DispatchState{}
	for cycle := 1; cycle <= RevisitStarvationCycles; cycle++ {
		opts := revisitFixOpts(now.Add(time.Duration(cycle) * time.Minute))
		plan := PlanDispatch([]PullRequest{pr}, artifact, state, opts)
		if len(plan.FixKicks) != 0 {
			t.Fatalf("cycle %d: fix kicked before the starvation limit", cycle)
		}
		if len(plan.ForcedFixes) != 0 {
			t.Fatalf("cycle %d: forced fix before the starvation limit: %+v", cycle, plan.ForcedFixes)
		}
		if got := revisitStarvedCycles(plan.State, qualifiedRevisitPR(pr, opts)); got != cycle {
			t.Fatalf("cycle %d: starved counter = %d", cycle, got)
		}
		if len(plan.FixExclusions) != 1 || plan.FixExclusions[0].Attempts != cycle {
			t.Fatalf("cycle %d: exclusions = %+v", cycle, plan.FixExclusions)
		}
		state = plan.State
	}

	plan := PlanDispatch([]PullRequest{pr}, artifact, state, revisitFixOpts(now.Add(time.Hour)))
	if len(plan.FixKicks) != 1 {
		t.Fatalf("after %d starved cycles the fix must be forced: fix=%d exclusions=%+v", RevisitStarvationCycles, len(plan.FixKicks), plan.FixExclusions)
	}
	if len(plan.ForcedFixes) != 1 || plan.ForcedFixes[0].Reason != FixDispatchExcludedRevisitBodyEdited || plan.ForcedFixes[0].Cycles != RevisitStarvationCycles {
		t.Fatalf("forced fixes = %+v", plan.ForcedFixes)
	}
	if len(plan.State.RevisitStarved) != 0 {
		t.Fatalf("starvation counter not reset after the forced fix: %+v", plan.State.RevisitStarved)
	}
	// The forced fix leaves the in-flight revisit's pending entries alone so
	// the relay can still bind the re-review's verdicts.
	if len(plan.State.Pending) == 0 {
		t.Fatal("forced fix dropped the in-flight revisit's pending entries")
	}
}

// A head whose revisit is interrupted by a normal settle does not carry its
// count over; "consecutive" means consecutive.
func TestRevisitStarvationResetsOnSettledCycle(t *testing.T) {
	pr := revisitFixPR(t, "sha1")
	now := pr.UpdatedAt.Add(time.Hour)
	edited := revisitFixAggregate(pr, PullRequestBodyDigest(pr.Title, "Refs #1"), pr.UpdatedAt.Add(-time.Hour))

	opts := revisitFixOpts(now)
	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{edited}}, DispatchState{}, opts)
	if revisitStarvedCycles(plan.State, qualifiedRevisitPR(pr, opts)) != 1 {
		t.Fatalf("starved = %+v, want 1 cycle", plan.State.RevisitStarved)
	}
	settled := revisitFixAggregate(pr, pr.BodyDigest, now)
	plan = PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{settled}}, plan.State, revisitFixOpts(now.Add(time.Minute)))
	if len(plan.FixKicks) != 1 || len(plan.State.RevisitStarved) != 0 {
		t.Fatalf("settled cycle: fix=%d starved=%+v, want 1 / none", len(plan.FixKicks), plan.State.RevisitStarved)
	}
}

func TestRevisitStarvationPrunedWhenHeadMoves(t *testing.T) {
	pr := revisitFixPR(t, "sha2")
	state := DispatchState{RevisitStarved: []RevisitStarvation{{Repo: "acme/hive", Number: pr.Number, HeadSHA: "sha1", Cycles: 2}}}
	plan := PlanDispatch([]PullRequest{pr}, Artifact{}, state, revisitFixOpts(pr.UpdatedAt))
	if len(plan.State.RevisitStarved) != 0 {
		t.Fatalf("old head's starvation survived a push: %+v", plan.State.RevisitStarved)
	}
}

// Collect stamps a digestless report with the digest its dispatch recorded,
// so the verdict artifact carries what the reviewer was shown even when the
// model left body_digest out.
func TestCollectStampsBodyDigestFromDispatch(t *testing.T) {
	dir := t.TempDir()
	report := `{"lane":"review-swarm","kind":"review","findings":[],"prs_opened":[],"beads_filed":[],"summary":"ok","perspective":"security","verdict":"changes_requested","repo":"acme/hive","number":7,"head_sha":"sha1"}`
	if err := os.WriteFile(filepath.Join(dir, ReviewReportFilePrefix+"security"+ReviewReportFileSuffix), []byte(report), 0o644); err != nil {
		t.Fatal(err)
	}
	digest := PullRequestBodyDigest("fix: guard nil input", "Closes #1")
	stamps := ModelStampsFromState(DispatchState{Pending: []PendingReview{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", BodyDigest: digest, Perspective: PerspectiveSecurity, Agent: "r1", Dispatched: time.Now()}}})
	art, err := Collect(dir, AggregateOptions{ModelStamps: stamps})
	if err != nil {
		t.Fatal(err)
	}
	if len(art.Items) != 1 || art.Items[0].BodyDigest != digest {
		t.Fatalf("artifact digest not stamped from dispatch: %+v", art.Items)
	}

	// A report that wrote its own digest keeps it.
	written := &PerspectiveReport{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", BodyDigest: "own", Perspective: PerspectiveSecurity}
	stamps.apply(written)
	if written.BodyDigest != "own" {
		t.Fatalf("reviewer-written digest overwritten: %q", written.BodyDigest)
	}

	// A head dispatched under two digests cannot attribute a digestless
	// report to either; it is left as written.
	ambiguous := ModelStampsFromState(DispatchState{Pending: []PendingReview{
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", BodyDigest: "d1", Perspective: PerspectiveSecurity, Agent: "r1"},
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", BodyDigest: "d2", Perspective: PerspectiveCorrectness, Agent: "r1"},
	}})
	loose := &PerspectiveReport{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveSecurity}
	ambiguous.apply(loose)
	if loose.BodyDigest != "" {
		t.Fatalf("ambiguous head stamped a digest: %q", loose.BodyDigest)
	}
}
