package automerge

import (
	"context"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#15825: items that have not passed the review or CI is
// not green should not be (a) ready for merge, (b) in a merge train.

// TestMergeTrainNotGreenOrReviewedIsNotMember: a PR at CI (check pending)
// or at Review (verdict owed), and one the lane has not judged at all,
// share the train's files but take no seat — no position, never front.
func TestMergeTrainNotGreenOrReviewedIsNotMember(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)
	api := &mergeTrainAPI{
		files: map[int][]string{15748: {"a.go"}, 15804: {"a.go"}, 15812: {"a.go"}, 15816: {"a.go"}},
		state: map[int]string{15748: "clean", 15804: "clean", 15812: "clean", 15816: "clean"},
	}
	srv := newMergeTrainServer(t, api)
	defer srv.Close()
	c, logs := newMergeTrainEngine(t, srv.URL, DefaultMergeTrainPolicy, nil)
	ciPending, ready, verdictOwed, fresh := mtPR(15748, t0, ""), mtPR(15804, t0.Add(time.Minute), ""), mtPR(15812, t0.Add(2*time.Minute), ""), mtPR(15816, t0.Add(3*time.Minute), "")
	c.noteMergeTrainOutcome("self", "acme", "widget", 15748, "sha15748", "required-check-pending:coverage")
	c.noteMergeTrainOutcome("self", "acme", "widget", 15804, "sha15804", "")
	c.noteMergeTrainOutcome("self", "acme", "widget", 15812, "sha15812", prstage.ContributorLaneReasonNoVerdict)
	prs := []*gh.PullRequest{ciPending, ready, verdictOwed, fresh}
	plan := plan0(c, prs)
	if !plan.isFront(15804) {
		t.Fatalf("the only green, reviewed head fronts: %+v", plan.groups)
	}
	for _, n := range []int{15748, 15812, 15816} {
		if plan.isFront(n) || plan.isSibling(n) {
			t.Errorf("#%d is not green / reviewed on its head and must hold no seat: %+v", n, plan.groups)
		}
		if !plan.isCandidate(n) {
			t.Errorf("#%d shares the train's files and is a candidate", n)
		}
		if g, _ := plan.candidateOf(n); g.Position(n) != 0 {
			t.Errorf("#%d position = %d, want none", n, g.Position(n))
		}
	}
	if g, _ := plan.group(15804); len(g.Order) != 1 || len(g.Candidates) != 3 {
		t.Fatalf("order = %v candidates = %v", g.Order, g.Candidates)
	}
	if strings.Contains(logs.String(), "member ejected") {
		t.Fatalf("nobody held a seat; nothing to eject:\n%s", logs.String())
	}
	// A candidate the lane finds ready is held for a seat, not merged
	// ahead of the front; the hold reads as ready next pass.
	reason := c.mergeTrainCandidateReason(plan, 15816)
	mt, ok := prstage.ParseMergeTrain(reason)
	if !ok || mt.State != prstage.MergeTrainStateAdmitted || mt.Front != 15804 || mt.Position != 0 || mt.Size != 1 {
		t.Fatalf("candidate reason = %q (%+v)", reason, mt)
	}
	if got := mergeTrainCandidateHold(withMergeTrainCandidateHold(context.Background(), reason)); got != reason {
		t.Fatalf("hold from ctx = %q, want %q", got, reason)
	}
	if mergeTrainCandidateHold(context.Background()) != "" || c.mergeTrainCandidateReason(plan, 15804) != "" {
		t.Fatal("a seated member or a plain context carries no hold")
	}
	if mergeTrainReadinessOf(reason) != mergeTrainReadinessReady {
		t.Fatal("an admitted candidate reads as ready for the next plan")
	}
	c.noteMergeTrainOutcome("self", "acme", "widget", 15816, "sha15816", reason)
	if plan := plan0(c, prs); !plan.isFront(15804) || !plan.isSibling(15816) {
		t.Fatalf("the admitted candidate takes the seat behind the front: %+v", plan.groups)
	}
}

// TestMergeTrainFrontEjectedOnHeadMoved: the front receives a push; CI
// restarts and the verdicts are owed again. On the next evaluation it is
// ejected (logged once, recorded on the stage store) and the next ready
// member fronts.
func TestMergeTrainFrontEjectedOnHeadMoved(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)
	api := &mergeTrainAPI{
		files: map[int][]string{15748: {"a.go"}, 15757: {"a.go"}, 15804: {"a.go"}},
		state: map[int]string{15748: "clean", 15757: "clean", 15804: "clean"},
		heads: map[int]string{},
	}
	srv := newMergeTrainServer(t, api)
	defer srv.Close()
	c, logs := newMergeTrainEngine(t, srv.URL, DefaultMergeTrainPolicy, nil)
	now := t0
	c.mergeTrainNow = func() time.Time { return now }
	c.stageStore = stagelabels.NewStore("", func() time.Time { return now })
	for _, n := range []int{15748, 15757, 15804} {
		c.stageStore.Observe(stagelabels.Key("acme/widget", n), stagelabels.AwaitingMergeAdmission, "")
	}
	prs := []*gh.PullRequest{mtPR(15748, t0, ""), mtPR(15757, t0.Add(time.Minute), ""), mtPR(15804, t0.Add(2*time.Minute), "")}
	mtReady(c, "self", prs...)
	if plan := plan0(c, prs); !plan.isFront(15748) || !plan.isSibling(15757) || !plan.isSibling(15804) {
		t.Fatalf("initial train: %+v", plan.groups)
	}
	// The push: a new head the lane has not judged.
	now = now.Add(5 * time.Minute)
	api.mu.Lock()
	api.heads[15748] = "sha15748b"
	api.mu.Unlock()
	prs[0].Head.SHA = gh.Ptr("sha15748b")
	plan := plan0(c, prs)
	if plan.isFront(15748) || plan.isSibling(15748) {
		t.Fatalf("a pushed front holds no seat: %+v", plan.groups)
	}
	if !plan.isFront(15757) || !plan.isSibling(15804) || !plan.isCandidate(15748) {
		t.Fatalf("the next ready member fronts, the pushed one is a candidate: %+v", plan.groups)
	}
	want := "msg=\"merge train: member ejected\" repo=acme/widget pr=15748 why=head-moved lane=self"
	if n := strings.Count(logs.String(), "member ejected"); n != 1 || !strings.Contains(logs.String(), want) {
		t.Fatalf("ejection logged %d times, want once as %q:\n%s", n, want, logs.String())
	}
	rec, _ := c.stageStore.Get(stagelabels.Key("acme/widget", 15748))
	if why, head, at, ok := rec.TrainEjected(); !ok || why != mergeTrainEjectHeadMoved || head != "sha15748b" || !at.Equal(now) {
		t.Fatalf("store ejection = %q %q %v %v", why, head, at, ok)
	}
	// The same pass again: no second line.
	plan0(c, prs)
	if n := strings.Count(logs.String(), "member ejected"); n != 1 {
		t.Fatalf("ejection logged %d times, want once", n)
	}
	// CI still running on the new head: still out, no new ejection.
	c.noteMergeTrainOutcome("self", "acme", "widget", 15748, "sha15748b", "check-pending")
	if plan := plan0(c, prs); plan.isFront(15748) || plan.isSibling(15748) || !plan.isFront(15757) {
		t.Fatalf("CI pending on the new head: still a candidate: %+v", plan.groups)
	}
	if n := strings.Count(logs.String(), "member ejected"); n != 1 {
		t.Fatalf("ejection logged %d times, want once", n)
	}
}

// TestMergeTrainEjectedReentersAtBack: the ejected PR turns green and
// reviewed again and takes the seat at the back, behind the members that
// kept theirs; its ejection record clears.
func TestMergeTrainEjectedReentersAtBack(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)
	api := &mergeTrainAPI{
		files: map[int][]string{15748: {"a.go"}, 15757: {"a.go"}, 15804: {"a.go"}},
		state: map[int]string{15748: "clean", 15757: "clean", 15804: "clean"},
		heads: map[int]string{},
	}
	srv := newMergeTrainServer(t, api)
	defer srv.Close()
	c, _ := newMergeTrainEngine(t, srv.URL, DefaultMergeTrainPolicy, nil)
	now := t0
	c.mergeTrainNow = func() time.Time { return now }
	c.stageStore = stagelabels.NewStore("", func() time.Time { return now })
	for _, n := range []int{15748, 15757, 15804} {
		c.stageStore.Observe(stagelabels.Key("acme/widget", n), stagelabels.AwaitingMergeAdmission, "")
	}
	prs := []*gh.PullRequest{mtPR(15748, t0, ""), mtPR(15757, t0.Add(time.Minute), ""), mtPR(15804, t0.Add(2*time.Minute), "")}
	mtReady(c, "self", prs...)
	plan0(c, prs)
	now = now.Add(5 * time.Minute)
	api.mu.Lock()
	api.heads[15748] = "sha15748b"
	api.mu.Unlock()
	prs[0].Head.SHA = gh.Ptr("sha15748b")
	c.noteMergeTrainOutcome("self", "acme", "widget", 15748, "sha15748b", "check-pending")
	if plan := plan0(c, prs); !plan.isCandidate(15748) {
		t.Fatalf("pushed front is a candidate: %+v", plan.groups)
	}
	// Green and reviewed again on the new head: the lane would merge it,
	// but it is a candidate of the train — held, then seated at the back.
	now = now.Add(10 * time.Minute)
	plan := plan0(c, prs)
	c.noteMergeTrainOutcome("self", "acme", "widget", 15748, "sha15748b", c.mergeTrainCandidateReason(plan, 15748))
	plan = plan0(c, prs)
	g, ok := plan.group(15748)
	if !ok || g.Front() != 15757 || g.Position(15804) != 2 || g.Position(15748) != 3 {
		t.Fatalf("re-entry order = %v, want [15757 15804 15748]", plan.groups)
	}
	rec, _ := c.stageStore.Get(stagelabels.Key("acme/widget", 15748))
	if _, _, _, ejected := rec.TrainEjected(); ejected {
		t.Fatal("a seated member carries no ejection")
	}
	// Age alone never beats a seat: #15748 is the oldest and still last.
	if g.Order[0] != 15757 {
		t.Fatalf("front = %d, want 15757", g.Order[0])
	}
}

// TestMergeTrainHeadReadinessIgnoresStaleStage: the stage store's 080 for
// an earlier head says nothing about the pushed one.
func TestMergeTrainHeadReadinessIgnoresStaleStage(t *testing.T) {
	c := &Engine{}
	c.stageStore = stagelabels.NewStore("", time.Now)
	key := stagelabels.Key("acme/widget", 7)
	c.stageStore.Observe(key, stagelabels.AwaitingMergeAdmission, "")
	c.stageStore.NoteActivity(key, stagelabels.Activity{Head: "old", Comments: -1, Reviews: -1})
	if got := c.mergeTrainHeadReadiness("self", "acme", "widget", 7, "old"); got != mergeTrainReadinessReady {
		t.Fatalf("readiness at the recorded head = %d, want ready", got)
	}
	if got := c.mergeTrainHeadReadiness("self", "acme", "widget", 7, "new"); got != mergeTrainReadinessUnknown {
		t.Fatalf("readiness at a pushed head = %d, want unknown", got)
	}
}
