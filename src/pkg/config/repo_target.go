package config

import (
	"net/url"
	"strings"
)

const repoTargetFixCTA = "Fix in Settings → Repos."

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

// NormalizeRepoForOrg accepts a repos-list entry and returns the bare repo name
// the org/repo target is built from.
//
// A repo entry is combined with project.org to form "org/repo", so an entry that
// already carries its own org ("zacburns/mlz-manager" under org "zacburns")
// resolves to "zacburns/zacburns/mlz-manager" and every agent targeting it
// fails. Owners paste the org-qualified form routinely — it is the shape GitHub
// shows everywhere — so accept it when the prefix matches the configured org and
// strip it back to the bare name.
//
// Only an exact, case-insensitive match on the configured org is stripped.
// An entry qualified with a DIFFERENT org ("other/hive" under org "kubestellar")
// is left untouched so validateRepoName still rejects it: silently dropping a
// mismatched org would retarget the hive at a repository the owner did not name.
// The second return reports whether a prefix was stripped.
func NormalizeRepoForOrg(org, repo string) (string, bool) {
	org = strings.TrimSpace(org)
	repo = strings.TrimSpace(repo)
	if org == "" || repo == "" || strings.Contains(org, "/") {
		return repo, false
	}
	// A URL is a different misconfiguration with its own error message; leave it
	// for validateRepoName rather than half-parsing it here.
	if looksLikeURL(repo) {
		return repo, false
	}
	prefix := org + "/"
	if len(repo) <= len(prefix) || !strings.EqualFold(repo[:len(prefix)], prefix) {
		return repo, false
	}
	rest := repo[len(prefix):]
	// "org/a/b" is not an org-qualified repo name — it is some deeper path we
	// must not guess at. Leave it to be rejected.
	if rest == "" || strings.Contains(rest, "/") {
		return repo, false
	}
	return rest, true
}

// NormalizeProjectRepos returns repos with any entry qualified by the configured
// org rewritten to its bare repo name. Entries that need no change are returned
// as-is. The second return reports whether anything changed, so callers can log
// or persist the repair.
func NormalizeProjectRepos(org string, repos []string) ([]string, bool) {
	if len(repos) == 0 {
		return repos, false
	}
	out := make([]string, len(repos))
	changed := false
	for i, repo := range repos {
		normalized, stripped := NormalizeRepoForOrg(org, repo)
		out[i] = normalized
		if stripped {
			changed = true
		}
	}
	if !changed {
		return repos, false
	}
	return out, true
}

// ValidateProjectRepoTargets is the testable core used by provisioning and
// config-save paths before they have a full Config object.
//
// Org-qualified entries that match org are normalized before validation, so
// "zacburns/mlz-manager" under org "zacburns" is accepted; anything else that
// still contains '/' keeps failing with the same operator-facing message.
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
		if repoIssue := validateRepoTargetForOrg("project.repos", org, repo, forgeHost); repoIssue != nil {
			return repoIssue
		}
	}
	if strings.TrimSpace(primaryRepo) != "" {
		if repoIssue := validateRepoTargetForOrg("project.primary_repo", org, primaryRepo, forgeHost); repoIssue != nil {
			return repoIssue
		}
	}
	return nil
}

func validateRepoTargetForOrg(field, org, repo, forgeHost string) *RepoTargetIssue {
	repo = strings.TrimSpace(repo)
	if parsed, ok := parseExplicitRepoTarget(repo); ok {
		if parsed.host != "" && !sameRepoTargetForgeHost(parsed.host, forgeHost) {
			return validateRepoName(field, repo)
		}
		if parsed.org != "" && !strings.EqualFold(parsed.org, org) {
			return issue(field, "repo '"+repo+"' belongs to org '"+parsed.org+"', but this hive is configured for org '"+org+"' — to migrate, save a repo from the new org in Settings → Repos so the dashboard can adopt it")
		}
	}
	normalized, _ := NormalizeRepoForOrg(org, repo)
	return validateRepoName(field, normalized)
}

type explicitRepoTarget struct {
	org  string
	host string
}

func parseExplicitRepoTarget(repo string) (explicitRepoTarget, bool) {
	if repo == "" {
		return explicitRepoTarget{}, false
	}
	if u, err := url.Parse(repo); err == nil && u.Scheme != "" && u.Host != "" {
		parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 3)
		if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
			return explicitRepoTarget{org: parts[0], host: strings.ToLower(u.Host)}, true
		}
		return explicitRepoTarget{}, false
	}
	stripped := strings.Trim(repo, "/")
	parts := strings.Split(stripped, "/")
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" && !strings.Contains(parts[0], ".") {
		return explicitRepoTarget{org: parts[0]}, true
	}
	if len(parts) >= 3 && strings.Contains(parts[0], ".") && parts[1] != "" && parts[2] != "" {
		return explicitRepoTarget{org: parts[1], host: strings.ToLower(parts[0])}, true
	}
	return explicitRepoTarget{}, false
}

func sameRepoTargetForgeHost(a, b string) bool {
	norm := func(h string) string {
		h = strings.ToLower(strings.Trim(strings.TrimSpace(h), "/"))
		if h == "" || h == "github.com" {
			return "github.com"
		}
		return h
	}
	return norm(a) == norm(b)
}

func validateRepoName(field, repo string) *RepoTargetIssue {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return issue(field, "repo is empty — expected org/repo")
	}
	if looksLikeURL(repo) {
		return issue(field, "repo '"+repo+"' is a URL — expected repo name only so the target resolves to org/repo")
	}
	if strings.Contains(repo, "/") {
		return issue(field, "repo '"+repo+"' contains '/' — expected repo name only so the target resolves to org/repo")
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
