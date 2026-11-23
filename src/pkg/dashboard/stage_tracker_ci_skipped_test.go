package dashboard

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// A check whose job is skipped by its own `if:` condition (a Pages deploy
// job gated on `github.event_name != 'pull_request'`) completes as skipped
// on every PR. The merge gate does not block on it; the CI track counted
// it as unfinished and, past the stuck threshold, labeled it stuck
// ("CI 1/2 · 'deploy' stuck") while the hive's own status said 2/2 green.

// skippedFixtureAge is how long ago the fixture's checks started: past the
// stuck threshold, so a check wrongly left pending would be called stuck.
const skippedFixtureAge = trackerCheckStuckAfterDefault + time.Hour

func skippedFixtureCheck(name, state, conclusion string, since time.Time) github.PRActivityCheck {
	at := since
	return github.PRActivityCheck{Name: name, Head: "head", State: state, Conclusion: conclusion, Since: &at}
}

// Required set unknown (the benchmarks case): every check on the head counts.
func TestStageTrackerCIHeadChecksSkippedCountsAsPassed(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	started := now.Add(-skippedFixtureAge)
	pr := func(deployState, deployConclusion string) github.PullRequest {
		return github.PullRequest{HeadSHA: "head", CIActivity: &github.PRCIActivity{
			Head: "head", Source: "github-check-runs", ObservedAt: &now, StatusesKnown: true,
			Checks: []github.PRActivityCheck{
				skippedFixtureCheck("build", "completed", "success", started),
				skippedFixtureCheck("deploy", deployState, deployConclusion, started),
			},
		}}
	}
	for _, tc := range []struct {
		name, state, conclusion string
		passed, failed          int
		pending, stuck, skipped []string
	}{
		{name: "skipped", state: "completed", conclusion: github.CheckConclusionSkipped, passed: 2, skipped: []string{"deploy"}},
		{name: "neutral", state: "completed", conclusion: github.CheckConclusionNeutral, passed: 2, skipped: []string{"deploy"}},
		{name: "cancelled", state: "completed", conclusion: "cancelled", passed: 1, failed: 1},
		{name: "pending", state: "in_progress", passed: 1, pending: []string{"deploy"}, stuck: []string{"deploy"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := stageTrackerCIActivity(pr(tc.state, tc.conclusion), now)
			if got.ChecksTotal != 2 || got.ChecksPassed != tc.passed || got.ChecksFailed != tc.failed {
				t.Fatalf("tally = %d passed / %d failed of %d, want %d / %d of 2: %+v", got.ChecksPassed, got.ChecksFailed, got.ChecksTotal, tc.passed, tc.failed, got)
			}
			if !slices.Equal(got.ChecksPending, tc.pending) || !slices.Equal(got.ChecksStuck, tc.stuck) || !slices.Equal(got.ChecksSkipped, tc.skipped) {
				t.Fatalf("pending=%v stuck=%v skipped=%v, want %v %v %v", got.ChecksPending, got.ChecksStuck, got.ChecksSkipped, tc.pending, tc.stuck, tc.skipped)
			}
			if len(tc.skipped) > 0 {
				if got.State == "running" || got.Active || strings.Contains(got.Caption, "stuck") {
					t.Fatalf("a completed skipped check read as motion or stuck: %+v", got)
				}
				if !strings.Contains(got.Detail, "'deploy' finished as skipped or neutral") {
					t.Fatalf("detail does not name the skipped check: %q", got.Detail)
				}
			}
		})
	}
}

// Required set known: a required check that finished skipped or neutral is
// a pass, as the merge gate and branch protection treat it; cancelled is
// red; an unfinished one is pending.
func TestStageTrackerCIRequiredSkippedMatchesMergeGate(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	started := now.Add(-skippedFixtureAge)
	pr := func(deployState, deployConclusion string) github.PullRequest {
		return github.PullRequest{HeadSHA: "head", CIActivity: &github.PRCIActivity{
			Head: "head", Source: "github-check-runs", ObservedAt: &now, StatusesKnown: true,
			RequiredKnown: true, RequiredSource: "protection", Required: []string{"build", "deploy"},
			RequiredChecks: []github.PRRequiredCheck{{Name: "build", AppKnown: true}, {Name: "deploy", AppKnown: true}},
			Checks: []github.PRActivityCheck{
				skippedFixtureCheck("build", "completed", "success", started),
				skippedFixtureCheck("deploy", deployState, deployConclusion, started),
			},
		}}
	}
	for _, tc := range []struct {
		name, state, conclusion, want string
		passed                        int
		pending, stuck, skipped       []string
	}{
		{name: "skipped", state: "completed", conclusion: github.CheckConclusionSkipped, want: "passing", passed: 2, skipped: []string{"deploy"}},
		{name: "neutral", state: "completed", conclusion: github.CheckConclusionNeutral, want: "passing", passed: 2, skipped: []string{"deploy"}},
		{name: "cancelled", state: "completed", conclusion: "cancelled", want: "failing", passed: 1},
		{name: "pending", state: "queued", want: "queued", passed: 1, pending: []string{"deploy"}, stuck: []string{"deploy"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := stageTrackerCIActivity(pr(tc.state, tc.conclusion), now)
			if got.State != tc.want || got.ChecksTotal != 2 || got.ChecksPassed != tc.passed {
				t.Fatalf("state %s %d/%d, want %s %d/2: %+v", got.State, got.ChecksPassed, got.ChecksTotal, tc.want, tc.passed, got)
			}
			if !slices.Equal(got.ChecksPending, tc.pending) || !slices.Equal(got.ChecksStuck, tc.stuck) || !slices.Equal(got.ChecksSkipped, tc.skipped) {
				t.Fatalf("pending=%v stuck=%v skipped=%v, want %v %v %v", got.ChecksPending, got.ChecksStuck, got.ChecksSkipped, tc.pending, tc.stuck, tc.skipped)
			}
			if tc.want == "passing" && got.Caption != "CI green · all 2 required checks passed" {
				t.Fatalf("caption = %q", got.Caption)
			}
		})
	}
}

// The tracker's notion of a non-blocking conclusion is the merge gate's.
func TestStageTrackerCINonBlockingConclusionsMatchMergeGate(t *testing.T) {
	for conclusion, want := range map[string]bool{
		"success": false, "skipped": true, "neutral": true, "cancelled": false,
		"failure": false, "timed_out": false, "action_required": false, "": false,
	} {
		if got := github.CheckConclusionNonBlocking(conclusion); got != want {
			t.Fatalf("CheckConclusionNonBlocking(%q) = %v, want %v", conclusion, got, want)
		}
		if github.CheckConclusionNonBlocking(conclusion) && github.CheckConclusionIsRed(conclusion) {
			t.Fatalf("%q is both non-blocking and red", conclusion)
		}
	}
	if !github.CheckConclusionIsRed("cancelled") {
		t.Fatal("cancelled must stay red, as the merge gate blocks on it")
	}
}
