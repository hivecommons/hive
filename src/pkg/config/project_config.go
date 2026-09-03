// Project configuration: repos/checkout layout, forge kind selection, work
// sources (GitHub Projects, Linear, Jira), and project observability.
package config

import (
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

type ProjectConfig struct {
	Org         string   `yaml:"org"`
	Name        string   `yaml:"name"`
	Repos       []string `yaml:"repos"`
	AIAuthor    string   `yaml:"ai_author"`
	PrimaryRepo string   `yaml:"primary_repo"`
	OpenPRs     *bool    `yaml:"open_prs,omitempty"`
	// Forge selects the source forge for this project: "github" (default) or
	// "gitlab". It is additive — an absent value means GitHub, so existing
	// GitHub-only configs are unaffected. Use ForgeKind() to read it with the
	// default applied.
	Forge string `yaml:"forge,omitempty"`
	// IssueFilter gates which open issues agents may initiate work on: the
	// require_labels allow-list ("only work issues labeled X"). The exclude
	// polarity is Governor.Labels.Exempt, which wins on conflict. Absent/empty
	// = no filtering, the pre-existing behavior. See IssueFilterConfig.
	IssueFilter IssueFilterConfig `yaml:"issue_filter,omitempty"`
	// CheckoutsDir is a host-local directory holding one checkout per monitored
	// repo, as "<CheckoutsDir>/<repo name>" — the bare name from Repos, without
	// the org. It is how an operator supplies the per-repo checkout root the
	// AGENTS.md convention needs (kubestellar/hive#5227): Hive agents work over
	// the API and keep no clones of their own, so without this there is no local
	// path for the scheduler to read a repo's AGENTS.md from.
	//
	// Optional and additive. Empty (the default) means no checkout root, which
	// is exactly the previous behavior — AGENTS.md injection stays a no-op. A
	// directory that is absent or holds no AGENTS.md is also a no-op; nothing
	// here can fail a kick. See CheckoutRootFor.
	CheckoutsDir string `yaml:"checkouts_dir,omitempty"`
}

const (
	// ForgeGitHub is the default forge kind (GitHub / GHE).
	ForgeGitHub = "github"
	// ForgeGitLab selects the GitLab forge (gitlab.com or self-managed).
	ForgeGitLab = "gitlab"
	// ForgeGitea selects the Gitea/Forgejo forge (self-managed or Codeberg).
	ForgeGitea = "gitea"
)

// ForgeKind returns the configured forge kind, defaulting to ForgeGitHub when
// unset so existing GitHub-only configs keep working unchanged.
func (p *ProjectConfig) ForgeKind() string {
	if p.Forge == "" {
		return ForgeGitHub
	}
	return p.Forge
}

// CheckoutRootFor returns the host-local checkout root for one monitored repo,
// or "" when none is configured. repo may be a bare name ("hive") or an
// org-qualified slug ("hivecommons/hive"); only the name portion is used, since
// CheckoutsDir is keyed by bare repo name.
//
// Returning "" is the no-op case and is deliberately the default: a hive that
// never sets checkouts_dir behaves exactly as it did before this existed.
func (p *ProjectConfig) CheckoutRootFor(repo string) string {
	dir := strings.TrimSpace(p.CheckoutsDir)
	name := strings.TrimSpace(repo)
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if dir == "" || name == "" {
		return ""
	}
	// Refuse a name that would escape CheckoutsDir. A repo name comes from
	// config rather than from a forge, but this is a filesystem path built from
	// a string and the guard costs nothing.
	if name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return ""
	}
	return filepath.Join(dir, name)
}

// PRsAllowed returns whether agents may open pull requests. Defaults to true.
func (p *ProjectConfig) PRsAllowed() bool {
	if p.OpenPRs != nil {
		return *p.OpenPRs
	}
	return true
}

// WorkSourceConfig selects where hive reads work items (Step 01 of the loop).
// Absent or type="" defaults to GitHub Issues — backward-compatible for all
// existing hives.
type WorkSourceConfig struct {
	// Type selects the work source: "" | "github" | "github_projects" | "linear" | "jira"
	Type string `yaml:"type" json:"type"`
	// GitHubProjects configures the GitHub Projects v2 adapter.
	GitHubProjects GitHubProjectsSourceConfig `yaml:"github_projects,omitempty" json:"github_projects,omitempty"`
	// Linear configures the Linear GraphQL adapter.
	Linear LinearSourceConfig `yaml:"linear,omitempty" json:"linear,omitempty"`
	// Jira configures the Jira Cloud REST v3 adapter.
	Jira JiraSourceConfig `yaml:"jira,omitempty" json:"jira,omitempty"`
}

// IsZero reports whether no work source has been configured at all: no type
// and no adapter-specific settings. Used by the dashboard-overlay reload to
// decide whether the overlay carries an operator-set work source.
func (w WorkSourceConfig) IsZero() bool {
	return w.Type == "" &&
		reflect.DeepEqual(w.GitHubProjects, GitHubProjectsSourceConfig{}) &&
		reflect.DeepEqual(w.Linear, LinearSourceConfig{}) &&
		reflect.DeepEqual(w.Jira, JiraSourceConfig{})
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

// JiraSourceConfig configures the Jira Cloud REST v3 work source.
type JiraSourceConfig struct {
	BaseURL     string   `yaml:"base_url" json:"base_url"`
	Email       string   `yaml:"email" json:"email"`
	APIToken    string   `yaml:"api_token,omitempty" json:"api_token,omitempty"`
	ProjectKeys []string `yaml:"project_keys,omitempty" json:"project_keys,omitempty"`
	JQL         string   `yaml:"jql,omitempty" json:"jql,omitempty"`
	Repo        string   `yaml:"repo,omitempty" json:"repo,omitempty"`
	HoldLabels  []string `yaml:"hold_labels,omitempty" json:"hold_labels,omitempty"`
}

// ProjectObservabilityBackendRef names references an agent may place in managed
// project configuration. Values are identifiers only: EndpointEnv is an
// environment-variable NAME and CredentialSecret is a Kubernetes-style
// "secret-name/key" reference, never a literal endpoint or credential.
type ProjectObservabilityBackendRef struct {
	EndpointEnv      string `yaml:"endpoint_env,omitempty" json:"endpoint_env,omitempty"`
	CredentialSecret string `yaml:"credential_secret,omitempty" json:"credential_secret,omitempty"`
}

// ProjectObservabilityConfig is the operator-confirmed target stack for the
// managed project's telemetry and operations agents. Empty means detect and
// report only; it never authorizes an exporter to send data off-box.
type ProjectObservabilityConfig struct {
	OpenSource []string                                  `yaml:"open_source,omitempty" json:"open_source,omitempty"`
	KubeNative []string                                  `yaml:"kube_native,omitempty" json:"kube_native,omitempty"`
	Commercial []string                                  `yaml:"commercial,omitempty" json:"commercial,omitempty"`
	References map[string]ProjectObservabilityBackendRef `yaml:"references,omitempty" json:"references,omitempty"`
}

// PromptSection renders the confirmed managed-project targets without exposing
// any secret values. The result is injected only into the telemetry and
// operations policy templates through ${PROJECT_OBSERVABILITY}.
func (p ProjectObservabilityConfig) PromptSection() string {
	var b strings.Builder
	b.WriteString("MANAGED-PROJECT OBSERVABILITY TARGETS (operator-confirmed):\n")
	writeFamily := func(label string, values []string) {
		if len(values) == 0 {
			b.WriteString("  " + label + ": (none configured)\n")
			return
		}
		b.WriteString("  " + label + ": " + strings.Join(values, ", ") + "\n")
	}
	writeFamily("open source", p.OpenSource)
	writeFamily("kube-native", p.KubeNative)
	writeFamily("commercial", p.Commercial)
	if len(p.OpenSource)+len(p.KubeNative)+len(p.Commercial) == 0 {
		b.WriteString("  No backend is confirmed. Detect the existing stack and report recommendations only; do not add an exporter or external data flow.\n")
	}
	if len(p.References) > 0 {
		b.WriteString("  safe references (names only):\n")
		keys := make([]string, 0, len(p.References))
		for name := range p.References {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		for _, name := range keys {
			ref := p.References[name]
			parts := make([]string, 0, 2)
			if ref.EndpointEnv != "" {
				parts = append(parts, "endpoint env="+ref.EndpointEnv)
			}
			if ref.CredentialSecret != "" {
				parts = append(parts, "credential secret="+ref.CredentialSecret)
			}
			if len(parts) > 0 {
				b.WriteString("    " + name + ": " + strings.Join(parts, ", ") + "\n")
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// RepoCount returns the number of repos this hive watches, for threshold
// scaling. A hive with an empty repos: list still watches at least its primary
// repo, so the floor is 1.
func (p ProjectConfig) RepoCount() int {
	if n := len(p.Repos); n > 0 {
		return n
	}
	return 1
}
