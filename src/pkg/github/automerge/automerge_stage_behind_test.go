package automerge

import (
	"context"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/utah#1072: a contributor PR whose only lane objection is
// trust, every check green, but GitHub reports the head BEHIND a `main`
// that requires up-to-date branches. The lane's fresh PR fetch carries the
// mergeable_state to the stage pass, and the 090 comment names the base
// merge instead of "Blocked only by".
func TestReadyCommentNamesBehindBaseFromLaneFetch(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 6, 15, 0, 0, 0, time.UTC)}
	verdict := &ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
	e, store := humanStageEngine(t, api.url, clock, verdict, time.Hour)
	ctx := context.Background()
	sweep := func(state string, labels ...string) {
		t.Helper()
		api.reset()
		pr := stagePR(9, labels...)
		pr.Base = &gh.PullRequestBranch{Ref: gh.Ptr("main")}
		pr.MergeableState = gh.Ptr(state)
		e.noteStagePR("acme", "widget", 9, pr)
		e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", pr, "untrusted-author-role", false, nil)
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonClaimTier, false, nil)
		e.noteStageRole("acme", "widget", 9, stagelabels.RoleContributor)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}
	sweep("behind")
	clock.at = clock.at.Add(time.Hour) // soak elapsed
	sweep("behind", "role/contributor", "author/human", "stage/060-reviewed")
	if got := stageOf(t, store, 9); got != stagelabels.ReadyForHumanMerge {
		t.Fatalf("stage = %v, want 090: behind does not change the stage", got)
	}
	if len(api.comments) != 1 || !strings.HasPrefix(api.comments[0], stagelabels.ReadyCommentMarker) {
		t.Fatalf("comments = %q, want one 090 entry comment", api.comments)
	}
	got := api.comments[0]
	for _, want := range []string{"3/3 checks green, DCO yes, behind `main`", "Blocked by `role/contributor` (below require_tiers).", "GitHub will not merge this head until `main` is merged into it."} {
		if !strings.Contains(got, want) {
			t.Fatalf("comment lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Blocked only by") || strings.Contains(got, "(or merge)") {
		t.Fatalf("comment still calls trust the only blocker:\n%s", got)
	}
}

// The mergeable_state a lane read is only evidence for the head it was read
// on: a state noted for an older head never reaches the observation of a
// newer one, and a clean state is not "behind".
func TestHumanBehindBaseOnlyForTheFetchedHead(t *testing.T) {
	tracker := NewStageTracker()
	e := New(nil, Options{StageLabelPolicy: func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} }, StageTracker: tracker})
	old := stagePR(7)
	old.Head.SHA = gh.Ptr("old")
	old.MergeableState = gh.Ptr("behind")
	e.noteStagePR("acme", "widget", 7, old)
	tracker.record(stageObservation{Lane: stageLaneContributor, Owner: "acme", Repo: "widget", Number: 7, HeadSHA: "new", BaseRef: "main"})
	cur := stagePR(8)
	cur.MergeableState = gh.Ptr("BEHIND")
	e.noteStagePR("acme", "widget", 8, cur)
	tracker.record(stageObservation{Lane: stageLaneContributor, Owner: "acme", Repo: "widget", Number: 8, HeadSHA: "sha", BaseRef: "main"})
	clean := stagePR(10)
	clean.MergeableState = gh.Ptr("clean")
	e.noteStagePR("acme", "widget", 10, clean)
	tracker.record(stageObservation{Lane: stageLaneContributor, Owner: "acme", Repo: "widget", Number: 10, HeadSHA: "sha", BaseRef: "main"})
	want := map[int]string{7: "", 8: "main", 10: ""}
	for _, list := range tracker.drain() {
		for _, obs := range list {
			if got := humanBehindBase(obs); got != want[obs.Number] {
				t.Fatalf("pr %d: humanBehindBase = %q, want %q (detail %+v)", obs.Number, got, want[obs.Number], obs.Detail)
			}
		}
	}
}
