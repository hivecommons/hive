package automerge

import (
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Fixer exhausted on the hive's own PR (hivecommons/hive#15744): the fix
// dispatcher refused another fix every cycle (fix-attempts-exhausted, 6
// attempts) while #15308 sat at 070 reading "Fixing" for 13h. The stage
// pass now parks it at 085 with the attempt count in the reason and posts
// the decision comment once per head.

func fixerExhaustedHarness(t *testing.T) (*offlineAdoptHarness, func(), map[int]review.FixDispatchStatus) {
	t.Helper()
	o, done := newOfflineAdoptHarness(t, true)
	status := map[int]review.FixDispatchStatus{}
	changes := ContributorLaneVerdict{Configured: true, Verdict: "changes_requested", HumanSummaries: []string{"handler drops the error from Close()\nmore detail"}}
	o.e.contributorLaneVerdict = func(string, int, string) ContributorLaneVerdict { return changes }
	o.e.fixDispatch = func(_ string, number int, _ string) (review.FixDispatchStatus, bool) {
		st, ok := status[number]
		return st, ok
	}
	return o, done, status
}

func hiveFixingPR(number int, head string, labels ...string) *gh.PullRequest {
	pr := adoptPR(number, head, labels...)
	pr.User = &gh.User{Login: gh.Ptr(testHiveAppBotLogin), Type: gh.Ptr("Bot")}
	pr.Head.Ref = gh.Ptr("hive/fix-" + head)
	pr.Head.Repo = &gh.Repository{FullName: gh.Ptr("acme/widget")}
	return pr
}

func decisionComments(api *stageAPI) []string {
	var out []string
	for _, c := range api.comments {
		if strings.HasPrefix(c, stagelabels.DecisionCommentMarker) {
			out = append(out, c)
		}
	}
	return out
}

func TestFixerExhaustedParksHivePRAtNeedsDecisionOnce(t *testing.T) {
	o, done, status := fixerExhaustedHarness(t)
	defer done()
	key := stageKey("acme", "widget", 90)

	// Attempts left: the PR stays on its fixing stage.
	status[90] = review.FixDispatchStatus{Attempts: 3}
	o.observeSelf(hiveFixingPR(90, "ccc", "stage/070-fixing"), contributorLaneReasonChangesRequested)
	if rec, _ := o.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.Fixing {
		t.Fatalf("with attempts left = %s (%q)", rec.Stage, rec.Reason)
	}

	status[90] = review.FixDispatchStatus{Attempts: 6, Reason: review.FixDispatchExcludedAttemptsExhausted}
	o.api.reset()
	o.clock.advance(time.Minute)
	o.observeSelf(hiveFixingPR(90, "ccc", "stage/070-fixing"), contributorLaneReasonChangesRequested)
	rec, _ := o.e.stageStoreOrShared().Get(key)
	if rec.Stage != stagelabels.NeedsDecision || rec.Reason != stagelabels.FixerExhaustedReason(6) {
		t.Fatalf("after exhaustion = %s (%q)", rec.Stage, rec.Reason)
	}
	if n, ok := stagelabels.ParseFixerExhausted(rec.Reason); !ok || n != 6 {
		t.Fatalf("reason %q does not parse to 6 attempts", rec.Reason)
	}
	comments := decisionComments(o.api)
	if len(comments) != 1 {
		t.Fatalf("decision comments = %d, want 1: %q", len(comments), comments)
	}
	for _, want := range []string{"gave up after 6 attempts", "last reviewer finding: handler drops the error from Close()", "Re-author from scratch", "Hand it to a person", "Close as not planned"} {
		if !strings.Contains(comments[0], want) {
			t.Fatalf("decision comment lacks %q:\n%s", want, comments[0])
		}
	}
	if strings.Contains(comments[0], "more detail") {
		t.Fatalf("decision comment carries more than the finding's first line:\n%s", comments[0])
	}

	// Same head, next pass: no second comment.
	o.clock.advance(5 * time.Minute)
	o.observeSelf(hiveFixingPR(90, "ccc", "stage/070-fixing"), contributorLaneReasonChangesRequested)
	if got := decisionComments(o.api); len(got) != 1 {
		t.Fatalf("decision comments after second pass = %d, want 1", len(got))
	}
	if rec, _ := o.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.NeedsDecision {
		t.Fatalf("second pass moved the PR to %s", rec.Stage)
	}

	// A fix in flight on a new head is a fixing stage again.
	status[90] = review.FixDispatchStatus{Active: true, Agent: "fixer", Attempts: 1}
	o.observeSelf(hiveFixingPR(90, "ddd", "stage/085-needs-decision"), contributorLaneReasonChangesRequested)
	if rec, _ := o.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.Fixing {
		t.Fatalf("new head with an active fix = %s (%q)", rec.Stage, rec.Reason)
	}
}

func TestFixerExhaustedIgnoresContributorPR(t *testing.T) {
	o, done, status := fixerExhaustedHarness(t)
	defer done()
	status[7] = review.FixDispatchStatus{Attempts: 6, Reason: review.FixDispatchExcludedAttemptsExhausted}
	o.observe(adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker"), contributorLaneReasonChangesRequested)
	if rec, _ := o.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7)); rec.Stage == stagelabels.NeedsDecision {
		t.Fatalf("contributor PR parked at 085 on the hive's fixer status: %+v", rec)
	}
	if got := decisionComments(o.api); len(got) != 0 {
		t.Fatalf("decision comments on a contributor PR = %d", len(got))
	}
}
