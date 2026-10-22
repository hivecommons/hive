package github

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRecordOperatorIssueDecisionApproveClearsParkAndReplies(t *testing.T) {
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{parkedIssue(7)}, map[string]string{"maintainer": "write"})
	c := newTestClient(t, server, "org", []string{"repo"})

	event, err := c.RecordOperatorIssueDecision(context.Background(), "org/repo", 7, "maintainer", "")
	if err != nil {
		t.Fatalf("decision: %v", err)
	}
	if event.Actor != "maintainer" || event.Number != 7 || event.Decision != "go with the recommendation" {
		t.Fatalf("event = %+v", event)
	}
	removed := strings.Join(rec.removed[7], ",")
	if !strings.Contains(removed, "needs-decision") {
		t.Fatalf("expected the decision park label removed, got %v", rec.removed[7])
	}
	// A maintainer's decision is the human input `needs-human` asked for
	// (#13568), so it comes off with the park label; `hold` is never touched.
	if !strings.Contains(removed, "needs-human") {
		t.Fatalf("needs-human is answered by the maintainer's decision and comes off, got %v", rec.removed[7])
	}
	if strings.Contains(removed, "hold") {
		t.Fatalf("hold must be left alone, got %v", rec.removed[7])
	}
	if len(rec.added[7]) != 1 || rec.added[7][0] != HumanAckLabel {
		t.Fatalf("expected %s added, got %v", HumanAckLabel, rec.added[7])
	}
	joined := strings.Join(rec.posted[7], "\n")
	if !strings.Contains(joined, "Un-parked by @maintainer") || !strings.Contains(joined, "hive dashboard") {
		t.Fatalf("expected a dashboard-attributed acceptance reply, got %v", rec.posted[7])
	}
	if !strings.Contains(joined, unparkReplyMarkerPrefix+"0 -->") {
		t.Fatalf("reply must carry the shared unpark marker with a non-command id, got %v", rec.posted[7])
	}
}

func TestRecordOperatorIssueDecisionRecordsDecisionText(t *testing.T) {
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{parkedIssue(8)}, nil)
	c := newTestClient(t, server, "org", []string{"repo"})

	event, err := c.RecordOperatorIssueDecision(context.Background(), "org/repo", 8, "maintainer", "  keep 10 stable tags ")
	if err != nil {
		t.Fatalf("decision: %v", err)
	}
	if event.Decision != "keep 10 stable tags" {
		t.Fatalf("decision = %q", event.Decision)
	}
	if !strings.Contains(strings.Join(rec.posted[8], "\n"), "Decision: keep 10 stable tags") {
		t.Fatalf("expected the decision in the reply, got %v", rec.posted[8])
	}
}

func TestRecordOperatorIssueDecisionRefusesUnparkedAndPRs(t *testing.T) {
	plain := parkedIssue(9)
	plain.Labels = []wireLabel{{Name: "bug"}}
	pr := parkedIssue(10)
	pr.PullRequest = &unparkWirePRLink{URL: "https://api.example.test/pulls/10"}
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{plain, pr}, nil)
	c := newTestClient(t, server, "org", []string{"repo"})

	if _, err := c.RecordOperatorIssueDecision(context.Background(), "org/repo", 9, "maintainer", ""); !errors.Is(err, ErrIssueNotParked) {
		t.Fatalf("un-parked issue: err = %v, want ErrIssueNotParked", err)
	}
	if _, err := c.RecordOperatorIssueDecision(context.Background(), "org/repo", 10, "maintainer", ""); !errors.Is(err, ErrDecisionTargetIsPR) {
		t.Fatalf("pull request: err = %v, want ErrDecisionTargetIsPR", err)
	}
	if _, err := c.RecordOperatorIssueDecision(context.Background(), "org/repo", 9, "", ""); err == nil {
		t.Fatal("blank actor must be refused")
	}
	if len(rec.added[9])+len(rec.added[10])+len(rec.posted[9])+len(rec.posted[10]) != 0 {
		t.Fatalf("refused decisions must write nothing, got added=%v/%v posted=%v/%v", rec.added[9], rec.added[10], rec.posted[9], rec.posted[10])
	}
}
