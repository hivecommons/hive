package github

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/gutenprint-printer-app#45, ps-printer-app#50 and
// hplip-printer-app#42: mergeraptor's Dependency Dashboards wore stage/045
// because a triage bot added `needs-human`. A standing meta issue is not
// work and derives no stage: the sweep row and an out-of-cycle fact both
// take the 045 off and write nothing else.
func TestStandingMetaIssueDerivesNoStage(t *testing.T) {
	for _, bucket := range []string{"dependency_dashboard", "hive_advisory"} {
		in := IssueStageInput{Repo: "o/r", Number: 45, Labels: []string{"triage/accepted", "needs-human", stagelabels.IssueNeedsDecision.Label()}, Bucket: bucket}
		if d := issueStageDecision(in, nil); d.Stage != stagelabels.None || !strings.Contains(d.Reason, "standing meta issue") {
			t.Fatalf("%s: decision = %+v, want no stage", bucket, d)
		}
	}

	srv := newStageFactsServer(t, "triage/accepted", "kind/cleanup", "needs-human", stagelabels.IssueNeedsDecision.Label())
	srv.title, srv.login = "Dependency Dashboard", "mergeraptor[bot]"
	c := testClient(t, srv.srv.URL)
	if err := c.StageFacts().Parked(context.Background(), c.IssueStageRef("o/r", 7), "needs-human"); err != nil {
		t.Fatal(err)
	}
	labels, added, removed := srv.snapshot()
	if stagelabels.Current(labels) != stagelabels.None || len(added) != 0 || strings.Join(removed, ",") != stagelabels.IssueNeedsDecision.Label() {
		t.Fatalf("labels=%v added=%v removed=%v, want 045 removed and nothing added", labels, added, removed)
	}
	if !issueHasLabel(labels, "needs-human") {
		t.Fatalf("labels = %v: the bot's needs-human is not the hive's to remove", labels)
	}

	f, err := c.enrichStageFacts(context.Background(), stageengine.Facts{Ref: c.IssueStageRef("o/r", 7), Issue: &stageengine.IssueFacts{Bucket: stageengine.BucketActionable}})
	if err != nil {
		t.Fatal(err)
	}
	if f.Issue.Bucket != stageengine.BucketDependencyDashboard {
		t.Fatalf("bucket = %q, want dependency_dashboard", f.Issue.Bucket)
	}
}
