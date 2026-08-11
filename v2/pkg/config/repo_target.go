package config

import (
	"net/url"
	"strings"
)

const repoTargetFixCTA = "Fix in Governor Config → Repos."

// RepoTargetIssue is the operator-facing validation result for project.org,
// project.repos, and project.primary_repo. It is safe to expose in API errors,
// dashboard status payloads, logs, and hub heartbeat state.
type RepoTargetIssue struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidateRepoTargets checks that every configured repository target resolves
// to exactly org/repo on the configured forge. It deliberately does not rewrite
// ambiguous shapes; callers should reject writes or surface the returned
// message so an owner fixes the config.
func ValidateRepoTargets(cfg *Config) *RepoTargetIssue {
	if cfg == nil {
		return nil
	}
	return ValidateProjectRepoTargets(cfg.Project.Org, cfg.Project.Repos, cfg.Project.PrimaryRepo, cfg.GitHub.HostLabel())
}

// ValidateProjectRepoTargets is the testable core used by provisioning and
// config-save paths before they have a full Config object.
func ValidateProjectRepoTargets(org string, repos []string, primaryRepo, forgeHost string) *RepoTargetIssue {
	org = strings.TrimSpace(org)
	if org == "" {
		return issue("project.org", "org is empty — expected org/repo")
	}
	if looksLikeURL(org) || strings.Contains(org, "/") {
		return issue("project.org", "org '"+org+"' is not an organization name — expected org/repo")
	}
	if looksLikeForgeHost(org, forgeHost) {
		return issue("project.org", "org '"+org+"' looks like a forge host — expected org/repo")
	}
	if len(repos) == 0 {
		return issue("project.repos", "repo is empty — expected org/repo")
	}
	for _, repo := range repos {
		if repoIssue := validateRepoName("project.repos", org, repo); repoIssue != nil {
			return repoIssue
		}
	}
	if strings.TrimSpace(primaryRepo) != "" {
		if repoIssue := validateRepoName("project.primary_repo", org, primaryRepo); repoIssue != nil {
			return repoIssue
		}
	}
	return nil
}

func validateRepoName(field, org, repo string) *RepoTargetIssue {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return issue(field, "repo is empty — expected org/repo")
	}
	if looksLikeURL(repo) {
		return issue(field, "repo '"+repo+"' is a URL — expected repo name only so the target resolves to org/repo")
	}
	if strings.Contains(repo, "/") {
		// A fully-qualified "<org>/<repo>" is accepted when the org prefix
		// matches the configured project.org — it is unambiguously the same
		// target, and the save path strips the prefix down to the bare name.
		// This is what a user naturally types/pastes when re-adding an existing
		// repo from the Governor → Repos page (issue #3021). Anything else
		// (a different org, a trailing slash, or multiple slashes) is still a
		// misconfiguration.
		org = strings.TrimSpace(org)
		prefix := org + "/"
		bare := strings.TrimPrefix(repo, prefix)
		if org != "" && strings.HasPrefix(repo, prefix) && bare != "" && !strings.Contains(bare, "/") {
			return nil
		}
		return issue(field, "repo '"+repo+"' contains '/' — expected repo name only (or '"+org+"/<repo>') so the target resolves to org/repo")
	}
	return nil
}

func issue(field, msg string) *RepoTargetIssue {
	return &RepoTargetIssue{Field: field, Message: "Repo target misconfigured: " + msg + ". " + repoTargetFixCTA}
}

func looksLikeURL(s string) bool {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(strings.ToLower(s), "http://") || strings.HasPrefix(strings.ToLower(s), "https://") {
		return true
	}
	u, err := url.Parse(s)
	return err == nil && u.Scheme != "" && u.Host != ""
}

func looksLikeForgeHost(value, configuredHost string) bool {
	value = strings.ToLower(strings.Trim(strings.TrimSpace(value), "/"))
	configuredHost = strings.ToLower(strings.Trim(strings.TrimSpace(configuredHost), "/"))
	if value == "" || !strings.Contains(value, ".") {
		return false
	}
	if configuredHost != "" && value == configuredHost {
		return true
	}
	return value == "github.com" || strings.HasPrefix(value, "github.")
}
