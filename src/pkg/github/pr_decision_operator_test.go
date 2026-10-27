package github

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/review"
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
