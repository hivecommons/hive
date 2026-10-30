package github

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// hivecommons/hive#15094: a human contributor's `issues.opened` delivery
// for an issue the reporter-trust gate does not yet admit must still carry
// the row the full pass would list (reporter_triage), so the governor's
// snapshot — and the activity monitor — show the issue from its first
// delivery rather than from the next enumeration.
func TestClassifyWebhookIssue_UnadmittedReporterCarriesTriageRow(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.NotFoundHandler()), "o", []string{"r"})
	c.SetIssueFilter(enabledReporterTrust())
	issue := &gh.Issue{Number: gh.Ptr(15092), State: gh.Ptr("open"), Title: gh.Ptr("Add a thing"),
		User: &gh.User{Login: gh.Ptr("kanrenibm")}, AuthorAssociation: gh.Ptr("CONTRIBUTOR"),
		Labels:    []*gh.Label{{Name: gh.Ptr("enhancement")}, {Name: gh.Ptr("help wanted")}},
		CreatedAt: &gh.Timestamp{Time: time.Now().Add(-time.Minute)}}
	p := c.classifyWebhookIssue("o/r", issue, 0, issueLabelNames(issue.Labels), time.Now())
	if p.Bucket != WebhookBucketUnknown {
		t.Fatalf("bucket = %q, want unknown (placement stays with the reconcile pass)", p.Bucket)
	}
	if p.WorkIssue == nil || p.WorkIssue.Bucket != workBucketReporterTriage || p.WorkIssue.Reason != reporterTriageAwaitingReason || p.WorkIssue.Number != 15092 {
		t.Fatalf("work row = %+v, want a reporter_triage row for #15092", p.WorkIssue)
	}
	if !p.WorkIssue.AuthorIsHuman {
		t.Fatalf("work row AuthorIsHuman = false, want the reporter's kind carried on the sidelined row")
	}
}

// An unknown-bucket read for an item no list carries inserts the row the
// classifier offered into the sidelined set and counts it open; a read
// with nothing to offer stays a no-op.
func TestApplyWebhookItem_UnlistedUnknownInsertsOfferedRow(t *testing.T) {
	r := patchFixture()
	row := RepoWorkIssue{Repo: "org/repo", Number: 15092, Title: "Add a thing", Author: "kanrenibm", Labels: []string{"help wanted"}, Bucket: workBucketReporterTriage, Reason: reporterTriageAwaitingReason}
	if !r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 15092, State: "open", Title: "Add a thing", Labels: []string{"help wanted"},
		Patch: WebhookItemPatch{Bucket: WebhookBucketUnknown, WorkIssue: &row}}) {
		t.Fatal("an opened issue with an offered row must be inserted")
	}
	var got *RepoWorkIssue
	for i, w := range r.WorkDetailsByRepo["org/repo"].Issues {
		if w.Number == 15092 {
			got = &r.WorkDetailsByRepo["org/repo"].Issues[i]
		}
	}
	if got == nil || got.Bucket != workBucketReporterTriage {
		t.Fatalf("sidelined rows = %+v, want #15092 in reporter_triage", r.WorkDetailsByRepo["org/repo"].Issues)
	}
	if r.TotalByRepo["org/repo"].Issues != 5 {
		t.Fatalf("open count = %d, want 5 (the new issue counted once)", r.TotalByRepo["org/repo"].Issues)
	}
	if r.Issues.Count != 2 {
		t.Fatalf("actionable count moved to %d; a reporter_triage row is not actionable", r.Issues.Count)
	}
	if r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 15093, State: "open", Title: "nothing offered"}) {
		t.Fatal("an unknown read with no row to offer must stay a no-op")
	}
	if r.TotalByRepo["org/repo"].Issues != 5 {
		t.Fatalf("no-op moved the count to %d", r.TotalByRepo["org/repo"].Issues)
	}
}
