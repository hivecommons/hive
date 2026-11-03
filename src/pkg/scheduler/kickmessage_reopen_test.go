package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

func TestFormatIssueListReopenedAfterMergeTellsAgentToImplement(t *testing.T) {
	s := newScheduler()
	issues := []github.Issue{{
		Repo: "repo1", Number: 9945, Title: "mysterious failure", AgeMinutes: 15,
		ClaimContext: &github.IssueClaimContext{
			PRRepo: "repo1", PRNumber: 11758, PRURL: "https://github.com/org/repo1/pull/11758",
			Decision: github.ClaimDecisionReopenedAfterMerge,
		},
	}}
	result, _ := s.formatIssueListWithPolicy(issues)
	if !strings.Contains(result, "reopened after repo1#11758 merged (https://github.com/org/repo1/pull/11758)") {
		t.Fatalf("missing reopened-after-merge context: %s", result)
	}
	if !strings.Contains(result, "do NOT re-verify") || strings.Contains(result, "verify once") {
		t.Fatalf("reopened issue must be implemented, not re-verified: %s", result)
	}
}
