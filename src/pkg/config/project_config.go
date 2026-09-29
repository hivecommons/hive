package config

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
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

	// PausedRepos is the per-repo agent pause: the repos in Repos that are
	// currently quiet. It is a RUN-STATE, and deliberately a SEPARATE list
	// rather than a field on a per-repo object, because #6111 (per-repo ACMM
	// levels) proposes turning Repos from []string into objects — this feature
	// must neither depend on that decision nor pre-empt it. A repo listed here
	// stays in Repos: it keeps its dashboard card and its ACMM eval, and only
	// agent activity stops. See RepoPause in repo_pause.go.
	//
	// Optional and additive. Absent — the state of every existing config —
	// means nothing is paused and the hive behaves exactly as before.
	PausedRepos []RepoPause `yaml:"paused_repos,omitempty"`
	// RepoPolicies stores optional per-repository policy overrides keyed by
	// repo name. project.repos remains the watched-repo identity list; this
	// sidecar list lets existing string-list configs keep round-tripping.
	RepoPolicies []RepoPolicy `yaml:"repo_policies,omitempty" json:"repo_policies,omitempty"`
	// WritingGuide is the hive owner's instruction for how the issues, PRs and
	// reviews its agents write should READ — length, structure, register — as
	// free text (hivecommons/hive#7667). It is rendered into every default
	// policy template that files an issue or PR, as ${WRITING_GUIDE},
	// immediately before the body template the agent is told to fill in. That
	// position is the point: a style rule in AGENTS.md arrives as background
	// knowledge and loses to the template that sits in the prompt, so the rule
	// has to sit next to the template. Review prompts built in Go receive the
	// same rendered section.
	//
	// Empty (the default) renders nothing, so a hive that never sets it gets
	// byte-identical prompts. See WritingGuideSection.
	WritingGuide string `yaml:"writing_guide,omitempty"`
}

const MaxWritingGuideBytes = 64 * 1024

func ValidateWritingGuide(v string) error {
	if len([]byte(v)) > MaxWritingGuideBytes {
		return fmt.Errorf("project.writing_guide is %d bytes, over the %d byte limit", len([]byte(v)), MaxWritingGuideBytes)
	}
	return nil
}

// WritingGuideSection renders project.writing_guide as the prompt paragraph
// ${WRITING_GUIDE} expands to, or "" when no guide is set (hivecommons/hive#7667).
//
// The header names the guide's authority (the hive owner), its scope (every
// issue body, PR body and review comment the agent writes in this session —
// the variable appears once per template, ahead of the first body template, and
// the later ones in the same policy are covered by this sentence) and its
// limit: it governs how the body reads, never what the policy requires it to
// contain. The quality policy demands evidence and a guide may ask for evidence
// under a fold; the limit is what keeps those from reading as a contradiction.
func (p *ProjectConfig) WritingGuideSection() string {
	guide := strings.TrimSpace(p.WritingGuide)
	if guide == "" {
		return ""
	}
	return "WRITING GUIDE (set by this hive's owner in project.writing_guide). Every issue body, PR body and review comment you write in this session MUST follow it. " +
		"It governs how the body reads — length, structure, wording — not what it contains: keep every section, field and piece of evidence the template below asks for, and apply the guide to how you write them.\n\n" +
		guide + "\n"
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

type PoliciesConfig struct {
	Repo         string        `yaml:"repo"`
	Branch       string        `yaml:"branch"`
	Path         string        `yaml:"path"`
	PollInterval time.Duration `yaml:"poll_interval"`
	LocalDir     string        `yaml:"local_dir"`
}

// StatsDisplayEntry defines a single metric to show in the agent's sidebar/detail view.
type StatsDisplayEntry struct {
	Key        string `yaml:"key" json:"key"`
	Label      string `yaml:"label" json:"label"`
	Source     string `yaml:"source" json:"source"`
	Field      string `yaml:"field" json:"field"`
	Style      string `yaml:"style" json:"style"`
	TrendField string `yaml:"trend_field,omitempty" json:"trendField,omitempty"`
	Target     int    `yaml:"target,omitempty" json:"target,omitempty"`
	// Desc is a one-line explanation of what the stat verifies, rendered
	// as a hover tooltip in the dashboard (health checks especially).
	Desc string `yaml:"desc,omitempty" json:"desc,omitempty"`
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
