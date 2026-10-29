package github

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/sentinel"
)

func TestRecordOperatorPullRequestApprovalPostsCommandsOnly(t *testing.T) {
	c, rec := newUnparkPRClient(t, []unparkWireIssue{hiveParkedPR()}, map[string]string{"maintainer": "write"})

	event, err := c.RecordOperatorPullRequestApproval(context.Background(), "org/repo", liveDecisionPR, "maintainer")
	if err != nil {
		t.Fatalf("approval: %v", err)
	}
	if event.Actor != "maintainer" || event.Number != liveDecisionPR || event.HeadSHA != liveDecisionHead {
		t.Fatalf("event = %+v", event)
	}
	posted := rec.posted[liveDecisionPR]
	if len(posted) != len(PRApprovalCommands) {
		t.Fatalf("expected %d command comments, got %v", len(PRApprovalCommands), posted)
	}
	for i, command := range PRApprovalCommands {
		if !strings.HasPrefix(posted[i], command+"\n") {
			t.Fatalf("comment %d must open with the %s command on its own line, got %q", i, command, posted[i])
		}
		if !strings.Contains(posted[i], "@maintainer") || !strings.Contains(posted[i], "Hive dashboard") {
			t.Fatalf("comment %d must name the operator, got %q", i, posted[i])
		}
	}
	if len(rec.added[liveDecisionPR])+len(rec.removed[liveDecisionPR]) != 0 {
		t.Fatalf("approval must not touch labels: added=%v removed=%v", rec.added[liveDecisionPR], rec.removed[liveDecisionPR])
	}
}

func TestRecordOperatorPullRequestApprovalRefusesClosedAndMissing(t *testing.T) {
	closed := hiveParkedPR()
	closed.State = "closed"
	c, rec := newUnparkPRClient(t, []unparkWireIssue{closed}, nil)

	if _, err := c.RecordOperatorPullRequestApproval(context.Background(), "org/repo", liveDecisionPR, "maintainer"); !errors.Is(err, ErrPullRequestClosed) {
		t.Fatalf("closed PR: err = %v, want ErrPullRequestClosed", err)
	}
	if _, err := c.RecordOperatorPullRequestApproval(context.Background(), "org/repo", 4040, "maintainer"); !errors.Is(err, ErrPullRequestNotFound) {
		t.Fatalf("missing PR: err = %v, want ErrPullRequestNotFound", err)
	}
	if _, err := c.RecordOperatorPullRequestApproval(context.Background(), "org/repo", liveDecisionPR, ""); err == nil {
		t.Fatal("empty actor must be refused")
	}
	if len(rec.posted[liveDecisionPR]) != 0 {
		t.Fatalf("refusals must post nothing, got %v", rec.posted[liveDecisionPR])
	}
}

func TestRecordOperatorPullRequestDecisionRecordsChangeRequestAndReplies(t *testing.T) {
	c, rec := newUnparkPRClient(t, []unparkWireIssue{hiveParkedPR()}, nil)
	store := review.NewHumanChangeRequests("")

	event, err := c.RecordOperatorPullRequestDecision(context.Background(), "org/repo", liveDecisionPR, "maintainer", liveDecisionText, store)
	if err != nil {
		t.Fatalf("decision: %v", err)
	}
	if event.Decision != liveDecisionText || event.HeadSHA != liveDecisionHead || event.Actor != "maintainer" {
		t.Fatalf("event = %+v", event)
	}
	got, ok := store.For("org/repo", liveDecisionPR, liveDecisionHead)
	if !ok || got.Body != liveDecisionText || got.Source != review.HumanChangeRequestSourceDecision || got.Actor != "maintainer" {
		t.Fatalf("stored request = %+v ok=%v", got, ok)
	}
	posted := strings.Join(rec.posted[liveDecisionPR], "\n")
	if !strings.Contains(posted, "Decision recorded by @maintainer") || !strings.Contains(posted, "hive dashboard") {
		t.Fatalf("expected the recorded-decision reply, got %q", posted)
	}
	if !strings.Contains(posted, unparkReplyMarkerPrefix+"0 -->") {
		t.Fatalf("reply must carry the shared unpark marker with a non-command id, got %q", posted)
	}
	if event.Mode != string(prDecisionChangesRequested) || got.IsApproval() {
		t.Fatalf("event mode = %q, kind = %q; want changes_requested", event.Mode, got.Kind)
	}
	if facts := prDecisionFacts(t, c); len(facts) != 1 || !facts[0].Instruction || facts[0].Head != liveDecisionHead {
		t.Fatalf("stage facts = %+v, want one PRDecided with the instruction", facts)
	}

	// A bare "approve" from the dashboard is an approval, not a fix request.
	c2, rec2 := newUnparkPRClient(t, []unparkWireIssue{hiveParkedPR()}, nil)
	event2, err := c2.RecordOperatorPullRequestDecision(context.Background(), "org/repo", liveDecisionPR, "maintainer", "approve", store)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if event2.Mode != string(prDecisionApprove) || review.HumanChangeRequestFor(store, "org/repo", liveDecisionPR, liveDecisionHead) != nil || review.HumanApprovalFor(store, "org/repo", liveDecisionPR, liveDecisionHead) == nil {
		t.Fatalf("approve event = %+v, store = %+v", event2, store)
	}
	if posted := strings.Join(rec2.posted[liveDecisionPR], "\n"); !strings.Contains(posted, "Approval recorded by @maintainer") {
		t.Fatalf("expected the approval reply, got %q", posted)
	}
	if facts := prDecisionFacts(t, c2); len(facts) != 1 || facts[0].Instruction {
		t.Fatalf("stage facts = %+v, want one PRDecided without an instruction", facts)
	}
}

func TestRecordOperatorPullRequestDecisionRefusesForeignEmptyAndUnstored(t *testing.T) {
	foreign := parkedPR(77, "someone", []string{PRNeedsDecisionStageLabel})
	c, rec := newUnparkPRClient(t, []unparkWireIssue{hiveParkedPR(), foreign}, nil)
	store := review.NewHumanChangeRequests("")

	if _, err := c.RecordOperatorPullRequestDecision(context.Background(), "org/repo", 77, "maintainer", "do it", store); !errors.Is(err, ErrPullRequestNotHiveAuthored) {
		t.Fatalf("foreign PR: err = %v, want ErrPullRequestNotHiveAuthored", err)
	}
	if _, err := c.RecordOperatorPullRequestDecision(context.Background(), "org/repo", liveDecisionPR, "maintainer", "  ", store); err == nil {
		t.Fatal("empty direction must be refused")
	}
	if _, err := c.RecordOperatorPullRequestDecision(context.Background(), "org/repo", liveDecisionPR, "maintainer", "do it", nil); err == nil {
		t.Fatal("nil store must be refused")
	}
	if _, ok := store.For("org/repo", 77, liveDecisionHead); ok {
		t.Fatal("a refused decision must not be stored")
	}
	if len(rec.posted[77])+len(rec.posted[liveDecisionPR]) != 0 {
		t.Fatalf("refusals must post nothing, got %v / %v", rec.posted[77], rec.posted[liveDecisionPR])
	}
}

// TestRecordOperatorPullRequestApprovalWithReleasesThePark: a person's
// Approve from the dashboard is the human review the row was parked for
// (hivecommons/hive#14821, #14822, crux#30 stayed at stage/085 after the
// owner approved because only the two command comments were written). With
// options it records the head-scoped approval the lanes and the stage engine
// honour, lifts `needs-human`, clears the sentinel label, acknowledges the
// bundle's sentinel findings and submits the approve decision to the stage
// engine — while the bare call stays comments-only for the delegated path.
func TestRecordOperatorPullRequestApprovalWithReleasesThePark(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 50, 0, 0, time.UTC)
	flagged := parkedPR(liveDecisionPR, "thatguywob", []string{PRNeedsDecisionStageLabel, issueNeedsHumanLabel, "sentinel-alert", "role/trusted"})
	c, rec := newUnparkPRClient(t, []unparkWireIssue{flagged}, map[string]string{"clubanderson": "admin"})
	root := withEvidenceRoot(t)
	c.SetReviewEvidence(func(string) ReviewEvidenceSettings { return ReviewEvidenceSettings{Enabled: true} })
	pr := &gh.PullRequest{Number: gh.Ptr(liveDecisionPR), User: &gh.User{Login: gh.Ptr("thatguywob"), Type: gh.Ptr("User")},
		Base: &gh.PullRequestBranch{SHA: gh.Ptr("base000")}, Head: &gh.PullRequestBranch{SHA: gh.Ptr(liveDecisionHead)}}
	c.recordReviewEvidenceSentinel("org/repo", pr, []sentinel.Finding{{Rule: sentinel.RuleSensitivePath, Summary: "workflow timeout 10→45", Paths: []string{".github/workflows/v2-ci.yml"}}}, false, now.Add(-time.Hour))
	store := review.NewHumanChangeRequests("")

	event, err := c.RecordOperatorPullRequestApprovalWith(context.Background(), "org/repo", liveDecisionPR, "clubanderson", PullRequestApprovalOptions{
		HumanChangeRequests: store, LiftNeedsHuman: true, SentinelLabel: "sentinel-alert", Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("approval: %v", err)
	}
	if !event.Recorded || !event.NeedsHumanLifted || !event.SentinelLabelCleared || !event.SentinelAcknowledged || event.HeadSHA != liveDecisionHead {
		t.Fatalf("event = %+v", event)
	}
	if event.SentinelLabelOpen || event.ProwManaged {
		t.Fatalf("a cleared alert on a non-Prow repo: event = %+v", event)
	}
	if posted := rec.posted[liveDecisionPR]; len(posted) != len(PRApprovalCommands) {
		t.Fatalf("the command comments are still posted, got %v", posted)
	}
	req, ok := review.DecisionAnswerFor(store, "org/repo", liveDecisionPR, liveDecisionHead)
	if !ok || !req.IsApproval() || req.Actor != "clubanderson" || req.Directive() != review.DecisionDirectiveApprove || req.SourceID != operatorDecisionMarkerID {
		t.Fatalf("recorded approval = %+v, ok=%v", req, ok)
	}
	if got := review.HumanApprovalFor(store, "org/repo", liveDecisionPR, liveDecisionHead); got == nil {
		t.Fatal("a bare approval must stand against the head for the lanes")
	}
	removed := strings.Join(rec.removed[liveDecisionPR], ",")
	if !strings.Contains(removed, issueNeedsHumanLabel) || !strings.Contains(removed, "sentinel-alert") {
		t.Fatalf("needs-human and the sentinel label must be lifted, removed=%v", rec.removed[liveDecisionPR])
	}
	if len(rec.added[liveDecisionPR]) != 0 {
		t.Fatalf("approval adds no labels, added=%v", rec.added[liveDecisionPR])
	}
	b, err := LoadReviewEvidence(ReviewEvidencePath(root, "org/repo", liveDecisionPR, liveDecisionHead))
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if b.SentinelAcknowledgedBy() != "clubanderson" || len(b.Sentinel) != 1 {
		t.Fatalf("bundle must carry the acknowledgment and keep the finding: actions=%+v sentinel=%+v", b.HumanActions, b.Sentinel)
	}
	facts := prDecisionFacts(t, c)
	if len(facts) != 1 || facts[0].Head != liveDecisionHead || facts[0].Instruction || facts[0].By != "clubanderson" || facts[0].SelfAuthored {
		t.Fatalf("stage engine must see one approve decision on the head, got %+v", facts)
	}

	// The same approval twice is idempotent: the record is replaced, no
	// label is removed twice, the acknowledgment is not duplicated.
	again, err := c.RecordOperatorPullRequestApprovalWith(context.Background(), "org/repo", liveDecisionPR, "clubanderson", PullRequestApprovalOptions{
		HumanChangeRequests: store, LiftNeedsHuman: true, SentinelLabel: "sentinel-alert", Now: func() time.Time { return now.Add(time.Minute) },
	})
	if err != nil {
		t.Fatalf("second approval: %v", err)
	}
	if !again.Recorded || !again.SentinelAcknowledged || again.NeedsHumanLifted || again.SentinelLabelCleared {
		t.Fatalf("second event = %+v", again)
	}
	if b, _ = LoadReviewEvidence(ReviewEvidencePath(root, "org/repo", liveDecisionPR, liveDecisionHead)); len(b.HumanActions) != 1 {
		t.Fatalf("acknowledgment duplicated: %+v", b.HumanActions)
	}
}

// TestProwManagedLabels pins the Prow markers an approval reports so the
// dashboard does not claim the relayed `/approve` counted for Prow.
func TestProwManagedLabels(t *testing.T) {
	for _, tc := range []struct {
		labels []string
		want   bool
	}{
		{[]string{"size/M", "role/trusted"}, true},
		{[]string{"approved", "lgtm"}, true},
		{[]string{"do-not-merge/hold"}, true},
		{[]string{"needs-ok-to-test"}, true},
		{[]string{"lgtm", "role/trusted", PRNeedsDecisionStageLabel}, false},
		{nil, false},
	} {
		if got := prowManagedLabels(tc.labels); got != tc.want {
			t.Errorf("prowManagedLabels(%v) = %v, want %v", tc.labels, got, tc.want)
		}
	}
}

// TestRecordOperatorPullRequestApprovalWithoutOptionsStaysCommentsOnly pins
// the delegated "Let hive decide" path: it recommends, it does not clear a
// sentinel alert or record a human decision.
func TestRecordOperatorPullRequestApprovalWithoutOptionsStaysCommentsOnly(t *testing.T) {
	flagged := parkedPR(liveDecisionPR, "thatguywob", []string{PRNeedsDecisionStageLabel, issueNeedsHumanLabel, "sentinel-alert"})
	c, rec := newUnparkPRClient(t, []unparkWireIssue{flagged}, nil)

	event, err := c.RecordOperatorPullRequestApproval(context.Background(), "org/repo", liveDecisionPR, "maintainer")
	if err != nil {
		t.Fatalf("approval: %v", err)
	}
	if event.Recorded || event.NeedsHumanLifted || event.SentinelLabelCleared || event.SentinelAcknowledged || event.Stage != "" {
		t.Fatalf("bare approval must release nothing: %+v", event)
	}
	if len(rec.removed[liveDecisionPR]) != 0 || len(prDecisionFacts(t, c)) != 0 {
		t.Fatalf("bare approval touched labels or the stage engine: removed=%v", rec.removed[liveDecisionPR])
	}
}
