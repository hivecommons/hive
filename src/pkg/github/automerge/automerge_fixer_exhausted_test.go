package automerge

import (
	"slices"
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

// hiveFixingPRNoIssue is a hive PR that closes no issue: nothing to
// re-author for, so exhaustion parks it for a person.
func hiveFixingPRNoIssue(number int, head string, labels ...string) *gh.PullRequest {
	pr := hiveFixingPR(number, head, labels...)
	pr.Body = gh.Ptr("Tidy the widget.\n\n— hive: backend=pi model=gpt-6\n")
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
	o.observeSelf(hiveFixingPRNoIssue(90, "ccc", "stage/070-fixing"), contributorLaneReasonChangesRequested)
	if rec, _ := o.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.Fixing {
		t.Fatalf("with attempts left = %s (%q)", rec.Stage, rec.Reason)
	}

	status[90] = review.FixDispatchStatus{Attempts: 6, Reason: review.FixDispatchExcludedAttemptsExhausted}
	o.api.reset()
	o.clock.advance(time.Minute)
	o.observeSelf(hiveFixingPRNoIssue(90, "ccc", "stage/070-fixing"), contributorLaneReasonChangesRequested)
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
	for _, want := range []string{"gave up after 6 attempts", "last reviewer finding: handler drops the error from Close()", "Give direction", "Hand it to a person", "Close as not planned"} {
		if !strings.Contains(comments[0], want) {
			t.Fatalf("decision comment lacks %q:\n%s", want, comments[0])
		}
	}
	if strings.Contains(comments[0], "more detail") {
		t.Fatalf("decision comment carries more than the finding's first line:\n%s", comments[0])
	}

	// Same head, next pass: no second comment.
	o.clock.advance(5 * time.Minute)
	o.observeSelf(hiveFixingPRNoIssue(90, "ccc", "stage/070-fixing"), contributorLaneReasonChangesRequested)
	if got := decisionComments(o.api); len(got) != 1 {
		t.Fatalf("decision comments after second pass = %d, want 1", len(got))
	}
	if rec, _ := o.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.NeedsDecision {
		t.Fatalf("second pass moved the PR to %s", rec.Stage)
	}

	// A fix in flight on a new head is a fixing stage again.
	status[90] = review.FixDispatchStatus{Active: true, Agent: "fixer", Attempts: 1}
	o.observeSelf(hiveFixingPRNoIssue(90, "ddd", "stage/085-needs-decision"), contributorLaneReasonChangesRequested)
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

// Owner on #16456: "we should not need a re-author pill on a hive agent's
// PR." The first exhaustion of a hive PR re-authors it automatically —
// marker label on the issue, note, superseded label, close, store record,
// reason re-authoring:<issue> — and never parks at 085. The second
// exhaustion for the same issue parks with the remaining answers only.
func TestFixerExhaustedHivePRReauthorsItselfOnce(t *testing.T) {
	o, done, status := fixerExhaustedHarness(t)
	defer done()
	key := stageKey("acme", "widget", 90)
	setStagePRTruth(12)

	status[90] = review.FixDispatchStatus{Attempts: 6, Reason: review.FixDispatchExcludedAttemptsExhausted}
	o.observeSelf(hiveFixingPR(90, "ccc", "stage/070-fixing"), contributorLaneReasonChangesRequested)
	rec, _ := o.e.stageStoreOrShared().Get(key)
	if rec.Stage != stagelabels.Fixing || rec.Reason != stagelabels.ReauthoringReason(12) {
		t.Fatalf("first exhaustion = %s (%q), want 070 re-authoring:12", rec.Stage, rec.Reason)
	}
	if !rec.Reauthored() || rec.ReauthorIssue != 12 || rec.ReauthorHead != "ccc" || rec.ReauthorAttempts != 6 || !rec.ReauthorNoted {
		t.Fatalf("store record = %+v", rec)
	}
	if got := decisionComments(o.api); len(got) != 0 {
		t.Fatalf("decision comment posted on an auto re-author: %q", got)
	}
	var notes []string
	for _, c := range o.api.comments {
		if strings.HasPrefix(c, reauthorMarker) {
			notes = append(notes, c)
		}
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "gave up on this PR after 6 attempts") || !strings.Contains(notes[0], "fresh PR for #12 from the current `v5`") {
		t.Fatalf("re-author notes = %q", notes)
	}
	added := o.addedLabels()
	for _, want := range []string{stagelabels.LabelReauthored, "hive/superseded"} {
		if !slices.Contains(added, want) {
			t.Fatalf("labels added %q lack %q", added, want)
		}
	}
	if !slices.Contains(o.adopt.prEdits, "90:closed") {
		t.Fatalf("PR edits = %q, want 90 closed", o.adopt.prEdits)
	}
	if got := o.e.stageStoreOrShared().ReauthoredFor("acme/widget", 12); got != 90 {
		t.Fatalf("ReauthoredFor(12) = %d, want 90", got)
	}

	// The fresh PR for the same issue exhausts its fixer too: parked at
	// 085 naming the earlier re-author; no second re-author, no re-author
	// option in the comment.
	setStagePRTruth(12, stagelabels.LabelReauthored)
	o.api.reset()
	o.adopt.prEdits = nil
	o.clock.advance(time.Hour)
	status[91] = review.FixDispatchStatus{Attempts: 6, Reason: review.FixDispatchExcludedAttemptsExhausted}
	o.observeSelf(hiveFixingPR(91, "ddd", "stage/070-fixing"), contributorLaneReasonChangesRequested)
	rec91, _ := o.e.stageStoreOrShared().Get(stageKey("acme", "widget", 91))
	if rec91.Stage != stagelabels.NeedsDecision || rec91.Reason != stagelabels.FixerExhaustedReason(6) || rec91.Reauthored() {
		t.Fatalf("second exhaustion = %s (%q) reauthored=%v, want 085 fixer-exhausted:6", rec91.Stage, rec91.Reason, rec91.Reauthored())
	}
	if len(o.adopt.prEdits) != 0 {
		t.Fatalf("second exhaustion closed a PR: %q", o.adopt.prEdits)
	}
	comments := decisionComments(o.api)
	if len(comments) != 1 {
		t.Fatalf("decision comments = %d, want 1: %q", len(comments), comments)
	}
	for _, want := range []string{"re-author of #90", "Give direction", "Hand it to a person", "Close as not planned"} {
		if !strings.Contains(comments[0], want) {
			t.Fatalf("decision comment lacks %q:\n%s", want, comments[0])
		}
	}
	if strings.Contains(comments[0], "Re-author from scratch") || strings.Contains(comments[0], "adoption/supersede") {
		t.Fatalf("decision comment still offers re-authoring:\n%s", comments[0])
	}
}

// Only the issue's marker label (store lost): still no second re-author.
func TestFixerExhaustedMarkerLabelAloneBlocksReauthor(t *testing.T) {
	o, done, status := fixerExhaustedHarness(t)
	defer done()
	setStagePRTruth(12, stagelabels.LabelReauthored)
	status[90] = review.FixDispatchStatus{Attempts: 6, Reason: review.FixDispatchExcludedAttemptsExhausted}
	o.observeSelf(hiveFixingPR(90, "ccc", "stage/070-fixing"), contributorLaneReasonChangesRequested)
	rec, _ := o.e.stageStoreOrShared().Get(stageKey("acme", "widget", 90))
	if rec.Stage != stagelabels.NeedsDecision || rec.Reauthored() || len(o.adopt.prEdits) != 0 {
		t.Fatalf("marker label ignored: %s (%q) edits=%q", rec.Stage, rec.Reason, o.adopt.prEdits)
	}
}
