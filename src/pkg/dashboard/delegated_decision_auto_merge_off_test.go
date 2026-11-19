package dashboard

import (
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
