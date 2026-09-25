package worksource

import (
	"context"
	"strings"
	"testing"
)

func TestLinearDesignInternalGuards(t *testing.T) {
	var nilSrc *LinearSource
	if _, _, err := nilSrc.designIssueLabels(context.Background(), Ref{ExternalID: "ENG-1"}); err == nil || !strings.Contains(err.Error(), "source unavailable") {
		t.Fatalf("nil designIssueLabels err = %v", err)
	}
	if got := repoBaseName(""); got != "" {
		t.Fatalf("repoBaseName empty = %q", got)
	}
	s := &LinearSource{cfg: LinearConfig{Teams: []LinearTeamConfig{{Key: "ENG", Repo: "org/app"}}}}
	related := linearRelatedIssue{}
	related.Team.Key = "OTHER"
	if got := s.linearRelatedRepo(LinearTeamConfig{Repo: "fallback/repo"}, related); got != "fallback/repo" {
		t.Fatalf("linearRelatedRepo fallback = %q", got)
	}
}
