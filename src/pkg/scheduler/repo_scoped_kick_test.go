package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestBuildKickMessagesRepoScopedTargetCarriesAndFiltersRepo(t *testing.T) {
	s := newScheduler()
	actionable := &github.ActionableResult{
		Issues: github.IssueResult{Items: []github.Issue{
			{Repo: "test-org/console", Number: 1, Title: "console work"},
			{Repo: "test-org/docs", Number: 2, Title: "docs work"},
		}},
		PRs: github.PRResult{
			Items: []github.PullRequest{
				{Repo: "test-org/console", Number: 3, Title: "console pr"},
				{Repo: "test-org/docs", Number: 4, Title: "docs pr"},
			},
			StaleDrafts: []github.PullRequest{
				{Repo: "test-org/console", Number: 5, Title: "console draft"},
				{Repo: "test-org/docs", Number: 6, Title: "docs draft"},
			},
		},
		TotalByRepo: map[string]github.RepoCounts{"test-org/console": {Issues: 1, PRs: 2}},
	}

	msgs := s.BuildKickMessages(actionable, []string{config.CadenceTargetKey("scanner", "test-org/console")})
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	if msgs[0].Agent != "scanner" || msgs[0].Repo != "test-org/console" {
		t.Fatalf("message target = agent %q repo %q, want scanner/test-org/console", msgs[0].Agent, msgs[0].Repo)
	}
	if got, want := msgs[0].IssueRefs, []string{"test-org/console#1"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("IssueRefs = %v, want %v", got, want)
	}
	if !strings.Contains(msgs[0].Message, "REPO-SCOPED CADENCE TARGET: test-org/console") {
		t.Fatalf("repo-scoped target preamble missing from message")
	}
	for _, forbidden := range []string{"docs work", "docs pr"} {
		if strings.Contains(msgs[0].Message, forbidden) {
			t.Fatalf("repo-scoped message leaked %q: %s", forbidden, msgs[0].Message)
		}
	}
	filtered := actionableForRepo(actionable, "test-org/console")
	if len(filtered.PRs.StaleDrafts) != 1 || filtered.PRs.StaleDrafts[0].Number != 5 {
		t.Fatalf("repo-scoped actionable stale drafts = %v, want only console draft", filtered.PRs.StaleDrafts)
	}
}
