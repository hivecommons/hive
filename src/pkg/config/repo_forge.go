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
var KnownForgeKinds = []string{ForgeGitHub, ForgeGitLab, ForgeGitea}

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
// forge key never silently stops a GitHub hive's sweeps.
func (c *Config) RepoOnGitHub(repo string) bool {
	switch c.ForgeKindForRepo(repo) {
	case ForgeGitLab, ForgeGitea:
		return false
	}
	return true
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
	default:
		return c.GitHub.ResolvedBaseURL()
	}
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
		// Only an entry that names a forge itself is held to having a URL: a
		// policy kept for another reason (acmm_level, label_driven) on a
		// project.forge: gitea hive must load exactly as it did before.
		if kind == ForgeGitea && c.ForgeURLForRepo(rp.Repo) == "" {
			return fmt.Errorf("%s: repo %q is on gitea but neither forge_url nor gitea.gitea_url is set", field, rp.Repo)
		}
	}
	return nil
}
