package config

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Owner-approved prompt and definition repos (hivecommons/hive#18490).
//
// The seed-only gate (variables.security.allow_github_prompt plus
// github_prompt_allowlist) stays exactly as it was. This file adds a second,
// narrower way in: the hive OWNER may approve one of the hive's own managed
// repos (project.org + project.repos) as a source for agents' prompt_source and
// definition_source. Each approval covers that one repo only; it does not turn
// on the seed allowlist, and a repo outside the managed set can never be
// approved this way.
//
// Approvals live in variables.security.owner_github_prompt_repos. Like the rest
// of variables.security they are honored only from the loaded config file (the
// seed, or on a hosted spoke the PVC runtime config the dashboard Save writes);
// the user-writable dashboard overlay's variables block is still never merged,
// so an overlay cannot approve a repo. The only writer is the owner-only
// dashboard endpoint, which persists through Save.

// OwnerPromptRepo is one owner approval: the repo and who approved it, when.
type OwnerPromptRepo struct {
	// Repo is the approved "owner/repo" slug. It must be one of the hive's
	// managed repos, both when it is approved and every time it is checked.
	Repo string `yaml:"repo" json:"repo"`
	// ApprovedBy is the dashboard login that approved the repo (audit only).
	ApprovedBy string `yaml:"approved_by,omitempty" json:"approved_by,omitempty"`
	// ApprovedAt is the RFC 3339 time of the approval (audit only).
	ApprovedAt string `yaml:"approved_at,omitempty" json:"approved_at,omitempty"`
}

// ErrNotManagedRepo is returned when an owner tries to approve a repo that is
// not one of the hive's managed repos. Such a repo still needs the seed edit.
var ErrNotManagedRepo = errors.New("repo is not one of this hive's managed repos")

// ownerPromptReposMu guards Variables.Security.OwnerGitHubPromptRepos: the
// prompt and definition resolvers read it from scheduler goroutines while the
// dashboard writes it. Lock order is saveMu then ownerPromptReposMu.
var ownerPromptReposMu sync.RWMutex

// IsManagedRepo reports whether slug ("owner/repo") is one of the hive's own
// managed repos: an entry of project.repos, qualified with project.org when it
// carries no owner. The match ignores case, as GitHub does.
func (c *Config) IsManagedRepo(slug string) bool {
	if c == nil {
		return false
	}
	key := strings.ToLower(strings.TrimSpace(slug))
	if key == "" || !strings.Contains(key, "/") {
		return false
	}
	for _, repo := range c.Project.Repos {
		if agentRepoKey(c.Project.Org, repo) == key {
			return true
		}
	}
	return false
}

// ManagedRepoSlugs returns the hive's managed repos as "owner/repo" slugs, in
// project.repos order.
func (c *Config) ManagedRepoSlugs() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.Project.Repos))
	for _, repo := range c.Project.Repos {
		if slug := qualifyAgentRepo(c.Project.Org, repo); strings.Contains(slug, "/") {
			out = append(out, slug)
		}
	}
	return out
}

// NormalizeOwnerPromptRepo validates slug for an owner approval and returns
// the managed repo's canonical slug. It refuses a malformed slug and any repo
// outside the managed set (wrapping ErrNotManagedRepo).
func (c *Config) NormalizeOwnerPromptRepo(slug string) (string, error) {
	slug = strings.TrimSpace(slug)
	parts := strings.Split(slug, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("repo %q must be an \"owner/repo\" slug", slug)
	}
	if c == nil {
		return "", fmt.Errorf("%w: %s", ErrNotManagedRepo, slug)
	}
	key := strings.ToLower(slug)
	for _, managed := range c.ManagedRepoSlugs() {
		if strings.ToLower(managed) == key {
			return managed, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrNotManagedRepo, slug)
}

// OwnerPromptRepos returns a copy of the owner-approved repos.
func (c *Config) OwnerPromptRepos() []OwnerPromptRepo {
	if c == nil {
		return nil
	}
	ownerPromptReposMu.RLock()
	defer ownerPromptReposMu.RUnlock()
	src := c.Variables.Security.OwnerGitHubPromptRepos
	if len(src) == 0 {
		return nil
	}
	return append([]OwnerPromptRepo(nil), src...)
}

// OwnerPromptRepoAllowed reports whether slug is owner-approved AND still one
// of the hive's managed repos. Removing a repo from project.repos therefore
// revokes its approval without anyone having to remember the allowlist.
func (c *Config) OwnerPromptRepoAllowed(slug string) bool {
	if c == nil || strings.TrimSpace(slug) == "" || !c.IsManagedRepo(slug) {
		return false
	}
	ownerPromptReposMu.RLock()
	defer ownerPromptReposMu.RUnlock()
	return ownerPromptRepoIndex(c.Variables.Security.OwnerGitHubPromptRepos, slug) >= 0
}

// ownerPromptRepoIndex returns the index of slug in list (case-insensitive),
// or -1. Callers hold ownerPromptReposMu.
func ownerPromptRepoIndex(list []OwnerPromptRepo, slug string) int {
	key := strings.TrimSpace(slug)
	for i, e := range list {
		if strings.EqualFold(strings.TrimSpace(e.Repo), key) {
			return i
		}
	}
	return -1
}

// AllowOwnerPromptRepoAndSave approves one managed repo as a prompt and
// definition source and persists the config, under the same saveMu as every
// other saver. A repo outside the managed set is refused before anything
// changes. Returns the stored entry and whether anything changed; approving an
// already-approved repo is a no-op that keeps the original approval record.
func (c *Config) AllowOwnerPromptRepoAndSave(slug, approvedBy string, at time.Time) (OwnerPromptRepo, bool, error) {
	repo, err := c.NormalizeOwnerPromptRepo(slug)
	if err != nil {
		return OwnerPromptRepo{}, false, err
	}
	saveMu.Lock()
	defer saveMu.Unlock()

	ownerPromptReposMu.Lock()
	list := c.Variables.Security.OwnerGitHubPromptRepos
	if i := ownerPromptRepoIndex(list, repo); i >= 0 {
		existing := list[i]
		ownerPromptReposMu.Unlock()
		return existing, false, nil
	}
	entry := OwnerPromptRepo{
		Repo:       repo,
		ApprovedBy: strings.TrimSpace(approvedBy),
		ApprovedAt: at.UTC().Format(time.RFC3339),
	}
	next := make([]OwnerPromptRepo, 0, len(list)+1)
	next = append(next, list...)
	c.Variables.Security.OwnerGitHubPromptRepos = append(next, entry)
	ownerPromptReposMu.Unlock()

	return entry, true, c.saveLocked()
}

// RevokeOwnerPromptRepoAndSave removes an owner approval and persists the
// config. Revoking is allowed for any slug on the list, managed or not, so an
// approval left behind by a repo dropped from project.repos can be cleaned up.
// Returns whether anything changed.
func (c *Config) RevokeOwnerPromptRepoAndSave(slug string) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("no config loaded")
	}
	saveMu.Lock()
	defer saveMu.Unlock()

	ownerPromptReposMu.Lock()
	list := c.Variables.Security.OwnerGitHubPromptRepos
	i := ownerPromptRepoIndex(list, slug)
	if i < 0 {
		ownerPromptReposMu.Unlock()
		return false, nil
	}
	next := make([]OwnerPromptRepo, 0, len(list)-1)
	next = append(next, list[:i]...)
	next = append(next, list[i+1:]...)
	if len(next) == 0 {
		next = nil
	}
	c.Variables.Security.OwnerGitHubPromptRepos = next
	ownerPromptReposMu.Unlock()

	return true, c.saveLocked()
}

// SeedGitHubPromptAllowed reports whether the SEED gate alone permits slug:
// allow_github_prompt on and slug on github_prompt_allowlist.
func (c *Config) SeedGitHubPromptAllowed(slug string) bool {
	if c == nil || slug == "" || !c.Variables.Security.AllowGitHubPrompt {
		return false
	}
	for _, allowed := range c.Variables.Security.GitHubPromptAllowlist {
		if strings.EqualFold(strings.TrimSpace(allowed), slug) {
			return true
		}
	}
	return false
}
