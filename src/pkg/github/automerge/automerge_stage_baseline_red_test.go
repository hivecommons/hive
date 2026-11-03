package automerge

import (
	"context"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// 2026-10-20: v5's own Docker build was red, so every PR rebased on it showed
// the same red checks and a one-file CSS PR was moved to 071 "waiting on the
// author". A red PR whose every failing check is also red on the base branch
// is not the author's to fix: it reads as CI pending (050/060) with the
// checks named; conflicts and requested changes stay author-facing.
func TestClassifyStageBaseBranchRedKeepsRedOutOfRepairStages(t *testing.T) {
	approve := stageDetail{Verdict: ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}, VerdictKnown: true}
	reject := stageDetail{Verdict: ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}, VerdictKnown: true}
	redOptional := approve
	redOptional.RedChecks, redOptional.RedChecksHead = []string{"docker (hub)"}, "sha"
	base := []string{"docker (hive)", "docker (hub)"}
	const want = "base-branch-red:docker (hive),docker (hub)"
	cases := []struct {
		name   string
		obs    stageObservation
		want   stagelabels.Stage
		reason string
	}{
		{name: "human red required", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "required-check-failing:docker (hive)", BaseRedChecks: base}, want: stagelabels.ReviewPending, reason: want},
		{name: "lane already names the base", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: want, BaseRedChecks: base}, want: stagelabels.ReviewPending, reason: want},
		{name: "trusted red approved", obs: stageObservation{Lane: stageLaneTrusted, HeadSHA: "sha", Reason: "check-failing:docker (hive)", Detail: approve, BaseRedChecks: base}, want: stagelabels.Reviewed, reason: want},
		{name: "self-authored red", obs: stageObservation{Lane: stageLaneSelf, HeadSHA: "sha", Reason: "status-failure", BaseRedChecks: base}, want: stagelabels.ReviewPending, reason: want},
		{name: "red optional while soaking", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: contributorLaneReasonSoaking + "(1h)", Detail: redOptional, BaseRedChecks: base}, want: stagelabels.Reviewed, reason: want},
		{name: "lane error with red optional", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "review-state-check", Err: true, Detail: redOptional, BaseRedChecks: base}, want: stagelabels.Reviewed, reason: want},
		{name: "pending CI is unchanged", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "required-check-pending:build", BaseRedChecks: base}, want: stagelabels.ReviewPending},
		{name: "conflicting stays author-facing", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "conflicting", BaseRedChecks: base}, want: stagelabels.NeedsAuthor},
		{name: "changes requested stays author-facing", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "required-check-failing:docker (hive)", Detail: reject, BaseRedChecks: base}, want: stagelabels.NeedsAuthor, reason: "reviewer-verdict:changes_requested"},
		{name: "no base evidence is red CI", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "required-check-failing:docker (hive)"}, want: stagelabels.NeedsAuthor},
		{name: "hive PR without base evidence is fixing", obs: stageObservation{Lane: stageLaneSelf, HeadSHA: "sha", Reason: "required-check-failing:docker (hive)"}, want: stagelabels.Fixing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyStage(tc.obs)
			if got.Stage != tc.want || got.Reason != tc.reason || got.Rank != stageRankOwned {
				t.Fatalf("classifyStage=%+v want owned stage %v reason %q", got, tc.want, tc.reason)
			}
		})
	}
}

func TestStageActivityBaseBranchRedWaitsOnHive(t *testing.T) {
	a := stageActivity(stageObservation{HeadSHA: "sha", Reason: "base-branch-red:docker (hive)"}, stagelabels.ReviewPending)
	if !a.WaitingOnHive {
		t.Fatal("base-branch-red does not wait on the hive")
	}
	if classifyCIReason("base-branch-red:docker (hive)") != ciReasonRed {
		t.Fatal("base-branch-red is still red CI (no 080, no merge)")
	}
}

// A base-red contributor PR stays at 050 across repeated sweeps — no 071/072,
// no nudge, no stall — and is handed back to its author only once the base
// recovers and the head is still red.
func TestApplyStageLabelsBaseBranchRedStableWithoutNudge(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 20, 21, 30, 0, 0, time.UTC)}
	sink := &fakeNudgeSink{outcome: ContributorNudgeOutcome{Mode: "spawn", Online: true}}
	baseRed := []string{"docker (hive)", "overlayfs-exec-guard"}
	var asked []string
	policy := StageLabelPolicy{Enabled: true, ContributorNudge: true, StalledRequireQuiet: true}
	policy.BaseBranchRed = func(repo string, number int, head string) []string {
		asked = append(asked, repo+"|"+head)
		if number != 7 {
			return nil
		}
		return baseRed
	}
	e := newNudgeEngine(t, apiURLOf(t, api), clock, sink, policy)
	ctx := context.Background()
	observe := func(pr *gh.PullRequest) {
		t.Helper()
		api.reset()
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, "required-check-failing:docker (hive)", false, nil)
		e.noteStageRole("acme", "widget", 7, stagelabels.RoleContributor)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}

	observe(nudgePR(7, "aaa"))
	if len(asked) == 0 || asked[0] != "acme/widget|aaa" {
		t.Fatalf("BaseBranchRed asked %v, want acme/widget|aaa", asked)
	}
	if !hasAdded(api, "stage/050-review-pending") || hasAdded(api, "stage/072-returned-to-contributor") || hasAdded(api, "stage/071-needs-author") {
		t.Fatalf("first sweep added=%v, want 050 and no 071/072", api.added)
	}
	if len(nudgeComments(api)) != 0 || len(sink.nudges) != 0 {
		t.Fatalf("base-red PR nudged: comments=%v sink=%v", api.comments, sink.nudges)
	}
	key := stageKey("acme", "widget", 7)
	if rec, _ := e.stageStoreOrShared().Get(key); rec.Reason != "base-branch-red:docker (hive),overlayfs-exec-guard" {
		t.Fatalf("stored reason %q", rec.Reason)
	}

	labels := []string{"stage/050-review-pending", "role/contributor", "author/clanker"}
	for i := 0; i < 4; i++ {
		clock.at = clock.at.Add(2 * time.Hour)
		observe(nudgePR(7, "aaa", labels...))
		if api.writes() != 0 || len(sink.nudges) != 0 {
			t.Fatalf("sweep %d: added=%v removed=%v comments=%v sink=%v", i+2, api.added, api.removed, api.comments, sink.nudges)
		}
	}

	// Base recovered and the head is still red: now it is the author's.
	baseRed = nil
	clock.at = clock.at.Add(time.Minute)
	observe(nudgePR(7, "aaa", labels...))
	if !hasAdded(api, "stage/072-returned-to-contributor") || len(nudgeComments(api)) != 1 || len(sink.nudges) != 1 {
		t.Fatalf("after recovery: added=%v comments=%v sink=%v", api.added, api.comments, sink.nudges)
	}
}
