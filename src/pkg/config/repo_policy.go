package config

import (
	"fmt"
	"strings"
)

// RepoPolicy records optional per-repository policy overrides. It is separate
// from project.repos so existing configs keep their string-list repo shape.
type RepoPolicy struct {
	Repo                  string `yaml:"repo" json:"repo"`
	SelfAuthorizationHold *bool  `yaml:"self_authorization_hold,omitempty" json:"self_authorization_hold,omitempty"`
}

// RepoPolicyFor returns the policy override entry covering repo, if any.
func (c *Config) RepoPolicyFor(repo string) (RepoPolicy, bool) {
	if c == nil {
		return RepoPolicy{}, false
	}
	key := repoPauseKey(c.Project.Org, repo)
	if key == "" {
		return RepoPolicy{}, false
	}
	repoPauseMu.RLock()
	defer repoPauseMu.RUnlock()
	for _, rp := range c.Project.RepoPolicies {
		if repoPauseKey(c.Project.Org, rp.Repo) == key {
			return rp, true
		}
	}
	return RepoPolicy{}, false
}

// SelfAuthorizationHoldEnabledForRepo resolves the #5117 hold switch for repo:
// per-repo override first, then the hive-wide GitHub default, then default ON.
func (c *Config) SelfAuthorizationHoldEnabledForRepo(repo string) bool {
	if c == nil {
		return true
	}
	if c.GitHub.selfAuthorizationHoldEnvOverride != nil {
		return *c.GitHub.selfAuthorizationHoldEnvOverride
	}
	if rp, ok := c.RepoPolicyFor(repo); ok && rp.SelfAuthorizationHold != nil {
		return *rp.SelfAuthorizationHold
	}
	return c.GitHub.SelfAuthorizationHoldEnabled()
}

// SetSelfAuthorizationHoldForRepoAndSave records, clears, and persists one
// repository's #5117 self-authorization hold override. nil clears the override
// so the repo inherits the hive-wide GitHub default.
func (c *Config) SetSelfAuthorizationHoldForRepoAndSave(repo string, enabled *bool) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("no config loaded")
	}
	name := strings.TrimSpace(repo)
	if name == "" {
		return false, fmt.Errorf("repo is required")
	}
	name, _ = NormalizeRepoForOrg(c.Project.Org, name)

	saveMu.Lock()
	defer saveMu.Unlock()

	key := repoPauseKey(c.Project.Org, name)
	repoPauseMu.Lock()
	idx := -1
	for i, rp := range c.Project.RepoPolicies {
		if repoPauseKey(c.Project.Org, rp.Repo) == key {
			idx = i
			break
		}
	}
	changed := false
	if enabled == nil {
		if idx >= 0 && c.Project.RepoPolicies[idx].SelfAuthorizationHold != nil {
			c.Project.RepoPolicies = append(c.Project.RepoPolicies[:idx:idx], c.Project.RepoPolicies[idx+1:]...)
			changed = true
		}
	} else {
		v := *enabled
		if idx >= 0 {
			if c.Project.RepoPolicies[idx].SelfAuthorizationHold == nil || *c.Project.RepoPolicies[idx].SelfAuthorizationHold != v {
				c.Project.RepoPolicies[idx].SelfAuthorizationHold = &v
				changed = true
			}
		} else {
			c.Project.RepoPolicies = append(c.Project.RepoPolicies, RepoPolicy{Repo: name, SelfAuthorizationHold: &v})
			changed = true
		}
	}
	repoPauseMu.Unlock()

	if !changed {
		return false, nil
	}
	return true, c.saveLocked()
}

// SetSelfAuthorizationHoldForRepos applies a batch of per-repo #5117 override
// edits without saving. Values set explicit repo overrides; nil clears one so
// the repo inherits the hive-wide default. The caller owns persistence.
func (c *Config) SetSelfAuthorizationHoldForRepos(overrides map[string]*bool) bool {
	if c == nil || len(overrides) == 0 {
		return false
	}
	repoPauseMu.Lock()
	defer repoPauseMu.Unlock()
	changed := false
	for repo, enabled := range overrides {
		name := strings.TrimSpace(repo)
		name, _ = NormalizeRepoForOrg(c.Project.Org, name)
		key := repoPauseKey(c.Project.Org, name)
		if key == "" {
			continue
		}
		idx := -1
		for i, rp := range c.Project.RepoPolicies {
			if repoPauseKey(c.Project.Org, rp.Repo) == key {
				idx = i
				break
			}
		}
		if enabled == nil {
			if idx >= 0 && c.Project.RepoPolicies[idx].SelfAuthorizationHold != nil {
				c.Project.RepoPolicies = append(c.Project.RepoPolicies[:idx:idx], c.Project.RepoPolicies[idx+1:]...)
				changed = true
			}
			continue
		}
		v := *enabled
		if idx >= 0 {
			if c.Project.RepoPolicies[idx].SelfAuthorizationHold == nil || *c.Project.RepoPolicies[idx].SelfAuthorizationHold != v {
				c.Project.RepoPolicies[idx].SelfAuthorizationHold = &v
				changed = true
			}
			continue
		}
		c.Project.RepoPolicies = append(c.Project.RepoPolicies, RepoPolicy{Repo: name, SelfAuthorizationHold: &v})
		changed = true
	}
	return changed
}

// ClearRepoPolicies removes every per-repo policy override. It is used when a
// repo-list save migrates the hive to another org, where bare overrides from
// the previous org must not silently retarget to same-named repos.
func (c *Config) ClearRepoPolicies() bool {
	if c == nil {
		return false
	}
	repoPauseMu.Lock()
	defer repoPauseMu.Unlock()
	if len(c.Project.RepoPolicies) == 0 {
		return false
	}
	c.Project.RepoPolicies = nil
	return true
}

// PruneRepoPoliciesToWatched drops overrides for repos no longer listed in
// project.repos. The current org is used for matching, so callers that are
// changing orgs should ClearRepoPolicies first instead.
func (c *Config) PruneRepoPoliciesToWatched() bool {
	if c == nil || len(c.Project.RepoPolicies) == 0 {
		return false
	}
	watched := make(map[string]bool, len(c.Project.Repos))
	for _, repo := range c.Project.Repos {
		if key := repoPauseKey(c.Project.Org, repo); key != "" {
			watched[key] = true
		}
	}
	repoPauseMu.Lock()
	defer repoPauseMu.Unlock()
	next := c.Project.RepoPolicies[:0]
	for _, rp := range c.Project.RepoPolicies {
		if watched[repoPauseKey(c.Project.Org, rp.Repo)] {
			next = append(next, rp)
		}
	}
	if len(next) == len(c.Project.RepoPolicies) {
		return false
	}
	c.Project.RepoPolicies = next
	if len(c.Project.RepoPolicies) == 0 {
		c.Project.RepoPolicies = nil
	}
	return true
}
