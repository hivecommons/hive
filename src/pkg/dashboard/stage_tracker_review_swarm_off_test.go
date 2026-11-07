package dashboard

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A contributor-lane PR parked at 050 on a hive whose review swarm is off
// (review.require_approval / review.fan_out unset) has no reviewer in flight
// and never will: the Review track must not read "0/5 · in progress"
// indefinitely. Danathar/sensi#681 and eleven sibling PRs on hive-wild-mole
// sat at "Review 0/5 · in progress" with green CI and no dispatch at all.
func TestStageTrackerReviewPendingSwarmOffIsNotInProgress(t *testing.T) {
	dir := t.TempDir()
	origDispatch, origVerdicts := review.ReviewDispatchStatePath, review.ReviewVerdictsPath
	review.ReviewDispatchStatePath = filepath.Join(dir, review.ReviewDispatchStateFile)
	review.ReviewVerdictsPath = filepath.Join(dir, review.ReviewVerdictsFile)
	t.Cleanup(func() { review.ReviewDispatchStatePath, review.ReviewVerdictsPath = origDispatch, origVerdicts })

	now := time.Date(2026, 11, 7, 19, 0, 0, 0, time.UTC)
	head := "005509a63ec4ce4c3a3f03f3beb11aa86da7278d"
	observed := now.Add(-50 * time.Minute)
	build := func(cfg *config.Config) *StageTrackerActivity {
		t.Helper()
		s := newTestServer()
		s.deps = &Dependencies{Config: cfg}
		clock := now.Add(-time.Hour)
		store := stagelabels.NewStore("", func() time.Time { return clock })
		s.stageStore = store
		store.Observe(stagelabels.Key("acme/sensi", 681), stagelabels.ReviewPending, "contributor-lane:no-verdict")
		clock = now
		status := minimalPayload()
		status.HiveID = "hv-test"
		status.Repos = []FrontendRepo{{
			Name: "sensi", Full: "acme/sensi",
			OpenPrs: []any{FrontendPR{PullRequest: github.PullRequest{
				Repo: "acme/sensi", Number: 681, Title: "docs: rename L6", Author: "owner", HeadSHA: head,
				CIStatus: "success", Labels: []string{"stage/050-review-pending", "author/clanker"},
				CIActivity: &github.PRCIActivity{Head: head, Source: "check-runs", ObservedAt: &observed, Checks: []github.PRActivityCheck{
					{Name: "build", State: "completed", Conclusion: "success", Since: &observed, Head: head},
				}},
			}}},
		}}
		s.statusMu.Lock()
		s.status = status
		s.statusMu.Unlock()
		it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 681)
		if it.ReviewActivity == nil {
			t.Fatal("review_activity missing")
		}
		return it.ReviewActivity
	}

	off := build(&config.Config{})
	if off.State == "reviewing" || off.Active || strings.Contains(off.Caption, "in progress") {
		t.Fatalf("swarm off: review track = state %q caption %q active %v; nothing is reviewing this head", off.State, off.Caption, off.Active)
	}
	if off.State != reviewStateNotStarted || off.Caption != "Review · not started · needs reviewer" || !strings.Contains(off.Detail, "review swarm is off") {
		t.Fatalf("swarm off: review track = state %q caption %q detail %q; want a person-must-review statement", off.State, off.Caption, off.Detail)
	}

	// With the swarm on, the #16599 behaviour stands: the 050 row is under
	// review and the planner's pending / wait rows take over once written.
	on := &config.Config{}
	on.Review.RequireApproval, on.Review.FanOut = true, true
	if got := build(on); got.State != "reviewing" || got.Caption != "Review 0/5 · in progress" {
		t.Fatalf("swarm on: review track = state %q caption %q", got.State, got.Caption)
	}
}
