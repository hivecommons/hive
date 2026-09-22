package taskmcp

import (
	"testing"
	"time"
)

func TestBuildCIHealthCountsFailuresLastGreenAndStaleness(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	data, _ := BuildCIHealth(CICacheInput{
		RequiredChecks: []string{"build", "test"},
		DefaultBranch:  "v6",
		States:         map[string]string{"build": "failure", "test": "success"},
		FailingPRs:     map[string]int{"build": 2},
		LastGreenSHA:   map[string]string{"build": "abc"},
		CachedAt:       now.Add(-time.Hour),
		PollInterval:   5 * time.Minute,
	}, now, PageRequest{Limit: 20})
	if !data.CacheStale || data.CacheAgeSecs == 0 || len(data.Checks) != 2 {
		t.Fatalf("ci health = %#v", data)
	}
	if data.Checks[0].Name != "build" || data.Checks[0].FailingPRs != 2 || data.Checks[0].LastGreenSHA != "abc" || data.Checks[0].State != "failure" {
		t.Fatalf("build check = %#v", data.Checks[0])
	}
}

func TestBuildCIHealthTreatsProwApprovalAsPending(t *testing.T) {
	data, _ := BuildCIHealth(CICacheInput{RequiredChecks: []string{"tide needs lgtm/approve"}, States: map[string]string{"tide needs lgtm/approve": "failure"}}, time.Now(), PageRequest{})
	if len(data.Checks) != 1 || data.Checks[0].State != "pending" || !data.Checks[0].PendingApproval {
		t.Fatalf("checks = %#v", data.Checks)
	}
}

func TestBuildCIHealthPaginates(t *testing.T) {
	data, page := BuildCIHealth(CICacheInput{RequiredChecks: []string{"a", "b", "c"}}, time.Now(), PageRequest{Limit: 2})
	if len(data.Checks) != 2 || !page.More || page.NextCursor == "" {
		t.Fatalf("checks=%#v page=%#v", data.Checks, page)
	}
}
