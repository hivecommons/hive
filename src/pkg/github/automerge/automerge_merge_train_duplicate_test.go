package automerge

import (
	"context"
	"slices"
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

// The one-PR-per-issue rule picks only between heads at the merge gate
// and keeps its pick until the winner is blocked for good
// (hivecommons/hive#17439): a head still running CI is neither picked nor
// held — it claims the issue's turn at the merge point instead.
func TestMergeTrainDuplicatePicks(t *testing.T) {
	t0 := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	cand := func(n, min int, atGate bool) mergeTrainDuplicateCandidate {
		return mergeTrainDuplicateCandidate{Number: n, CreatedAt: t0.Add(time.Duration(min) * time.Minute), Green: atGate, AtGate: atGate}
	}
	terminal := func(c mergeTrainDuplicateCandidate) mergeTrainDuplicateCandidate {
		c.Terminal = true
		return c
	}
	for _, tc := range []struct {
		name       string
		cands      []mergeTrainDuplicateCandidate
		remembered int
		winner     int
		held       []int
		turns      []int
	}{
		{"both at the gate: the earlier wins, the later is held", []mergeTrainDuplicateCandidate{cand(15407, 30, true), cand(15402, 0, true)}, 0, 15402, []int{15407}, nil},
		{"CI running on both: nobody is picked or held", []mergeTrainDuplicateCandidate{cand(15402, 0, false), cand(15407, 30, false)}, 0, 0, nil, []int{15402, 15407}},
		{"only the later at the gate: it takes the issue, the earlier is not held", []mergeTrainDuplicateCandidate{cand(15402, 0, false), cand(15407, 30, true)}, 0, 15407, nil, []int{15402}},
		{"remembered winner with a red check keeps the issue", []mergeTrainDuplicateCandidate{cand(15402, 0, false), cand(15407, 30, true)}, 15402, 15402, []int{15407}, nil},
		{"remembered winner blocked for good lets it go", []mergeTrainDuplicateCandidate{terminal(cand(15402, 0, false)), cand(15407, 30, true)}, 15402, 15407, nil, []int{15402}},
		{"remembered winner gone: re-picked at the gate", []mergeTrainDuplicateCandidate{cand(15407, 30, true), cand(15410, 60, true)}, 15402, 15407, []int{15410}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			winners, holds, turns := mergeTrainDuplicatePicks(map[int][]mergeTrainDuplicateCandidate{15396: tc.cands}, map[int]int{15396: tc.remembered})
			if winners[15396] != tc.winner {
				t.Fatalf("winner = #%d, want #%d", winners[15396], tc.winner)
			}
			var held []int
			for _, h := range holds {
				if h.Winner != tc.winner || h.Issue != 15396 {
					t.Fatalf("hold %+v, want behind #%d for #15396", h, tc.winner)
				}
				held = append(held, h.Number)
			}
			if !slices.Equal(held, tc.held) {
				t.Fatalf("held = %v, want %v", held, tc.held)
			}
			var turned []int
			for n, issues := range turns {
				if !slices.Equal(issues, []int{15396}) {
					t.Fatalf("turn of #%d = %v", n, issues)
				}
				turned = append(turned, n)
			}
			slices.Sort(turned)
			if !slices.Equal(turned, tc.turns) {
				t.Fatalf("turns = %v, want %v", turned, tc.turns)
			}
		})
	}

	// A PR closing two issues is held on the first it loses and claims no
	// turn on the other; an issue only one PR closes is no contest.
	winners, holds, turns := mergeTrainDuplicatePicks(map[int][]mergeTrainDuplicateCandidate{
		15300: {cand(15320, 0, true), cand(15330, 60, true)},
		15301: {cand(15330, 60, true), cand(15340, 90, false)},
		15999: {cand(15350, 0, true)},
	}, nil)
	if len(holds) != 1 || holds[0] != (mergeTrainDuplicateHold{Number: 15330, Winner: 15320, Issue: 15300}) {
		t.Fatalf("holds = %+v", holds)
	}
	if len(turns) != 1 || !slices.Equal(turns[15340], []int{15301}) {
		t.Fatalf("turns = %v", turns)
	}
	if _, contested := winners[15999]; contested || winners[15301] != 15330 {
		t.Fatalf("winners = %v", winners)
	}
	mt, ok := prstage.ParseMergeTrain(mergeTrainDuplicateReason(holds[0]))
	if !ok || mt.State != prstage.MergeTrainStateDuplicateOf || mt.Front != 15320 || mt.Position != 15300 {
		t.Fatalf("reason parsed %+v ok=%v", mt, ok)
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
	for _, p := range prs {
		c.noteMergeTrainOutcome(stageLaneSelf, "o", "r", p.GetNumber(), p.GetHead().GetSHA(), "")
	}
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

// hivecommons/hive#17439: #17414 and #17415 fixed one issue and, with CI
// at 19/64, both read "needs human decision" while the winner flipped on
// every red check. A head short of the merge gate is not held: the lane
// evaluates it, so its row reads CI running. The first to reach the merge
// point keeps the issue; the other is held there and, at the gate, held
// behind it. A red check on the winner does not flip the pick; a block no
// re-run clears does.
func TestPlanDuplicateHoldsJudgesOnlyHeadsAtTheGate(t *testing.T) {
	c := &Engine{}
	t0 := time.Date(2026, 11, 9, 17, 0, 0, 0, time.UTC)
	early, late := claimPR(17414, "hive-app[bot]", "Fixes #17392", t0), claimPR(17415, "hive-app[bot]", "Fixes #17392", t0.Add(time.Minute))
	prs := []*gh.PullRequest{early, late}
	note := func(pr *gh.PullRequest, reason string) {
		c.noteMergeTrainOutcome(stageLaneSelf, "o", "r", pr.GetNumber(), pr.GetHead().GetSHA(), reason)
	}
	plan := func() *mergeTrainPlan {
		p := &mergeTrainPlan{lane: stageLaneSelf, displayRepo: "o/r", owner: "o", repo: "r"}
		c.planDuplicateHolds(context.Background(), p, stageLaneSelf, "o", "r", "o/r", prs, func(*gh.PullRequest) string { return "" })
		return p
	}
	atMergePoint := func(p *mergeTrainPlan, pr *gh.PullRequest) string {
		return mergeTrainCandidateHold(c.mergeTrainEvalContext(context.Background(), p, pr.GetNumber()))
	}

	note(early, "required-check-failing:lint")
	note(late, "required-check-pending:build")
	p := plan()
	if p.duplicates != nil || p.claimConflicts != nil {
		t.Fatalf("held while CI runs: dup=%v conflict=%v", p.duplicates, p.claimConflicts)
	}
	if _, turn := p.issueTurnOf(17414); !turn {
		t.Fatal("the red head carries no turn to the merge point")
	}
	if r := atMergePoint(p, late); r != "" {
		t.Fatalf("first at the merge point held: %q", r)
	}
	held := atMergePoint(p, early)
	if mt, ok := prstage.ParseMergeTrain(held); !ok || mt.State != prstage.MergeTrainStateDuplicateOf || mt.Front != 17415 || mt.Position != 17392 {
		t.Fatalf("second at the merge point = %q", held)
	}

	note(late, prstage.ContributorLaneReasonSoaking)
	note(early, held)
	p = plan()
	if h, ok := p.duplicateOf(17414); !ok || h.Winner != 17415 || h.Issue != 17392 {
		t.Fatalf("at the gate behind the first: %+v held=%v", h, ok)
	}
	if _, ok := p.duplicateOf(17415); ok {
		t.Fatal("the winner is held")
	}

	note(late, "required-check-failing:build")
	p = plan()
	if h, ok := p.duplicateOf(17414); !ok || h.Winner != 17415 {
		t.Fatalf("a red check flipped the pick: %+v held=%v", h, ok)
	}

	note(late, "not-mergeable")
	p = plan()
	if _, ok := p.duplicateOf(17414); ok {
		t.Fatal("a winner blocked for good still holds the issue")
	}
	if r := atMergePoint(p, early); r != "" {
		t.Fatalf("the new winner is held at the merge point: %q", r)
	}
	if mt, ok := prstage.ParseMergeTrain(atMergePoint(p, late)); !ok || mt.State != prstage.MergeTrainStateDuplicateOf || mt.Front != 17414 {
		t.Fatalf("the old winner at the merge point = %+v ok=%v", mt, ok)
	}
}

// The same fix opened against two release lines says "Fixes #N" twice and
// is not a duplicate: neither PR is held and neither is a claim conflict
// (hivecommons/hive#17436). Two PRs on the SAME line still compete.
func TestPlanDuplicateHoldsGroupsByBaseBranch(t *testing.T) {
	c := &Engine{}
	t0 := time.Date(2026, 11, 9, 17, 0, 0, 0, time.UTC)
	pr := func(n int, base string, at time.Time) *gh.PullRequest {
		return &gh.PullRequest{Number: gh.Int(n), Title: gh.String("t"), Body: gh.String("Fixes #17392"), CreatedAt: &gh.Timestamp{Time: at}, MergeableState: gh.String("clean"), State: gh.String("open"), Base: &gh.PullRequestBranch{Ref: gh.String(base)}, Head: &gh.PullRequestBranch{SHA: gh.String("sha")}}
	}
	prs := []*gh.PullRequest{
		pr(17414, "v5", t0),
		pr(17415, "v6", t0.Add(time.Minute)),
		pr(17420, "v6", t0.Add(2*time.Minute)),
	}
	// Every head is at the merge gate, so the rule judges them now.
	for _, p := range prs {
		c.noteMergeTrainOutcome(stageLaneSelf, "o", "r", p.GetNumber(), p.GetHead().GetSHA(), prstage.ContributorLaneReasonSoaking)
	}
	plan := &mergeTrainPlan{lane: "self", displayRepo: "o/r"}
	kept := c.planDuplicateHolds(context.Background(), plan, stageLaneSelf, "o", "r", "o/r", prs, func(*gh.PullRequest) string { return "" })
	if len(kept) != 2 || kept[0].GetNumber() != 17414 || kept[1].GetNumber() != 17415 {
		t.Fatalf("kept = %v", kept)
	}
	if _, held := plan.duplicateOf(17415); held {
		t.Fatal("a sibling on another base branch is not a duplicate")
	}
	if h, held := plan.duplicateOf(17420); !held || h.Winner != 17415 {
		t.Fatalf("same-line duplicate hold = %+v held=%v", h, held)
	}
	if _, held := plan.claimConflictOf(17415); held {
		t.Fatal("a sibling on another base branch is not a claim conflict")
	}

	// A prefilter refusal on the v5 sibling leaves it an open claimant, but
	// it still does not conflict with the v6 PR on another line.
	plan2 := &mergeTrainPlan{}
	c.planDuplicateHolds(context.Background(), plan2, stageLaneSelf, "o", "r", "o/r", prs[:2], func(p *gh.PullRequest) string {
		if p.GetNumber() == 17414 {
			return "label:hold"
		}
		return ""
	})
	if _, held := plan2.claimConflictOf(17415); held {
		t.Fatal("cross-line claimant must not hold the candidate")
	}
}
