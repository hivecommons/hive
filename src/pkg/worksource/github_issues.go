package worksource

import (
	"context"
	"fmt"
	"strconv"

	"github.com/hivecommons/hive/pkg/github"
)

// githubIssuesSource is the default WorkSource: it delegates to the existing
// github.Client.EnumerateActionable, which handles hold labels, issue filters,
// SLA tracking, and PR exclusion. Zero config change for existing hives.
type githubIssuesSource struct {
	client *github.Client
}

// NewGitHubIssuesSource builds a WorkSource backed by the existing GitHub
// Issues enumeration.
func NewGitHubIssuesSource(client *github.Client) WorkSource {
	return &githubIssuesSource{client: client}
}

func (s *githubIssuesSource) SourceType() string { return "github" }

func (s *githubIssuesSource) ListIssues(ctx context.Context) ([]Issue, error) {
	res, err := s.client.EnumerateActionable(ctx)
	if err != nil {
		return nil, fmt.Errorf("worksource/github: enumerate: %w", err)
	}
	if res == nil {
		return nil, nil
	}
	out := make([]Issue, 0, len(res.Issues.Items))
	for _, it := range res.Issues.Items {
		out = append(out, Issue{
			SourceType: "github",
			Repo:       it.Repo,
			ExternalID: strconv.Itoa(it.Number),
			Number:     it.Number,
			Title:      it.Title,
			Author:     it.Author,
			Labels:     it.Labels,
			Assignees:  it.Assignees,
			State:      "open",
			CreatedAt:  it.CreatedAt,
			UpdatedAt:  it.UpdatedAt,
			URL:        it.URL,
		})
	}
	return out, nil
}
