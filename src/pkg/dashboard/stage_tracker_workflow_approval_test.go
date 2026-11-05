package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// A dependabot head whose 10 unreported required checks are workflow runs
// GitHub holds behind its approve-workflows gate reads as waiting on a
// person, not as "hasn't reported" (hivecommons/hive#16599).
func TestStageTrackerCIActivityWorkflowApproval(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	seen := now.Add(-2 * time.Hour)
	required := []string{"verify", "build", "lint"}
	activity := func(wa *github.PRWorkflowApproval) github.PullRequest {
		return github.PullRequest{HeadSHA: "head", CIActivity: &github.PRCIActivity{
			Head: "head", Source: "github-check-runs", ObservedAt: &now, RequiredKnown: true, Required: required, StatusesKnown: true,
			RequiredSource:   "protection",
			RequiredChecks:   []github.PRRequiredCheck{{Name: "verify", AppKnown: true}, {Name: "build", AppKnown: true}, {Name: "lint", AppKnown: true}},
			Checks:           []github.PRActivityCheck{{Name: "verify", State: "completed", Conclusion: "success"}},
			WorkflowApproval: wa,
		}}
	}
	for _, tc := range []struct {
		name        string
		wa          *github.PRWorkflowApproval
		wantBlocker string
		wantCaption string
		wantActor   string
	}{
		{"gated", &github.PRWorkflowApproval{Pending: 10, RunIDs: []int64{1}, Actor: "dependabot[bot]", ObservedAt: &seen},
			"workflow-approval-required:10", "CI waiting · 10 workflow runs need approval on GitHub", "dependabot[bot]"},
		{"single gated run", &github.PRWorkflowApproval{Pending: 1}, "workflow-approval-required:1", "CI waiting · 1 workflow run needs approval on GitHub", ""},
		// The hive's own approval was refused (no actions: write): the
		// caption says the click is a person's (hivecommons/hive#16603).
		{"approval refused", &github.PRWorkflowApproval{Pending: 3, PermissionDenied: true}, "workflow-approval-required:3", "CI waiting · needs workflow approval on GitHub", ""},
		{"no gate", nil, "build", "hasn't reported for this head yet", ""},
		{"empty record", &github.PRWorkflowApproval{}, "build", "hasn't reported for this head yet", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := stageTrackerCIActivity(activity(tc.wa), now)
			if got.State != "missing" || got.Active {
				t.Fatalf("state=%q active=%v", got.State, got.Active)
			}
			if !strings.HasPrefix(got.Blocker, strings.TrimSuffix(tc.wantBlocker, "build")) || !strings.Contains(got.Caption, tc.wantCaption) {
				t.Fatalf("blocker=%q caption=%q", got.Blocker, got.Caption)
			}
			if got.Actor != tc.wantActor {
				t.Fatalf("actor=%q want %q", got.Actor, tc.wantActor)
			}
			if got.ChecksPassed != 1 || got.ChecksTotal != 3 {
				t.Fatalf("counts %d/%d", got.ChecksPassed, got.ChecksTotal)
			}
			if tc.wa != nil && tc.wa.Pending > 0 {
				if !strings.Contains(got.Detail, "approve workflows") {
					t.Fatalf("detail=%q", got.Detail)
				}
				if tc.wa.ObservedAt != nil && (got.Since == nil || !got.Since.Equal(seen)) {
					t.Fatalf("since=%v", got.Since)
				}
			}
		})
	}
}

// A PR at review pending (050) with no settled verdict is under review:
// "reviewing" with the posted-perspective count, never "unknown"; without
// a head bundle nothing has posted (hivecommons/hive#16599).
func TestStageTrackerReviewPendingState(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                 string
		in                   StageTrackerActivity
		stage                int
		wantState, wantCap   string
		wantDone, wantActive int
	}{
		{"no evidence at 050", StageTrackerActivity{State: "unknown", Source: "unavailable", PerspectivesDone: 5, PerspectivesTotal: 5}, 50, "reviewing", "Review 0/5 · in progress", 0, 1},
		{"partial bundle at 050", StageTrackerActivity{State: "unknown", Source: "verified-review-bundle", PerspectivesDone: 1, PerspectivesTotal: 5}, 50, "reviewing", "Review 1/5 · in progress", 1, 1},
		{"all posted, no aggregate", StageTrackerActivity{State: "unknown", Source: "verified-review-bundle", PerspectivesDone: 5, PerspectivesTotal: 5}, 50, "adjudicating", "Review 5/5 · verdicts in · aggregating", 5, 1},
		{"not at 050", StageTrackerActivity{State: "unknown", Source: "unavailable", PerspectivesDone: 2, PerspectivesTotal: 5}, 30, "unknown", "", 2, 0},
		{"settled verdict untouched", StageTrackerActivity{State: "approved", Caption: "Review approved", PerspectivesDone: 5, PerspectivesTotal: 5}, 50, "approved", "Review approved", 5, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := tc.in
			stageTrackerReviewPendingState(&out, tc.stage, &now)
			if out.State != tc.wantState || out.PerspectivesDone != tc.wantDone || out.Active != (tc.wantActive == 1) {
				t.Fatalf("got %+v", out)
			}
			if tc.wantCap != "" && out.Caption != tc.wantCap {
				t.Fatalf("caption=%q want %q", out.Caption, tc.wantCap)
			}
			if tc.wantActive == 1 && (out.ObservedAt == nil || !out.ObservedAt.Equal(now) && tc.in.ObservedAt == nil || !strings.Contains(out.Source, "stage-review-pending")) {
				t.Fatalf("observed=%v source=%q", out.ObservedAt, out.Source)
			}
		})
	}
}
