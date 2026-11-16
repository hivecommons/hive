package github

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// techdocsPRReplyProfile is the techdocs test model with a label mapped to
// the PR state `approved`, so `approve` at draft-review (PR stage 050) both
// moves the item and applies a label.
func techdocsPRReplyProfile(t *testing.T) *workflowmodel.Profile {
	t.Helper()
	b, err := os.ReadFile("../workflowmodel/testdata/models/techdocs.json")
	if err != nil {
		t.Fatal(err)
	}
	var m workflowmodel.Model
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	m.Spec.Labels = workflowmodel.LabelSet{Prefix: "docs", Entries: []workflowmodel.LabelEntry{{Name: "docs/090-approved", State: "approved"}}}
	p, _, err := workflowmodel.Compile(&m)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSweepModelReplyOnPullRequestState(t *testing.T) {
	pr := unparkWireIssue{
		Number:      12,
		Title:       "docs draft",
		User:        wireUser{Login: "author"},
		Labels:      []wireLabel{{Name: stagelabels.ReviewPending.Label()}},
		Comments:    []unparkWireComment{humanComment(21, "maintainer", "/hive approve ship it")},
		PullRequest: &unparkWirePRLink{URL: "https://example.test/pulls/12"},
	}
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{pr}, map[string]string{"maintainer": "write"})
	c := newTestClient(t, server, "org", []string{"repo"})
	p := techdocsPRReplyProfile(t)

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{
		WorkflowFor: func(string, string) *workflowmodel.Profile { return p },
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := strings.Join(rec.added[12], ","); got != "docs/090-approved" {
		t.Fatalf("expected the mapped model label added to PR 12, got %v", rec.added[12])
	}
	if len(result.ModelReplies) != 1 || result.ModelReplies[0].Number != 12 || result.ModelReplies[0].Decision != "approve: ship it" {
		t.Fatalf("expected the model reply on PR 12 recorded, got %+v", result.ModelReplies)
	}
	if len(result.Unparked) != 0 {
		t.Fatalf("a PR reply is not an un-park: %+v", result.Unparked)
	}
	if len(rec.removed[12]) != 0 {
		t.Fatalf("a model reply off the parked path must remove nothing, got %v", rec.removed[12])
	}
	joined := strings.Join(rec.posted[12], "\n")
	if !strings.Contains(joined, "Workflow reply `approve` moves the item from `draft-review` to `approved`; `docs/090-approved` applied.") {
		t.Fatalf("reply = %v", rec.posted[12])
	}
	if !strings.Contains(joined, unparkReplyMarkerPrefix+"21 -->") || strings.Contains(joined, "Un-parked") {
		t.Fatalf("reply must answer comment 21 without claiming an un-park, got %v", rec.posted[12])
	}
}

func TestSweepModelReplyOnUnparkedIssueFollowsCurrentState(t *testing.T) {
	// The issue sits at 020 (analysing), which is not parked; its verb
	// records only. Issue 9 sits there too but replies with a verb only
	// 045 offers.
	issue := parkedIssue(8)
	issue.Labels = []wireLabel{{Name: stagelabels.Claimed.Label()}}
	issue.Comments = []unparkWireComment{humanComment(31, "maintainer", "/hive hand back over to docs team")}
	other := parkedIssue(9, humanComment(41, "maintainer", "/hive revise"))
	other.Labels = []wireLabel{{Name: stagelabels.Claimed.Label()}}
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue, other}, map[string]string{"maintainer": "write"})
	c := newTestClient(t, server, "org", []string{"repo"})
	p := techdocsReplyProfile(t)

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{
		WorkflowFor: func(string, string) *workflowmodel.Profile { return p },
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.ModelReplies) != 1 || result.ModelReplies[0].Number != 8 || result.ModelReplies[0].Decision != "hand back: over to docs team" {
		t.Fatalf("expected issue 8's reply recorded, got %+v", result.ModelReplies)
	}
	joined := strings.Join(rec.posted[8], "\n")
	if !strings.Contains(joined, "Recorded for @maintainer. Decision: hand back: over to docs team.") || !strings.Contains(joined, "Workflow reply `hand back` is recorded; the model declares no transition for it.") {
		t.Fatalf("reply = %v", rec.posted[8])
	}
	if len(rec.added[8])+len(rec.removed[8]) != 0 {
		t.Fatalf("a record-only reply changes no labels, got added=%v removed=%v", rec.added[8], rec.removed[8])
	}
	if len(rec.posted[9])+len(rec.added[9]) != 0 {
		t.Fatalf("a verb the current state does not offer must be ignored, got posted=%v added=%v", rec.posted[9], rec.added[9])
	}

	// Unbound: the same comment is no command at all.
	server2, rec2 := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, map[string]string{"maintainer": "write"})
	c2 := newTestClient(t, server2, "org", []string{"repo"})
	result, err = c2.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("unbound sweep: %v", err)
	}
	if len(result.ModelReplies) != 0 || len(rec2.posted[8]) != 0 {
		t.Fatalf("unbound repo must not answer a model verb: %+v posted=%v", result.ModelReplies, rec2.posted[8])
	}
}
