package config

import (
	"fmt"
	"net/url"
	"strings"
)

// KnownForgeKinds is every value project.forge and
// project.repo_policies[].forge may name. The values match pkg/forge.Kind by
// value (pkg/forge cannot be imported from here); a test in pkg/forge pins
// that they stay equal.
var KnownForgeKinds = []string{ForgeGitHub, ForgeGitLab, ForgeGitea, ForgeForgejo}

func isKnownForgeKind(kind string) bool {
	for _, k := range KnownForgeKinds {
		if kind == k {
			return true
		}
	}
	return false
}

// ForgeKindForRepo returns the forge kind repo lives on: its
// repo_policies[].forge override when set, else project.forge (defaulting to
// GitHub). A nil config is GitHub, the pre-forge behavior.
func (c *Config) ForgeKindForRepo(repo string) string {
	if c == nil {
		return ForgeGitHub
	}
	if rp, ok := c.RepoPolicyFor(repo); ok {
		if k := strings.ToLower(strings.TrimSpace(rp.Forge)); k != "" {
			return k
		}
	}
	return c.Project.ForgeKind()
}

// RepoOnGitHub reports whether repo's GitHub-only paths (the automerge and
// close-on-merge sweeps, which call the GitHub API directly) may act on it. It
// is false only for a repo that resolves to GitLab or Gitea; an unknown kind
// is treated as GitHub, the same fallback the forge writer takes, so a typo'd
// forge key never silently stops a GitHub hive's sweeps. A Forgejo repo gets
// its own sweeps instead (pkg/forgesweep); GitLab and Gitea repos get none.
// See ForgeSweepsFor.
func (c *Config) RepoOnGitHub(repo string) bool {
	switch c.ForgeKindForRepo(repo) {
	case ForgeGitLab, ForgeGitea, ForgeForgejo:
		return false
	}
	return true
}

func onNonGitHubForge(kind string) bool {
	switch kind {
	case ForgeGitLab, ForgeGitea, ForgeForgejo:
		return true
	}
	return false
}

// AllReposOffGitHub reports whether every repo resolves to a non-GitHub forge:
// project.forge names one and no repo_policies entry moves a repo to GitHub.
// Such a hive needs the forge's own URL and token, not a GitHub credential.
// An unknown or unset project.forge is GitHub, so this is false for it.
func (c *Config) AllReposOffGitHub() bool {
	if c == nil || !onNonGitHubForge(c.Project.ForgeKind()) {
		return false
	}
	repoPauseMu.RLock()
	defer repoPauseMu.RUnlock()
	for _, rp := range c.Project.RepoPolicies {
		if k := strings.ToLower(strings.TrimSpace(rp.Forge)); k != "" && !onNonGitHubForge(k) {
			return false
		}
	}
	return true
}

func forgeURLKey(kind string) string {
	switch kind {
	case ForgeGitLab:
		return "gitlab.gitlab_url"
	case ForgeGitea:
		return "gitea.gitea_url"
	}
	return "forgejo.forgejo_url"
}

func (c *Config) validateForgeCredentials() error {
	repos := append([]string{""}, c.Project.Repos...)
	for _, rp := range c.Project.RepoPolicies {
		repos = append(repos, rp.Repo)
	}
	for _, repo := range repos {
		if c.ForgeURLForRepo(repo) != "" {
			continue
		}
		kind := c.ForgeKindForRepo(repo)
		if repo == "" {
			return fmt.Errorf("project.forge is %s, so %s is required (github.token, github.app_id or github.forge is not needed when every repo is on another forge)", kind, forgeURLKey(kind))
		}
		return fmt.Errorf("repo %q is on %s, so %s or a repo_policies forge_url is required", repo, kind, forgeURLKey(kind))
	}
	return nil
}

// ForgeURLForRepo returns the instance root of repo's forge: its
// repo_policies[].forge_url override when set, else the forge-wide setting
// for the repo's kind (gitlab.gitlab_url, gitea.gitea_url, or GitHub's
// resolved base URL). It may be empty for a Gitea repo with no URL anywhere;
// Validate rejects that configuration.
func (c *Config) ForgeURLForRepo(repo string) string {
	if c == nil {
		return ""
	}
	if rp, ok := c.RepoPolicyFor(repo); ok {
		if u := strings.TrimRight(strings.TrimSpace(rp.ForgeURL), "/"); u != "" {
			return u
		}
	}
	switch c.ForgeKindForRepo(repo) {
	case ForgeGitLab:
		return c.GitLab.InstanceURL()
	case ForgeGitea:
		return c.Gitea.InstanceURL()
	case ForgeForgejo:
		return c.Forgejo.InstanceURL()
	default:
		return c.GitHub.ResolvedBaseURL()
	}
}

// CheckForgeTokenHost returns an error when repo's forge token would go to a
// different host than the one its forge block configures. The token comes from
// the block of repo's kind (gitlab, gitea or forgejo), so a
// repo_policies[].forge_url on another host would send it there. A gitlab block
// with no URL defaults to gitlab.com, so that default is the host to protect.
// A gitea or forgejo block with no URL of its own has no default and names no
// host, so nothing is compared. A block URL that is unparsable or has no host
// is an error, because the comparison cannot run.
func (c *Config) CheckForgeTokenHost(repo string) error {
	if c == nil {
		return nil
	}
	rp, ok := c.RepoPolicyFor(repo)
	if !ok || strings.TrimSpace(rp.ForgeURL) == "" {
		return nil
	}
	kind := c.ForgeKindForRepo(repo)
	var blockKey, blockURL string
	switch kind {
	case ForgeGitLab:
		blockKey, blockURL = "gitlab.gitlab_url", c.GitLab.InstanceURL()
	case ForgeGitea:
		blockKey, blockURL = "gitea.gitea_url", c.Gitea.URL
	case ForgeForgejo:
		blockKey, blockURL = "forgejo.forgejo_url", c.Forgejo.URL
	default:
		return nil
	}
	if strings.TrimSpace(blockURL) == "" {
		return nil
	}
	want, err := hostPort(blockURL)
	if err != nil {
		return fmt.Errorf("repo %q: %s %w", repo, blockKey, err)
	}
	got, err := hostPort(rp.ForgeURL)
	if err != nil {
		return fmt.Errorf("repo %q: forge_url %w", repo, err)
	}
	if got != want {
		return fmt.Errorf("repo %q: forge_url host %q differs from %s host %q; the %s token is only sent to its own block's host", repo, got, blockKey, want, kind)
	}
	return nil
}

// hostPort returns the lowercased host of an http(s) URL, with the port
// appended unless it is the scheme's default (443 for https, 80 for http). It
// errors when raw does not parse or has no host, such as a URL with no scheme.
func hostPort(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%q is not a URL with a host", raw)
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port == "" || (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		return host, nil
	}
	return host + ":" + port, nil
}

// HasRepoForgeOverrides reports whether any repo_policies entry sets forge or
// forge_url. When false every repo resolves to project.forge, so callers can
// keep their single project-wide forge path unchanged.
func (c *Config) HasRepoForgeOverrides() bool {
	if c == nil {
		return false
	}
	repoPauseMu.RLock()
	defer repoPauseMu.RUnlock()
	for _, rp := range c.Project.RepoPolicies {
		if strings.TrimSpace(rp.Forge) != "" || strings.TrimSpace(rp.ForgeURL) != "" {
			return true
		}
	}
	return false
}

// validateRepoForges checks the per-repo forge overrides. project.forge itself
// is not checked here: an unknown project-wide value already falls back to
// GitHub with a warning at runtime, and failing load on it now would change
// behavior for existing configs. The per-repo keys are new, so they are strict.
func (c *Config) validateRepoForges() error {
	for i, rp := range c.Project.RepoPolicies {
		field := fmt.Sprintf("project.repo_policies[%d]", i)
		kind := strings.ToLower(strings.TrimSpace(rp.Forge))
		if kind != "" && !isKnownForgeKind(kind) {
			return fmt.Errorf("%s.forge: unknown forge %q (want %s)", field, rp.Forge, strings.Join(KnownForgeKinds, ", "))
		}
		if raw := strings.TrimSpace(rp.ForgeURL); raw != "" {
			u, err := url.Parse(raw)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return fmt.Errorf("%s.forge_url: %q is not an http(s) URL with a host", field, rp.ForgeURL)
			}
			if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return fmt.Errorf("%s.forge_url: %q must be a bare instance root (no credentials, query or fragment)", field, rp.ForgeURL)
			}
		}
		if err := c.CheckForgeTokenHost(rp.Repo); err != nil {
			return fmt.Errorf("%s.forge_url: %w", field, err)
		}
		// Only an entry that names a forge itself is held to having a URL: a
		// policy kept for another reason (acmm_level, label_driven) on a
		// project.forge: gitea hive must load exactly as it did before.
		if kind == ForgeGitea && c.ForgeURLForRepo(rp.Repo) == "" {
			return fmt.Errorf("%s: repo %q is on gitea but neither forge_url nor gitea.gitea_url is set", field, rp.Repo)
		}
		if kind == ForgeForgejo && c.ForgeURLForRepo(rp.Repo) == "" {
			return fmt.Errorf("%s: repo %q is on forgejo but neither forge_url nor forgejo.forgejo_url is set", field, rp.Repo)
		}
	}
	return nil
}

// HeartbeatForge is the forge a hive reports to its hub for display: the kind
// its primary repo lives on and, for a forge other than GitHub, that forge's
// instance root and the hive's own login there. A GitHub hive reports only its
// kind, because its host already rides the heartbeat as github_host
// (hivecommons/hive#20181).
type HeartbeatForge struct {
	Kind    string
	BaseURL string
	Login   string
}

// HeartbeatForge returns the forge of the primary repo (project.forge when no
// primary repo is set), read from config only so it is safe on every beat.
// The login is forgejo.login for a Forgejo repo, else project.ai_author; it
// is empty when neither is set, since resolving the token's user would need a
// network call.
func (c *Config) HeartbeatForge() HeartbeatForge {
	if c == nil {
		return HeartbeatForge{Kind: ForgeGitHub}
	}
	repo := c.Project.PrimaryRepo
	kind := c.ForgeKindForRepo(repo)
	if !onNonGitHubForge(kind) {
		return HeartbeatForge{Kind: ForgeGitHub}
	}
	login := ""
	if kind == ForgeForgejo {
		login = strings.TrimSpace(c.Forgejo.Login)
	}
	if login == "" {
		login = strings.TrimSpace(c.Project.AIAuthor)
	}
	return HeartbeatForge{Kind: kind, BaseURL: c.ForgeURLForRepo(repo), Login: login}
}
