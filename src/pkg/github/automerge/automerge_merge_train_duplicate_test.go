package automerge

import (
	"context"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

// One PR per issue in the merge queue (hivecommons/hive#15395): green
// beats red, earlier beats later, the later-opened duplicate is held.
func TestMergeTrainDuplicateWinner(t *testing.T) {
	t0 := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	cand := func(n int, min int, green bool) mergeTrainDuplicateCandidate {
		return mergeTrainDuplicateCandidate{Number: n, CreatedAt: t0.Add(time.Duration(min) * time.Minute), Green: green}
	}
	for _, tc := range []struct {
		name  string
		cands []mergeTrainDuplicateCandidate
		want  int
	}{
		{"both green: the earlier wins", []mergeTrainDuplicateCandidate{cand(15402, 0, true), cand(15407, 30, true)}, 15402},
		{"both green, listed later first: still the earlier", []mergeTrainDuplicateCandidate{cand(15407, 30, true), cand(15402, 0, true)}, 15402},
		{"earlier red, later green: the later wins", []mergeTrainDuplicateCandidate{cand(15402, 0, false), cand(15407, 30, true)}, 15407},
		{"earlier green, later red: the earlier wins", []mergeTrainDuplicateCandidate{cand(15402, 0, true), cand(15407, 30, false)}, 15402},
		{"both red: the earlier wins", []mergeTrainDuplicateCandidate{cand(15402, 0, false), cand(15407, 30, false)}, 15402},
		{"same instant: the lower number", []mergeTrainDuplicateCandidate{cand(15407, 0, true), cand(15402, 0, true)}, 15402},
		{"three: the earliest green", []mergeTrainDuplicateCandidate{cand(15402, 0, false), cand(15407, 30, true), cand(15410, 60, true)}, 15407},
		{"one candidate", []mergeTrainDuplicateCandidate{cand(15402, 0, false)}, 15402},
		{"none", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mergeTrainDuplicateWinner(tc.cands); got != tc.want {
				t.Fatalf("winner = #%d, want #%d", got, tc.want)
			}
		})
	}
}

func TestMergeTrainDuplicateHolds(t *testing.T) {
	t0 := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	byIssue := map[int][]mergeTrainDuplicateCandidate{
		15396: {{Number: 15402, CreatedAt: t0, Green: true}, {Number: 15407, CreatedAt: t0.Add(time.Hour), Green: true}},
		15300: {{Number: 15320, CreatedAt: t0, Green: false}, {Number: 15330, CreatedAt: t0.Add(time.Hour), Green: true}},
		15999: {{Number: 15350, CreatedAt: t0, Green: true}},
	}
	got := mergeTrainDuplicateHolds(byIssue)
	want := []mergeTrainDuplicateHold{{Number: 15320, Winner: 15330, Issue: 15300}, {Number: 15407, Winner: 15402, Issue: 15396}}
	if len(got) != len(want) {
		t.Fatalf("holds = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hold[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	reason := mergeTrainDuplicateReason(want[1])
	if reason != "merge-train:duplicate-of#15402:15396/0:0/0:" {
		t.Fatalf("reason = %q", reason)
	}
	m, ok := prstage.ParseMergeTrain(reason)
	if !ok || m.State != prstage.MergeTrainStateDuplicateOf || m.Front != 15402 || m.Position != 15396 {
		t.Fatalf("parse = %+v ok=%v", m, ok)
	}
	if s := m.Sentence(); s != "duplicate of #15402 for issue #15396; superseded by #15402 — the merge queue carries one PR per issue; close it or let the delegate decide" {
		t.Fatalf("sentence = %q", s)
	}
}

// planDuplicateHolds removes the held PR from the candidates the trains are
// grouped from and remembers who it yields to; a PR with no closing
// keyword, or the only PR for its issue, is untouched.
func TestPlanDuplicateHoldsFiltersCandidates(t *testing.T) {
	c := &Engine{}
	t0 := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	pr := func(n int, body string, at time.Time, state string) *gh.PullRequest {
		return &gh.PullRequest{Number: gh.Int(n), Title: gh.String("t"), Body: gh.String(body), CreatedAt: &gh.Timestamp{Time: at}, MergeableState: gh.String(state), Head: &gh.PullRequestBranch{SHA: gh.String("sha")}}
	}
	prs := []*gh.PullRequest{
		pr(15402, "Fixes #15396", t0, "dirty"),
		pr(15407, "Fixes #15396", t0.Add(time.Hour), "clean"),
		pr(15410, "Refs #15396", t0, "clean"),
		pr(15411, "Fixes #15397", t0, "clean"),
	}
	plan := &mergeTrainPlan{lane: "self", displayRepo: "o/r"}
	kept := c.planDuplicateHolds(context.Background(), plan, stageLaneSelf, "o", "r", "o/r", prs, func(*gh.PullRequest) string { return "" })
	if len(kept) != 3 || kept[0].GetNumber() != 15407 {
		t.Fatalf("kept = %v", kept)
	}
	h, held := plan.duplicateOf(15402)
	if !held || h.Winner != 15407 || h.Issue != 15396 {
		t.Fatalf("hold = %+v held=%v", h, held)
	}
	if _, held := plan.duplicateOf(15407); held {
		t.Fatal("the winner is not held")
	}
	if _, held := plan.duplicateOf(15410); held {
		t.Fatal("a Refs line is not a claim")
	}
	// A prefilter refusal keeps the PR out of the winner rule entirely —
	// but it is still an open claimant, so the lane's remaining candidate
	// for that issue is held as a claim conflict (hivecommons/hive#15810).
	plan2 := &mergeTrainPlan{}
	kept = c.planDuplicateHolds(context.Background(), plan2, stageLaneSelf, "o", "r", "o/r", prs, func(p *gh.PullRequest) string {
		if p.GetNumber() == 15407 {
			return "label:hold"
		}
		return ""
	})
	if len(kept) != 3 || plan2.duplicates != nil {
		t.Fatalf("kept = %d dup=%v", len(kept), plan2.duplicates)
	}
	if h, held := plan2.claimConflictOf(15402); !held || h.Other != 15407 || h.Issue != 15396 {
		t.Fatalf("claim conflict = %+v held=%v", h, held)
	}
}

// Through the planner: the held duplicate leaves the train (the train is
// grouped from the survivors), the plan answers duplicateOf for it, and
// the skip reason parses back to the duplicate-of state.
func TestPlanMergeTrainsHoldsSameIssueDuplicate(t *testing.T) {
	t0 := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	api := &mergeTrainAPI{
		files: map[int][]string{15402: {"tracker.go"}, 15407: {"tracker.go"}, 15410: {"tracker.go"}},
		state: map[int]string{15402: "clean", 15407: "clean", 15410: "clean"},
	}
	srv := newMergeTrainServer(t, api)
	defer srv.Close()
	c, _ := newMergeTrainEngine(t, srv.URL, DefaultMergeTrainPolicy, nil)
	withBody := func(pr *gh.PullRequest, body string) *gh.PullRequest { pr.Body = gh.String(body); return pr }
	prs := []*gh.PullRequest{
		withBody(mtPR(15402, t0, ""), "Fixes #15396"),
		withBody(mtPR(15407, t0.Add(time.Hour), ""), "Fixes #15396"),
		withBody(mtPR(15410, t0.Add(2*time.Hour), ""), "Fixes #15399"),
	}
	mtReady(c, "self", prs...)
	plan := c.planMergeTrains(context.Background(), "self", "acme/widget", "acme", "widget", prs, func(*gh.PullRequest) string { return "" })
	h, held := plan.duplicateOf(15407)
	if !held || h.Winner != 15402 || h.Issue != 15396 {
		t.Fatalf("hold = %+v held=%v", h, held)
	}
	if plan.isSibling(15407) || plan.isFront(15407) {
		t.Fatal("a held duplicate rides no train")
	}
	if !plan.isFront(15402) || !plan.isSibling(15410) {
		t.Fatalf("train = %+v", plan.groups)
	}
	mt, ok := prstage.ParseMergeTrain(c.mergeTrainDuplicateSkip(plan, 15407))
	if !ok || mt.State != prstage.MergeTrainStateDuplicateOf || mt.Front != 15402 || mt.Position != 15396 {
		t.Fatalf("skip reason parsed %+v ok=%v", mt, ok)
	}
}
