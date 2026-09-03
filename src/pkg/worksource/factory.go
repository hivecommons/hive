package worksource

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/linearagent"
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
		apiKey, err := resolveSecretRef("work_source.linear.api_key", c.APIKey)
		if err != nil {
			return nil, err
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
			APIKey:     apiKey,
			Teams:      teams,
			HoldLabels: c.HoldLabels,
			ViewerID:   viewerID,
			Logger:     logger,
		}, nil), nil
	case "jira":
		c := cfg.Jira
		apiToken, err := resolveSecretRef("work_source.jira.api_token", c.APIToken)
		if err != nil {
			return nil, err
		}
		return NewJiraSource(JiraConfig{
			BaseURL:     c.BaseURL,
			Email:       c.Email,
			APIToken:    apiToken,
			ProjectKeys: c.ProjectKeys,
			JQL:         c.JQL,
			Repo:        c.Repo,
			HoldLabels:  c.HoldLabels,
		}), nil
	default:
		return nil, fmt.Errorf("unknown work_source type %q (want github, github_projects, linear, or jira)", cfg.Type)
	}
}

// secretRefPattern matches a credential written as a whole-value environment
// reference: `${LINEAR_API_KEY}` or `$LINEAR_API_KEY`.
var secretRefPattern = regexp.MustCompile(`^\$(?:\{([A-Za-z_][A-Za-z0-9_]*)\}|([A-Za-z_][A-Za-z0-9_]*))$`)

// resolveSecretRef resolves a work-source credential at the point of use.
//
// hive.yaml documents `api_key: ${LINEAR_API_KEY}`. Values loaded from the
// file are env-expanded by config.Load, but a value saved from the dashboard
// (PUT /api/config/governor/work-source) is stored verbatim, so the adapter
// used to send the literal string `${LINEAR_API_KEY}` as its Authorization
// header and got a 401. Resolving here — rather than at save time — keeps the
// secret out of the persisted config: the overlay only ever holds the
// reference. An unset or empty variable is a clear configuration error, never
// a literal header. Anything that is not a whole-value reference is returned
// unchanged.
func resolveSecretRef(field, raw string) (string, error) {
	m := secretRefPattern.FindStringSubmatch(raw)
	if m == nil {
		return raw, nil
	}
	name := m[1]
	if name == "" {
		name = m[2]
	}
	val, ok := os.LookupEnv(name)
	if !ok || val == "" {
		return "", fmt.Errorf("%s references environment variable %s, which is not set in the hive's environment", field, name)
	}
	return val, nil
}

func coalesce(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
