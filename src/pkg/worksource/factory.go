package worksource

import (
	"fmt"
	"log/slog"

	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/linearagent"
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
		if c.APIKey == "" {
			return nil, fmt.Errorf("work_source.linear.api_key is required")
		}
		if len(c.Teams) == 0 {
			return nil, fmt.Errorf("work_source.linear.teams must contain at least one team")
		}
		teams := make([]LinearTeamConfig, len(c.Teams))
		for i, t := range c.Teams {
			if t.Key == "" {
				return nil, fmt.Errorf("work_source.linear.teams[%d].key is required", i)
			}
			if t.Repo == "" {
				return nil, fmt.Errorf("work_source.linear.teams[%d].repo is required", i)
			}
			if t.Cycles != "" && t.Cycles != "current" {
				return nil, fmt.Errorf("work_source.linear.teams[%d].cycles = %q (want empty or current)", i, t.Cycles)
			}
			projects := make([]LinearProjectConfig, len(t.Projects))
			for j, p := range t.Projects {
				if p.Name == "" {
					return nil, fmt.Errorf("work_source.linear.teams[%d].projects[%d].name is required", i, j)
				}
				projects[j] = LinearProjectConfig{Name: p.Name, Repo: p.Repo}
			}
			teams[i] = LinearTeamConfig{Key: t.Key, Repo: t.Repo, States: t.States, Projects: projects, Cycles: t.Cycles}
		}
		viewerID := ""
		if c.AssignedOnly {
			// Fail closed: assigned_only without a connected Linear agent
			// cannot mean "enumerate everything" — that would silently hand
			// agents the whole backlog the operator asked to narrow.
			viewerID = linearagent.StoredViewerID(linearagent.DefaultStorePath())
			if viewerID == "" {
				return nil, fmt.Errorf("work_source.linear.assigned_only requires the Linear agent to be connected (no install found at %s)", linearagent.DefaultStorePath())
			}
		}
		return NewLinearSource(LinearConfig{
			APIKey:     c.APIKey,
			Teams:      teams,
			HoldLabels: c.HoldLabels,
			ViewerID:   viewerID,
			Logger:     logger,
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
