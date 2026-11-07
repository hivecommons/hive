package automerge

import (
	"context"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/utah-packages#917, 2026-11-07 12:02–16:11Z: a fork PR with
// an approve verdict, parked at 085 on its fork-guarded red required Canary,
// went 085 → 080 → 085 about 15 times on one unchanged head. Each 080 came
// from a contributor-lane pass that could not take the destination-merge
// lock ("destination merge already in progress": another sweep was
// evaluating a PR into the same base). The lane returns that refusal as
// base-refresh:merge-guard WITH the error, so the observation is Err, and
// the classifier's lane-errored branch ran before the hand-off check:
// approve verdict + no CI reason = a weak 080, the merge train seated the
// PR as its front, and the next evaluated pass put it back at 085.
//
// The test replays that: evaluated passes and merge-guard passes (recorded
// exactly as the lane records them, error included) alternate, both inside
// and outside the 15-minute reversal guard and the 30-minute dampener
// window. The PR must stay at 085 with the fork-guard reason and 080 must
// never be written.
func TestMergeGuardRefusalKeepsForkGuardedPRAtNeedsDecision(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	const head = "7e52182c0"
	policy := StageLabelPolicy{Enabled: true, ForkGuard: func(repo string, number int, h string) []string {
		if h == head {
			return []string{"Canary"}
		}
		return nil
	}}
	e := newNudgeEngine(t, apiURLOf(t, api), clock, &fakeNudgeSink{}, policy)
	ctx := context.Background()
	labels := []string{"role/trusted", "author/clanker"}
	key := stageKey("acme", "widget", 917)
	pass := func(n int, mergeGuard bool) {
		t.Helper()
		api.reset()
		pr := nudgePR(917, head, labels...)
		pr.Head.Repo = &gh.Repository{FullName: gh.Ptr("kylerankin/widget")}
		e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", pr, "not-app-authored", false, nil)
		if mergeGuard {
			e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, basePreparationPrefix+"merge-guard", false, hgithub.ErrDestinationMergeBusy)
		} else {
			e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, "required-check-failing:Canary", false, nil)
			e.noteStageRole("acme", "widget", 917, stagelabels.RoleTrusted)
		}
		e.noteStageVerdict("acme", "widget", 917, ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"})
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
		for _, batch := range api.added {
			for _, l := range batch {
				if l == stagelabels.AwaitingMergeAdmission.Label() {
					t.Fatalf("pass %d (merge-guard=%v) wrote %s on an unchanged red fork-guarded head", n, mergeGuard, l)
				}
			}
		}
		labels = stageLabelsAfter(labels, api)
		if got := stageLabelOf(labels); got != stagelabels.NeedsDecision.Label() {
			t.Fatalf("pass %d (merge-guard=%v): stage %q, want %s", n, mergeGuard, got, stagelabels.NeedsDecision.Label())
		}
		if rec, _ := e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.NeedsDecision || rec.Reason != prstage.StageReasonForkGuard+"Canary" {
			t.Fatalf("pass %d (merge-guard=%v): record %s %q, want 085 %q", n, mergeGuard, rec.Stage, rec.Reason, prstage.StageReasonForkGuard+"Canary")
		}
	}
	pass(0, false)
	// Live gaps: the refusal, then the evaluated pass 2–3 minutes later,
	// then the next round trip 30–60 minutes on.
	gaps := []time.Duration{2 * time.Minute, 3 * time.Minute, 35 * time.Minute, 2 * time.Minute, 50 * time.Minute, 70 * time.Second, 70 * time.Second, 40 * time.Minute}
	for i, gap := range gaps {
		clock.advance(gap)
		pass(i+1, i%2 == 0)
	}
}

// The classifier view of the same refusal: a lane that could not take the
// destination-merge lock never looked at the PR, error or not, so it has
// no stage opinion even when every author-side gate looks satisfied.
func TestErroredMergeGuardIsNoStageOpinion(t *testing.T) {
	o := stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: prstage.StageReasonBaseRefreshMergeGuard, Err: true}
	o.Detail.VerdictKnown = true
	o.Detail.Verdict.Approved = true
	o.Detail.Verdict.Verdict = "approve"
	if v := classifyStage(o); v.Rank != stageRankNone {
		t.Fatalf("errored merge-guard classified %+v, want no opinion", v)
	}
}
