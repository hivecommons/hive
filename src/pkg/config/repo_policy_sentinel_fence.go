package config

import (
	"fmt"
	"strings"
)

// Per-repository overrides of two hive-wide merge-safety keys:
//
//   - repo_policies[].sentinel_sensitive_paths overrides
//     sentinel.sensitive_paths for one repository's pull requests.
//   - repo_policies[].workflow_fence_scope overrides
//     auto_merge.contributor_lane.workflow_fence_scope for one repository.
//
// Both are opt-in: a repository without the key reads the hive-wide value,
// so a config that never sets them behaves exactly as before. An override
// may be stricter or looser than the hive-wide value, and a later change
// to the hive-wide value does not touch a repository that overrides it.
//
// Fail closed: Validate rejects an invalid override, so a config carrying
// one does not load or reload. If one reaches the running config anyway,
// the resolvers ignore it and use the hive-wide value.

// RepoSentinelSensitivePaths returns the Sentinel sensitive-path globs for
// repo and whether they come from its repo_policies override. Without a
// valid override it returns the hive-wide list (sentinel.sensitive_paths,
// or the shipped defaults when that is unset). An explicit empty override
// is returned as a non-nil empty list, which matches nothing.
func (c *Config) RepoSentinelSensitivePaths(repo string) ([]string, bool) {
	if c == nil {
		return SentinelConfig{}.EffectiveSensitivePaths(), false
	}
	if rp, ok := c.RepoPolicyFor(repo); ok && rp.SentinelSensitivePaths != nil &&
		ValidateRepoSentinelSensitivePaths("", rp.SentinelSensitivePaths) == nil {
		return append([]string{}, rp.SentinelSensitivePaths...), true
	}
	return c.Sentinel.EffectiveSensitivePaths(), false
}

// RepoWorkflowFenceScope returns the contributor lane's workflow fence scope
// for repo ("all" or "human_merge_paths") and whether it comes from its
// repo_policies override. Without a valid override it returns the hive-wide
// auto_merge.contributor_lane.workflow_fence_scope.
func (c *Config) RepoWorkflowFenceScope(repo string) (string, bool) {
	if c == nil {
		return ContributorLaneWorkflowFenceScopeAll, false
	}
	if rp, ok := c.RepoPolicyFor(repo); ok {
		if scope, valid := repoPolicyFenceScopeValue(rp.WorkflowFenceScope); valid {
			return scope, true
		}
	}
	return c.AutoMerge.ContributorLane.EffectiveWorkflowFenceScope(), false
}

// repoPolicyFenceScopeValue normalizes a repo_policies[].workflow_fence_scope
// value. ok is false for an unset or unknown value.
func repoPolicyFenceScopeValue(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case ContributorLaneWorkflowFenceScopeAll:
		return ContributorLaneWorkflowFenceScopeAll, true
	case ContributorLaneWorkflowFenceScopeHumanMergePaths:
		return ContributorLaneWorkflowFenceScopeHumanMergePaths, true
	}
	return "", false
}

// ValidateRepoSentinelSensitivePaths rejects blank patterns in a
// repo_policies[].sentinel_sensitive_paths override, as ValidateSentinel
// does for the hive-wide list. nil (unset) and an empty list are valid.
func ValidateRepoSentinelSensitivePaths(field string, paths []string) error {
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("%s: sentinel_sensitive_paths must not contain blank patterns", field)
		}
	}
	return nil
}

// ValidateRepoWorkflowFenceScope accepts "" (unset), "all" and
// "human_merge_paths" (any case).
func ValidateRepoWorkflowFenceScope(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	if _, ok := repoPolicyFenceScopeValue(value); !ok {
		return fmt.Errorf("%s: invalid workflow_fence_scope %q (allowed: %s)", field, value, strings.Join(ContributorLaneWorkflowFenceScopes, ", "))
	}
	return nil
}

// RepoSensitivePathsOverride is one repository's Sentinel sensitive-path
// override, as the dashboard and admin MCP show it.
type RepoSensitivePathsOverride struct {
	Repo  string
	Paths []string
}

// RepoWorkflowFenceScopeOverride is one repository's workflow fence scope
// override, as the dashboard and admin MCP show it.
type RepoWorkflowFenceScopeOverride struct {
	Repo  string
	Scope string
}

// SentinelSensitivePathsRepoOverrides lists the repositories whose valid
// repo_policies override replaces sentinel.sensitive_paths, in config
// order, with org-qualified names.
func (c *Config) SentinelSensitivePathsRepoOverrides() []RepoSensitivePathsOverride {
	var out []RepoSensitivePathsOverride
	for _, rp := range c.repoPoliciesSnapshot() {
		if rp.SentinelSensitivePaths == nil || ValidateRepoSentinelSensitivePaths("", rp.SentinelSensitivePaths) != nil {
			continue
		}
		out = append(out, RepoSensitivePathsOverride{Repo: c.repoPolicyDisplayName(rp.Repo), Paths: append([]string{}, rp.SentinelSensitivePaths...)})
	}
	return out
}

// WorkflowFenceScopeRepoOverrides lists the repositories whose valid
// repo_policies override replaces
// auto_merge.contributor_lane.workflow_fence_scope, in config order, with
// org-qualified names.
func (c *Config) WorkflowFenceScopeRepoOverrides() []RepoWorkflowFenceScopeOverride {
	var out []RepoWorkflowFenceScopeOverride
	for _, rp := range c.repoPoliciesSnapshot() {
		scope, ok := repoPolicyFenceScopeValue(rp.WorkflowFenceScope)
		if !ok {
			continue
		}
		out = append(out, RepoWorkflowFenceScopeOverride{Repo: c.repoPolicyDisplayName(rp.Repo), Scope: scope})
	}
	return out
}

func (c *Config) repoPoliciesSnapshot() []RepoPolicy {
	if c == nil {
		return nil
	}
	repoPauseMu.RLock()
	defer repoPauseMu.RUnlock()
	return append([]RepoPolicy(nil), c.Project.RepoPolicies...)
}

func (c *Config) repoPolicyDisplayName(repo string) string {
	return QualifyRepo(c.Project.Org, repo)
}
