package github

import (
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func TestPRCIActivityBindsHeadAndObservation(t *testing.T) {
	now := time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)
	checks := []*gh.CheckRun{
		nil,
		{Name: gh.Ptr("old failure"), HeadSHA: gh.Ptr("old"), Status: gh.Ptr("completed"), Conclusion: gh.Ptr("failure")},
		{Name: gh.Ptr("build"), HeadSHA: gh.Ptr("head"), Status: gh.Ptr("in_progress"), StartedAt: &gh.Timestamp{Time: now}, DetailsURL: gh.Ptr("https://github.com/org/repo/actions/runs/1")},
		{Name: gh.Ptr("optional"), Status: gh.Ptr("completed"), Conclusion: gh.Ptr("skipped"), CompletedAt: &gh.Timestamp{Time: now}},
	}
	a := prCIActivity("head", "github-check-runs", checks, &now)
	if a.Head != "head" || a.ObservedAt != &now || len(a.Checks) != 2 ||
		a.Checks[0].Name != "build" || a.Checks[0].Since == nil || !a.Checks[0].Since.Equal(now) {
		t.Fatalf("wrong head or timestamps: %+v", a)
	}
	a.setRequired(map[string]bool{"build": true, "optional": false}, true)
	if !a.RequiredKnown || len(a.Required) != 1 || a.Required[0] != "build" {
		t.Fatalf("optional check became required: %+v", a)
	}
	a.setRequired(nil, false)
	if a.RequiredKnown || len(a.Required) != 0 {
		t.Fatalf("unknown required set became known: %+v", a)
	}
}

func TestPRCIActivityGraphQLCacheObservationIsInvalidated(t *testing.T) {
	now := time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)
	key := prBatchCheckRunKey{repo: "org/repo", number: 7, headSHA: "head"}
	c := &Client{
		prBatchCheckRuns:       map[prBatchCheckRunKey][]*gh.CheckRun{key: {}},
		prBatchCheckObservedAt: map[prBatchCheckRunKey]time.Time{key: now},
	}
	if at := c.graphQLBatchCheckObservedAt("org/repo", 7, "head"); at == nil || !at.Equal(now) {
		t.Fatalf("cache observation time lost: %v", at)
	}
	if at := c.graphQLBatchCheckObservedAt("org/repo", 7, "old"); at != nil {
		t.Fatalf("old-head time leaked: %v", at)
	}
	c.invalidateGraphQLPRBatchPR("org/repo", 7)
	if at := c.graphQLBatchCheckObservedAt("org/repo", 7, "head"); at != nil {
		t.Fatalf("invalidated checks retained freshness: %v", at)
	}
	c.prBatchCheckRuns[key] = nil
	c.prBatchCheckObservedAt[key] = now
	c.clearGraphQLPRBatchRepo("org/repo")
	if at := c.graphQLBatchCheckObservedAt("org/repo", 7, "head"); at != nil {
		t.Fatalf("cleared repo retained freshness: %v", at)
	}
}
