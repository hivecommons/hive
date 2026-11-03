package dashboard

import (
	"fmt"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
)

func TestExecutionEligibilityRotatesPastUnreadyMembersIndependently(t *testing.T) {
	s := newTestServer()
	scope := config.IntegrationExecutionScope{Repo: "o/r", Base: "v5"}
	prs := make([]*gh.PullRequest, 2*integrationExecutionPreparationMembers)
	for index := range prs {
		prs[index] = &gh.PullRequest{Number: gh.Ptr(index + 1)}
	}
	seen := map[int]bool{}
	for range 2 {
		turn := s.executionEligibilityTurn(scope, prs)
		if len(turn) != integrationExecutionPreparationMembers {
			t.Fatalf("unbounded/incomplete probe turn: %d", len(turn))
		}
		for _, pr := range turn {
			if seen[pr.GetNumber()] {
				t.Fatal("an unready older cohort starved later members")
			}
			seen[pr.GetNumber()] = true
		}
	}
	if len(seen) != len(prs) {
		t.Fatal("bounded turns did not cover the discovered cohort")
	}
	scope.Base = "other"
	if got := s.executionEligibilityTurn(scope, prs)[0].GetNumber(); got != 1 {
		t.Fatalf("another destination inherited the first destination's cursor: %d", got)
	}
	if len(s.executionEligibilityTurn(scope, nil)) != 0 {
		t.Fatal("empty cohort generated work")
	}
	for index := range config.MaxIntegrationScopes + 2 {
		scope.Base = fmt.Sprintf("scope-%d", index)
		s.executionEligibilityTurn(scope, prs)
	}
	if len(s.integration.executionEligibilityOffsets) > config.MaxIntegrationScopes {
		t.Fatal("configuration churn grew discovery cursors without bounds")
	}
}
