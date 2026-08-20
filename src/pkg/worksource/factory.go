package worksource

import (
	"fmt"
	"log/slog"

	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/github"
)

// FromConfig constructs the WorkSource for a governor from its WorkSourceConfig.
// When cfg.Type is "" or "github", returns a githubIssuesSource wrapping the
// existing ghClient — no config change needed for existing hives.
func FromConfig(cfg config.WorkSourceConfig, ghClient *github.Client, ghToken, ghOrg string, logger *slog.Logger) (WorkSource, error) {
	switch cfg.Type {
	case "", "github":
		return NewGitHubIssuesSource(ghClient), nil
	case "github_projects":
		c := cfg.GitHubProjects
		return NewGitHubProjectsSource(GitHubProjectsConfig{
			Token:          ghToken,
			Org:            coalesce(c.Org, ghOrg),
			ProjectNumber:  c.ProjectNumber,
			States:         c.States,
			PriorityField:  c.PriorityField,
			IterationField: c.IterationField,
			DefaultRepo:    c.DefaultRepo,
		}), nil
	case "linear":
		c := cfg.Linear
		teams := make([]LinearTeamConfig, len(c.Teams))
		for i, t := range c.Teams {
			teams[i] = LinearTeamConfig{Key: t.Key, Repo: t.Repo, States: t.States}
		}
		return NewLinearSource(LinearConfig{
			APIKey:     c.APIKey,
			Teams:      teams,
			HoldLabels: c.HoldLabels,
		}, nil), nil
	case "jira":
		c := cfg.Jira
		return NewJiraSource(JiraConfig{
			BaseURL:     c.BaseURL,
			Email:       c.Email,
			APIToken:    c.APIToken,
			ProjectKeys: c.ProjectKeys,
			JQL:         c.JQL,
			Repo:        c.Repo,
			HoldLabels:  c.HoldLabels,
		}), nil
	default:
		return nil, fmt.Errorf("unknown work_source type %q (want github, github_projects, linear, or jira)", cfg.Type)
	}
}

func coalesce(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
