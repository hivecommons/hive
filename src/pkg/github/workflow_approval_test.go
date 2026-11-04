package github

import (
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func TestWorkflowApprovalFromRuns(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	runs := []*gh.WorkflowRun{
		nil,
		{ID: gh.Ptr(int64(1)), Status: gh.Ptr("completed"), Conclusion: gh.Ptr("success")},
		{ID: gh.Ptr(int64(2)), Status: gh.Ptr(WorkflowApprovalStatus), Actor: &gh.User{Login: gh.Ptr("dependabot[bot]")}},
		{ID: gh.Ptr(int64(3)), Status: gh.Ptr("completed"), Conclusion: gh.Ptr(WorkflowApprovalStatus)},
		{ID: gh.Ptr(int64(4)), Status: gh.Ptr("in_progress")},
	}
	got := workflowApprovalFromRuns(runs, now)
	if got == nil {
		t.Fatal("expected approval evidence")
	}
	if got.Pending != 2 || len(got.RunIDs) != 2 || got.RunIDs[0] != 2 || got.RunIDs[1] != 3 {
		t.Fatalf("pending=%d ids=%v", got.Pending, got.RunIDs)
	}
	if got.Actor != "dependabot[bot]" {
		t.Fatalf("actor=%q", got.Actor)
	}
	if got.ObservedAt == nil || !got.ObservedAt.Equal(now) {
		t.Fatalf("observed_at=%v", got.ObservedAt)
	}
	if workflowApprovalFromRuns(runs[:2], now) != nil {
		t.Fatal("no gated runs must yield nil, not an empty record")
	}
}

func TestCloneCIActivityCopiesWorkflowApproval(t *testing.T) {
	src := &PRCIActivity{Head: "abc", WorkflowApproval: &PRWorkflowApproval{Pending: 1, RunIDs: []int64{7}}}
	out := cloneCIActivity(src)
	out.WorkflowApproval.RunIDs[0] = 9
	out.WorkflowApproval.Pending = 5
	if src.WorkflowApproval.RunIDs[0] != 7 || src.WorkflowApproval.Pending != 1 {
		t.Fatal("clone shares workflow approval with its source")
	}
}

func TestWorkflowApprovalCacheEvictsAndForgets(t *testing.T) {
	now := time.Now()
	c := &Client{}
	c.workflowApprovals.probes = map[string]workflowApprovalEntry{
		"org/repo@old":   {at: now.Add(-2 * workflowApprovalCacheTTL)},
		"org/repo@fresh": {at: now},
	}
	c.workflowApprovals.evictStaleProbes(now)
	if _, ok := c.workflowApprovals.probes["org/repo@old"]; ok {
		t.Fatal("stale entry survived eviction")
	}
	if _, ok := c.workflowApprovals.probes["org/repo@fresh"]; !ok {
		t.Fatal("fresh entry evicted")
	}
	c.forgetWorkflowApproval("org/repo", "fresh")
	if _, ok := c.workflowApprovals.probes["org/repo@fresh"]; ok {
		t.Fatal("forget left the entry")
	}
}
