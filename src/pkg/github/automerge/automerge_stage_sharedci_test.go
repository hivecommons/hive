package automerge

import (
	"context"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#12606: a red PR deferred to an open shared-CI incident is
// not the author's to fix. It reads as CI pending (050/060) with the incident
// named; conflicts and requested changes stay author-facing.
func TestClassifyStageSharedCIIncidentKeepsRedOutOfRepairStages(t *testing.T) {
	approve := stageDetail{Verdict: ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}, VerdictKnown: true}
	reject := stageDetail{Verdict: ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}, VerdictKnown: true}
	redOptional := approve
	redOptional.RedChecks, redOptional.RedChecksHead = []string{"lint"}, "sha"
	const incident = "shared-ci-incident:4242"
	cases := []struct {
		name   string
		obs    stageObservation
		want   stagelabels.Stage
		reason string
	}{
		{name: "contributor red required", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "required-check-failing:build", SharedCIIncident: 4242}, want: stagelabels.ReviewPending, reason: incident},
		{name: "trusted red approved", obs: stageObservation{Lane: stageLaneTrusted, HeadSHA: "sha", Reason: "check-failing:unit", Detail: approve, SharedCIIncident: 4242}, want: stagelabels.Reviewed, reason: incident},
		{name: "self-authored red", obs: stageObservation{Lane: stageLaneSelf, HeadSHA: "sha", Reason: "status-failure", SharedCIIncident: 4242}, want: stagelabels.ReviewPending, reason: incident},
		{name: "red optional while soaking", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: contributorLaneReasonSoaking + "(1h)", Detail: redOptional, SharedCIIncident: 4242}, want: stagelabels.Reviewed, reason: incident},
		{name: "lane error with red optional", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "review-state-check", Err: true, Detail: redOptional, SharedCIIncident: 4242}, want: stagelabels.Reviewed, reason: incident},
		{name: "pending CI is unchanged", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "required-check-pending:build", SharedCIIncident: 4242}, want: stagelabels.ReviewPending},
		{name: "conflicting stays author-facing", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "conflicting", SharedCIIncident: 4242}, want: stagelabels.NeedsAuthor},
		{name: "changes requested stays author-facing", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "required-check-failing:build", Detail: reject, SharedCIIncident: 4242}, want: stagelabels.NeedsAuthor, reason: "reviewer-verdict:changes_requested"},
		{name: "no incident is red CI", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "required-check-failing:build"}, want: stagelabels.NeedsAuthor},
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

func TestStageActivitySharedCIAndBudgetWaitOnHive(t *testing.T) {
	for _, reason := range []string{"shared-ci-incident:4242", autoMergeReasonBehindBudgetExhausted} {
		if a := stageActivity(stageObservation{HeadSHA: "sha", Reason: reason}, stagelabels.ReviewPending); !a.WaitingOnHive {
			t.Fatalf("%q does not wait on the hive", reason)
		}
	}
	if a := stageActivity(stageObservation{HeadSHA: "sha", Reason: "conflicting"}, stagelabels.ReturnedToContributor); a.WaitingOnHive {
		t.Fatal("conflicting waits on the hive")
	}
}

// A base-red clanker PR stays at 050 across repeated sweeps — no 072, no
// nudge comment or relay interrupt, no stall — and is handed back to its
// author with a nudge only once the incident closes and the head is still
// red.
func TestApplyStageLabelsSharedCIIncidentStableWithoutNudge(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)}
	sink := &fakeNudgeSink{outcome: ContributorNudgeOutcome{Mode: "spawn", Online: true}}
	open := 4242
	var asked []string
	policy := StageLabelPolicy{Enabled: true, ContributorNudge: true, StalledRequireQuiet: true}
	policy.SharedCIIncident = func(repo string, number int, head string) int {
		asked = append(asked, repo+"|"+head)
		if number != 7 {
			return 0
		}
		return open
	}
	e := newNudgeEngine(t, apiURLOf(t, api), clock, sink, policy)
	ctx := context.Background()
	observe := func(pr *gh.PullRequest) {
		t.Helper()
		api.reset()
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, "required-check-failing:build", false, nil)
		e.noteStageRole("acme", "widget", 7, stagelabels.RoleContributor)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}

	observe(nudgePR(7, "aaa"))
	if len(asked) == 0 || asked[0] != "acme/widget|aaa" {
		t.Fatalf("SharedCIIncident asked %v, want acme/widget|aaa", asked)
	}
	if !hasAdded(api, "stage/050-review-pending") || hasAdded(api, "stage/072-returned-to-contributor") {
		t.Fatalf("first sweep added=%v, want 050 and no 072", api.added)
	}
	if len(nudgeComments(api)) != 0 || len(sink.nudges) != 0 {
		t.Fatalf("base-red PR nudged: comments=%v sink=%v", api.comments, sink.nudges)
	}
	key := stageKey("acme", "widget", 7)
	if rec, _ := e.stageStoreOrShared().Get(key); rec.Reason != "shared-ci-incident:4242" {
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

	// Incident closed and the head is still red: now it is the author's.
	open = 0
	clock.at = clock.at.Add(time.Minute)
	observe(nudgePR(7, "aaa", labels...))
	if !hasAdded(api, "stage/072-returned-to-contributor") || len(nudgeComments(api)) != 1 || len(sink.nudges) != 1 {
		t.Fatalf("after recovery: added=%v comments=%v sink=%v", api.added, api.comments, sink.nudges)
	}
}

func hasAdded(api *stageAPI, label string) bool {
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, batch := range api.added {
		for _, l := range batch {
			if strings.EqualFold(l, label) {
				return true
			}
		}
	}
	return false
}
