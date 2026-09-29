package config

import (
	"fmt"
	"reflect"
	"strings"
)

// WorkSourceConfig selects where hive reads work items (Step 01 of the loop).
// Absent or type="" defaults to GitHub Issues — backward-compatible for all
// existing hives.
type WorkSourceConfig struct {
	// Type selects the work source: "" | "github" | "github_projects" | "linear" | "jira" | "gitea" | "gitlab"
	Type string `yaml:"type" json:"type"`
	// RunStages appends pending long-running run stages as non-issue-shaped work
	// items. Default false preserves byte-identical ListIssues output.
	RunStages bool `yaml:"run_stages,omitempty" json:"run_stages,omitempty"`
	// GitHubProjects configures the GitHub Projects v2 adapter.
	GitHubProjects GitHubProjectsSourceConfig `yaml:"github_projects,omitempty" json:"github_projects,omitempty"`
	// Linear configures the Linear GraphQL adapter.
	Linear LinearSourceConfig `yaml:"linear,omitempty" json:"linear,omitempty"`
	// Jira configures the Jira Cloud or Jira Data Center REST adapter.
	Jira JiraSourceConfig `yaml:"jira,omitempty" json:"jira,omitempty"`
	// Gitea configures the Gitea/Forgejo REST work source.
	Gitea GiteaSourceConfig `yaml:"gitea,omitempty" json:"gitea,omitempty"`
	// GitLab configures the GitLab REST work source.
	GitLab GitLabSourceConfig `yaml:"gitlab,omitempty" json:"gitlab,omitempty"`
	// Wavefront appends the ready nodes of an imported, versioned migration
	// graph (Crustify/Wavefront) as run-stage work items. Default disabled
	// preserves byte-identical ListIssues output.
	Wavefront WavefrontSourceConfig `yaml:"wavefront,omitempty" json:"wavefront,omitempty"`
}

// WavefrontSourceConfig configures the additive Wavefront migration-graph work
// source (hivecommons/hive#8362). Wavefront stays authoritative for its
// semantic graph: Hive reads the graph, lists its ready nodes, and records
// receipts when a node completes. It never re-derives the graph with an LLM.
type WavefrontSourceConfig struct {
	// Enabled turns the source on. Default false: nothing is read or listed.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Path is a local JSON file holding the versioned migration graph. Exactly
	// one of Path or URL must be set when Enabled is true.
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
	// URL is an HTTP(S) endpoint that serves the same JSON document.
	URL string `yaml:"url,omitempty" json:"url,omitempty"`
	// Repo is the owner/name repository the migration happens against. It
	// scopes every node key ("owner/name!<graph>:<node>").
	Repo string `yaml:"repo,omitempty" json:"repo,omitempty"`
	// ReceiptsDir is where node-completion receipts are written. Empty means
	// receipts are kept only in memory for the life of the process.
	ReceiptsDir string `yaml:"receipts_dir,omitempty" json:"receipts_dir,omitempty"`
}

// Validate checks the Wavefront source block. A disabled block is always valid
// so an operator can stage settings before switching the source on.
func (w WavefrontSourceConfig) Validate() error {
	if !w.Enabled {
		return nil
	}
	graphPath := strings.TrimSpace(w.Path)
	endpoint := strings.TrimSpace(w.URL)
	switch {
	case graphPath == "" && endpoint == "":
		return fmt.Errorf("work_source.wavefront: one of path or url is required when enabled")
	case graphPath != "" && endpoint != "":
		return fmt.Errorf("work_source.wavefront: path and url are mutually exclusive")
	}
	if endpoint != "" && !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		return fmt.Errorf("work_source.wavefront: url must start with http:// or https://")
	}
	if strings.TrimSpace(w.Repo) == "" {
		return fmt.Errorf("work_source.wavefront: repo (owner/name) is required when enabled")
	}
	return nil
}

// IsZero reports whether no work source has been configured at all: no type
// and no adapter-specific settings. Used by the dashboard-overlay reload to
// decide whether the overlay carries an operator-set work source.
func (w WorkSourceConfig) IsZero() bool {
	return w.Type == "" &&
		!w.RunStages &&
		reflect.DeepEqual(w.GitHubProjects, GitHubProjectsSourceConfig{}) &&
		reflect.DeepEqual(w.Linear, LinearSourceConfig{}) &&
		reflect.DeepEqual(w.Jira, JiraSourceConfig{}) &&
		reflect.DeepEqual(w.Gitea, GiteaSourceConfig{}) &&
		reflect.DeepEqual(w.GitLab, GitLabSourceConfig{}) &&
		reflect.DeepEqual(w.Wavefront, WavefrontSourceConfig{})
}

// GitHubProjectsSourceConfig configures the GitHub Projects v2 work source.
type GitHubProjectsSourceConfig struct {
	ProjectNumber  int      `yaml:"project_number" json:"project_number"`
	Org            string   `yaml:"org,omitempty" json:"org,omitempty"`
	States         []string `yaml:"states,omitempty" json:"states,omitempty"`
	PriorityField  string   `yaml:"priority_field,omitempty" json:"priority_field,omitempty"`
	IterationField string   `yaml:"iteration_field,omitempty" json:"iteration_field,omitempty"`
	DefaultRepo    string   `yaml:"default_repo,omitempty" json:"default_repo,omitempty"`
}

// LinearSourceConfig configures the Linear GraphQL work source.
type LinearSourceConfig struct {
	APIKey     string                   `yaml:"api_key,omitempty" json:"api_key,omitempty"`
	Teams      []LinearTeamSourceConfig `yaml:"teams,omitempty" json:"teams,omitempty"`
	HoldLabels []string                 `yaml:"hold_labels,omitempty" json:"hold_labels,omitempty"`
	// AssignedOnly narrows enumeration to issues assigned/delegated to the
	// installed Linear agent app (RFC #4492 Part 2, component E). Opt-in and
	// fail-closed: it requires the agent to be connected (the app user id is
	// learned at install time), and worksource construction errors when it is
	// set without an install rather than silently enumerating everything.
	AssignedOnly bool `yaml:"assigned_only,omitempty" json:"assigned_only,omitempty"`
	// SessionAgent names the hive agent that receives Linear agent sessions
	// (delegations and mentions). When empty and exactly one agent is
	// configured, that agent is used; otherwise session events are
	// acknowledged with an error activity naming the missing config.
	SessionAgent string `yaml:"session_agent,omitempty" json:"session_agent,omitempty"`
	// Transitions maps Hive design status names to Linear workflow state names
	// or ids. When a status is not present, the status string itself is used.
	Transitions map[string]string `yaml:"transitions,omitempty" json:"transitions,omitempty"`
}

// LinearTeamSourceConfig maps one Linear team to the GitHub repo agents work in.
type LinearTeamSourceConfig struct {
	Key      string                      `yaml:"key" json:"key"`
	Repo     string                      `yaml:"repo" json:"repo"`
	States   []string                    `yaml:"states,omitempty" json:"states,omitempty"`
	Projects []LinearProjectSourceConfig `yaml:"projects,omitempty" json:"projects,omitempty"`
	Cycles   string                      `yaml:"cycles,omitempty" json:"cycles,omitempty"`
}

type LinearProjectSourceConfig struct {
	Name string `yaml:"name" json:"name"`
	Repo string `yaml:"repo,omitempty" json:"repo,omitempty"`
}

// JiraSourceConfig configures the Jira Cloud or Jira Data Center work source.
type JiraSourceConfig struct {
	Deployment         string            `yaml:"deployment,omitempty" json:"deployment,omitempty"`
	BaseURL            string            `yaml:"base_url" json:"base_url"`
	Email              string            `yaml:"email" json:"email"`
	Username           string            `yaml:"username,omitempty" json:"username,omitempty"`
	APIToken           string            `yaml:"api_token,omitempty" json:"api_token,omitempty"`
	Password           string            `yaml:"password,omitempty" json:"password,omitempty"`
	CABundle           string            `yaml:"ca_bundle,omitempty" json:"ca_bundle,omitempty"`
	InsecureSkipVerify bool              `yaml:"insecure_skip_verify,omitempty" json:"insecure_skip_verify,omitempty"`
	ClientCert         string            `yaml:"client_cert,omitempty" json:"client_cert,omitempty"`
	ClientKey          string            `yaml:"client_key,omitempty" json:"client_key,omitempty"`
	ProjectKeys        []string          `yaml:"project_keys,omitempty" json:"project_keys,omitempty"`
	JQL                string            `yaml:"jql,omitempty" json:"jql,omitempty"`
	Repo               string            `yaml:"repo,omitempty" json:"repo,omitempty"`
	HoldLabels         []string          `yaml:"hold_labels,omitempty" json:"hold_labels,omitempty"`
	Transitions        map[string]string `yaml:"transitions,omitempty" json:"transitions,omitempty"`
}

// ForgeWorkRepoSourceConfig maps one forge project/repository to the repository
// Hive agents should clone. WorkRepo defaults to Repo.
type ForgeWorkRepoSourceConfig struct {
	Repo     string `yaml:"repo" json:"repo"`
	WorkRepo string `yaml:"work_repo,omitempty" json:"work_repo,omitempty"`
}

// GiteaSourceConfig configures the Gitea/Forgejo work-source adapter.
type GiteaSourceConfig struct {
	BaseURL    string                      `yaml:"base_url,omitempty" json:"base_url,omitempty"`
	Token      string                      `yaml:"token,omitempty" json:"token,omitempty"`
	TokenEnv   string                      `yaml:"token_env,omitempty" json:"token_env,omitempty"`
	Org        string                      `yaml:"org,omitempty" json:"org,omitempty"`
	Repos      []ForgeWorkRepoSourceConfig `yaml:"repos,omitempty" json:"repos,omitempty"`
	States     []string                    `yaml:"states,omitempty" json:"states,omitempty"`
	Labels     []string                    `yaml:"labels,omitempty" json:"labels,omitempty"`
	Assignee   string                      `yaml:"assignee,omitempty" json:"assignee,omitempty"`
	HoldLabels []string                    `yaml:"hold_labels,omitempty" json:"hold_labels,omitempty"`
}

// GitLabSourceConfig configures the GitLab work-source adapter.
type GitLabSourceConfig struct {
	BaseURL    string                      `yaml:"base_url,omitempty" json:"base_url,omitempty"`
	Token      string                      `yaml:"token,omitempty" json:"token,omitempty"`
	TokenEnv   string                      `yaml:"token_env,omitempty" json:"token_env,omitempty"`
	Org        string                      `yaml:"org,omitempty" json:"org,omitempty"`
	Repos      []ForgeWorkRepoSourceConfig `yaml:"repos,omitempty" json:"repos,omitempty"`
	States     []string                    `yaml:"states,omitempty" json:"states,omitempty"`
	Labels     []string                    `yaml:"labels,omitempty" json:"labels,omitempty"`
	Assignee   string                      `yaml:"assignee,omitempty" json:"assignee,omitempty"`
	HoldLabels []string                    `yaml:"hold_labels,omitempty" json:"hold_labels,omitempty"`
}

// Validate checks work-source adapter-specific fields that can be validated
// without contacting the external service.
func (w WorkSourceConfig) Validate() error {
	switch strings.TrimSpace(w.Type) {
	case "", "github", "github_projects", "linear", "jira":
		return nil
	case "gitea":
		if strings.TrimSpace(w.Gitea.BaseURL) == "" {
			return fmt.Errorf("work_source.gitea.base_url is required")
		}
		return validateForgeWorkRepos("work_source.gitea.repos", w.Gitea.Repos)
	case "gitlab":
		return validateForgeWorkRepos("work_source.gitlab.repos", w.GitLab.Repos)
	default:
		return fmt.Errorf("unknown work_source type %q (want github, github_projects, linear, jira, gitea, or gitlab)", w.Type)
	}
}

func validateForgeWorkRepos(field string, repos []ForgeWorkRepoSourceConfig) error {
	if len(repos) == 0 {
		return fmt.Errorf("%s must contain at least one repository", field)
	}
	for i, repo := range repos {
		if strings.TrimSpace(repo.Repo) == "" {
			return fmt.Errorf("%s[%d].repo is required", field, i)
		}
	}
	return nil
}
