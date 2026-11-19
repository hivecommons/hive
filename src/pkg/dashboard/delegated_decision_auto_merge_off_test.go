package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// TestDelegatedHumanMergesReadsRepoAutoMerge: the decision comment's
// "a person merges it" switch is config.RepoAutoMergeEnabled — the switch
// the merge sweep reads — so it is on below ACMM L6 and for a repo whose
// owner turned auto-merge off at L6, off for an L6 repo the hive merges,
// and never set on an issue.
func TestDelegatedHumanMergesReadsRepoAutoMerge(t *testing.T) {
	srv := newDelegatedServer(t, &repoItemDecisionMock{})
	pr := delegatedecision.Input{Kind: delegatedecision.KindPR, Repo: "testorg/testrepo", Number: 8}
	issue := delegatedecision.Input{Kind: delegatedecision.KindIssue, Repo: "testorg/testrepo", Number: 7}

	// newFullServer runs the hive at ACMM L2: auto-merge is fail-closed.
	if !srv.delegatedHumanMerges(pr) || !srv.delegatedCommentOptions(pr).HumanMerges {
		t.Fatal("below ACMM L6 a person merges every PR")
	}
	if srv.delegatedHumanMerges(issue) || srv.delegatedCommentOptions(issue).HumanMerges {
		t.Fatal("an issue has nothing to merge")
	}

	level := config.SelfMergeMinACMMLevel
	srv.deps.Config.ACMMLevel = &level
	if srv.delegatedHumanMerges(pr) {
		t.Fatal("at L6 with no repo override the hive merges")
	}
	opts := srv.delegatedCommentOptions(pr)
	if opts.HumanMerges || opts.Kind != delegatedecision.KindPR || opts.EffectTimeout != srv.delegatedEffectTimeout() {
		t.Fatalf("options = %+v", opts)
	}

	srv.deps.Config.Project.RepoPolicies = []config.RepoPolicy{{Repo: "testrepo", AutoMerge: boolPtr(false)}}
	if !srv.delegatedHumanMerges(pr) || srv.delegatedHumanMerges(delegatedecision.Input{Kind: delegatedecision.KindPR, Repo: "testorg/other", Number: 8}) {
		t.Fatal("the repo's own auto_merge: false must read as a person merging, for that repo only")
	}
}

// TestDelegatedApproveOnAutoMergeOffRepoWaitsForNothing: "Let hive decide"
// approved a PR on a repo whose auto-merge is off, and the comment said
// "The merge queue merges this PR once its checks are green — expected
// within 30 min." Nothing merged it: a person does. The comment now says
// so, and a repeat click past the deadline is a plain no-op — no
// "decision-stalled" follow-up waiting on a merge that is not coming. On a
// repo the hive merges, the same repeat still reports the stall.
func TestDelegatedApproveOnAutoMergeOffRepoWaitsForNothing(t *testing.T) {
	mock := &repoItemDecisionMock{}
	srv := newDelegatedServer(t, mock)

	// The hive runs at ACMM L2 here, so auto-merge is off for every repo.
	w := postDelegated(t, srv, 8, "writer", `{"type":"pr"}`, nil)
	job := decodeDelegatedJob(t, w)
	if w.Code != http.StatusOK || job.Decision == nil || job.Decision.Verdict != delegatedecision.VerdictApprove {
		t.Fatalf("code=%d job=%+v", w.Code, job)
	}
	rule := job.Decision.Rule
	var decision string
	for _, c := range mock.comments {
		if delegatedecision.IsDelegatedComment(c) {
			decision = c
		}
	}
	t.Logf("decision comment:\n%s", decision)
	if decision == "" {
		t.Fatalf("no decision comment in %v", mock.comments)
	}
	if strings.Contains(decision, "merge queue merges") || strings.Contains(decision, "expected within") ||
		!strings.Contains(decision, "**What happens next:** A person merges this PR — auto-merge is off for this repo") {
		t.Fatalf("decision comment promises the merge queue:\n%s", decision)
	}
	// The toast and the job notes say the same: a person merges it.
	if job.Result == nil || job.Result.Next != delegatedHumanMergesNext {
		t.Fatalf("toast next = %+v", job.Result)
	}
	for _, n := range job.Notes {
		if strings.Contains(n, "hive merges it") {
			t.Fatalf("note promises a hive merge: %v", job.Notes)
		}
	}
	if !strings.Contains(strings.Join(job.Notes, "\n"), "`/lgtm` + `/approve` recorded; auto-merge is off for this repo, so a person merges it") {
		t.Fatalf("notes = %v", job.Notes)
	}

	// Repeat click well past decisions.delegate.effect_timeout (the mock
	// stamps prior comments 2026-10-28T12:01:00Z).
	pinDelegatedClock(t, "2026-10-28T14:23:00Z")
	mock.priorComments = []string{decision}
	mock.comments = nil
	w = postDelegated(t, srv, 8, "writer", `{"type":"pr"}`, nil)
	job = decodeDelegatedJob(t, w)
	if job.Result == nil || job.Result.Action != rowActionNoop {
		t.Fatalf("auto-merge-off repeat must be a no-op: result=%+v notes=%v err=%s", job.Result, job.Notes, job.Error)
	}
	if !strings.HasPrefix(job.Result.Outcome, "No change — hive already recorded this decision") || !strings.Contains(job.Result.Next, "Merge it by hand") {
		t.Fatalf("outcome=%q next=%q", job.Result.Outcome, job.Result.Next)
	}
	if len(mock.comments) != 0 {
		t.Fatalf("no stalled follow-up may be posted where a person merges: %v", mock.comments)
	}

	// The same repeat on a repo the hive merges (ACMM L6, no override)
	// still reports that the merge did not happen.
	level := config.SelfMergeMinACMMLevel
	srv.deps.Config.ACMMLevel = &level
	mock.comments = nil
	w = postDelegated(t, srv, 8, "writer", `{"type":"pr"}`, nil)
	job = decodeDelegatedJob(t, w)
	if job.Result == nil || job.Result.Action != rowActionNoEffect {
		t.Fatalf("auto-merge-on repeat must report no-effect: result=%+v notes=%v err=%s", job.Result, job.Notes, job.Error)
	}
	if len(mock.comments) != 1 || !delegatedecision.IsStalledComment(mock.comments[0], rule) {
		t.Fatalf("auto-merge-on stall must post one follow-up: %v", mock.comments)
	}
}

// TestDelegatedRowActionResultHumanMerges: the approval toast on a PR whose
// repo's auto-merge is off names the person's merge, never the hive's next
// merge pass; with auto-merge on, and for every other branch, it is the
// text it always was.
func TestDelegatedRowActionResultHumanMerges(t *testing.T) {
	pr := delegatedecision.Input{Repo: "o/r", Kind: delegatedecision.KindPR, Number: 8}
	approve := delegatedecision.Decision{Verdict: delegatedecision.VerdictApprove, Rule: "default-approve", Text: "approve #8"}
	const onNext = "Hive merges it on its next merge pass once CI is green and the head has soaked."
	for _, notes := range [][]string{nil, {"`/lgtm` + `/approve` recorded; auto-merge is off for this repo, so a person merges it once CI is green"}} {
		off := delegatedRowActionResultWith(pr, approve, notes, true)
		if off.Next != delegatedHumanMergesNext || strings.Contains(off.Next, "merge pass") || strings.Contains(strings.ToLower(off.Next), "sweep") {
			t.Errorf("auto-merge off, notes %v: next = %q", notes, off.Next)
		}
		if off.Action != rowActionApproved {
			t.Errorf("auto-merge off: action = %q", off.Action)
		}
		if on := delegatedRowActionResult(pr, approve, notes); on.Next != onNext {
			t.Errorf("auto-merge on, notes %v: next = %q", notes, on.Next)
		}
	}
	lgtm := []string{"`/lgtm` + `/approve` recorded"}
	if got := delegatedRowActionResultWith(pr, approve, lgtm, true).Posted; strings.Join(got, "|") != "`/lgtm` posted|`/approve` posted" {
		t.Errorf("posted = %v", got)
	}
	// Other branches are untouched by the switch.
	issue := delegatedecision.Input{Repo: "o/r", Kind: delegatedecision.KindIssue, Number: 7}
	for _, tc := range []struct {
		in    delegatedecision.Input
		notes []string
	}{
		{issue, nil},
		{pr, []string{"queue approval deferred: the sentinel label is cleared only by a person"}},
		{pr, []string{"Approve review submitted on the head and queued for auto-merge (the engine merges per repo settings)"}},
	} {
		if on, off := delegatedRowActionResult(tc.in, approve, tc.notes), delegatedRowActionResultWith(tc.in, approve, tc.notes, true); on.Next != off.Next {
			t.Errorf("%s notes %v changed with the switch: %q -> %q", tc.in.Kind, tc.notes, on.Next, off.Next)
		}
	}
}

// TestDelegatedPlanWordingHumanMerges: the "Let hive decide" preview of a
// PR approval names the person's merge where auto-merge is off and keeps
// the merge-queue wording where the hive merges; an issue's plan never
// changes.
func TestDelegatedPlanWordingHumanMerges(t *testing.T) {
	pr := delegatedecision.Input{Kind: delegatedecision.KindPR, Number: 8}
	approve := delegatedecision.Decision{Verdict: delegatedecision.VerdictApprove, Rule: "default-approve"}

	summary, next := delegatedPlanWordingWith(pr, approve, true)
	all := summary + " " + strings.Join(next, " ")
	if summary != "Approve #8 as it is; a person merges it — auto-merge is off for this repo." ||
		strings.Contains(all, "merge queue") || strings.Contains(all, "#8 merges once") || !strings.Contains(all, "a person merges #8 once CI is green") {
		t.Fatalf("auto-merge off: %q %q", summary, next)
	}

	summary, next = delegatedPlanWordingWith(pr, approve, false)
	if summary != "Approve #8 as it is and let the merge queue merge it." ||
		strings.Join(next, "|") != "Hive approves the current commit.|#8 merges once CI is green and it has soaked." {
		t.Fatalf("auto-merge on changed: %q %q", summary, next)
	}
	if s2, n2 := delegatedPlanWording(pr, approve); s2 != summary || strings.Join(n2, "|") != strings.Join(next, "|") {
		t.Fatalf("delegatedPlanWording must stay the auto-merge-on wording: %q %q", s2, n2)
	}

	issue := delegatedecision.Input{Kind: delegatedecision.KindIssue, Number: 15734}
	s1, n1 := delegatedPlanWordingWith(issue, approve, true)
	s0, n0 := delegatedPlanWording(issue, approve)
	if s1 != s0 || strings.Join(n1, "|") != strings.Join(n0, "|") {
		t.Fatalf("issue plan changed with the switch: %q %q vs %q %q", s1, n1, s0, n0)
	}
}

// TestDelegatedDecisionPlanMergeableAsIsAtL6KeepsMergeQueue: the plan
// endpoint threads the repo's auto-merge switch — at ACMM L6 with no repo
// override the mergeable-as-is plan still promises the merge queue (the
// L2 run of the same request is TestDelegatedDecisionPlanMergeableAsIsOffersMerge).
func TestDelegatedDecisionPlanMergeableAsIsAtL6KeepsMergeQueue(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true, prLabels: []string{"stage/085-needs-decision", "needs-human"}}
	srv := newDelegatedServer(t, mock)
	level := config.SelfMergeMinACMMLevel
	srv.deps.Config.ACMMLevel = &level
	body := `{"type":"pr","reason":"needs-decision","caption":"needs a decision"}`
	w := postDelegatedPlan(t, srv, 8, "writer", body, map[string]string{"X-Hive-Role": "merger"})
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var plan delegatedDecisionPlan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Would != string(delegatedecision.VerdictApprove) || plan.Summary != "Approve #8 as it is and let the merge queue merge it." {
		t.Fatalf("plan would=%q summary=%q", plan.Would, plan.Summary)
	}
}
