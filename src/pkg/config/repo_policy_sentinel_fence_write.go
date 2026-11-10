package config

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Writes for the per-repository Sentinel sensitive-path list and workflow
// fence scope (repo_policy_sentinel_fence.go). The dashboard's merge and
// Security settings and the admin MCP auto_merge.update and sentinel.settings
// operations set them through these, so an owner can change one repository's
// override without a hand edit of hive.yaml.
//
// Both setters edit one repository at a time and nothing else: they never
// touch the hive-wide sentinel.sensitive_paths or
// auto_merge.contributor_lane.workflow_fence_scope, nor any other field of
// the repository's policy entry. A nil value clears the repository's
// override, so it reads the hive-wide value again. An invalid value refuses
// the whole batch before anything changes. Persistence is the caller's job.

// RepoOverrideHiveWide is how a change line names "no override": the
// repository reads the hive-wide value.
const RepoOverrideHiveWide = "hive-wide"

// NormalizeRepoSentinelSensitivePaths trims each pattern, drops exact
// duplicates and checks the result: a blank pattern is refused (as
// ValidateRepoSentinelSensitivePaths does when hive.yaml loads) and so is a
// malformed glob, which would otherwise never match. An empty list stays a
// non-nil empty list: it flags nothing by path.
func NormalizeRepoSentinelSensitivePaths(field string, paths []string) ([]string, error) {
	out := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, ValidateRepoSentinelSensitivePaths(field, []string{p})
		}
		if _, err := filepath.Match(filepath.ToSlash(p), ""); err != nil {
			return nil, fmt.Errorf("%s: sentinel_sensitive_paths: %q is not a valid glob", field, p)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, nil
}

// SetRepoWorkflowFenceScopes records or clears per-repository
// workflow_fence_scope overrides in memory. A value ("all" or
// "human_merge_paths", any case) sets the override; nil clears it. It returns
// one "owner/repo: before → after" line per repository it changed, sorted.
func (c *Config) SetRepoWorkflowFenceScopes(values map[string]*string) ([]string, error) {
	if c == nil {
		return nil, fmt.Errorf("no config loaded")
	}
	next := make(map[string]string, len(values))
	for _, repo := range sortedRepoOverrideKeys(values) {
		scope := ""
		if v := values[repo]; v != nil {
			normalized, ok := repoPolicyFenceScopeValue(*v)
			if !ok {
				return nil, fmt.Errorf("workflow_fence_scope for %s: %q is not one of %s", repo, *v, strings.Join(ContributorLaneWorkflowFenceScopes, ", "))
			}
			scope = normalized
		}
		next[repo] = scope
	}
	repoPauseMu.Lock()
	defer repoPauseMu.Unlock()
	var changes []string
	for _, repo := range sortedRepoOverrideKeys(values) {
		scope := next[repo]
		before := RepoOverrideHiveWide
		if c.editRepoSentinelFencePolicyLocked(repo, func(rp *RepoPolicy) bool {
			if cur, ok := repoPolicyFenceScopeValue(rp.WorkflowFenceScope); ok {
				before = cur
			}
			if rp.WorkflowFenceScope == scope {
				return false
			}
			rp.WorkflowFenceScope = scope
			return true
		}) {
			after := scope
			if after == "" {
				after = RepoOverrideHiveWide
			}
			changes = append(changes, QualifyRepo(c.Project.Org, repo)+": "+before+" → "+after)
		}
	}
	return changes, nil
}

// SetRepoSentinelSensitivePaths records or clears per-repository
// sentinel_sensitive_paths overrides in memory. A list (normalized by
// NormalizeRepoSentinelSensitivePaths) replaces the repository's own list,
// and an empty list flags nothing by path there; nil clears the override. It
// returns one "owner/repo: before → after" line per repository it changed,
// sorted.
func (c *Config) SetRepoSentinelSensitivePaths(values map[string]*[]string) ([]string, error) {
	if c == nil {
		return nil, fmt.Errorf("no config loaded")
	}
	next := make(map[string][]string, len(values))
	for _, repo := range sortedRepoOverrideKeys(values) {
		if v := values[repo]; v != nil {
			paths, err := NormalizeRepoSentinelSensitivePaths("repo "+repo, *v)
			if err != nil {
				return nil, err
			}
			next[repo] = paths
		}
	}
	repoPauseMu.Lock()
	defer repoPauseMu.Unlock()
	var changes []string
	for _, repo := range sortedRepoOverrideKeys(values) {
		paths := next[repo]
		before := RepoOverrideHiveWide
		if c.editRepoSentinelFencePolicyLocked(repo, func(rp *RepoPolicy) bool {
			if rp.SentinelSensitivePaths != nil {
				before = repoOverridePathsLabel(rp.SentinelSensitivePaths)
			}
			if (rp.SentinelSensitivePaths == nil) == (paths == nil) && strings.Join(rp.SentinelSensitivePaths, "\n") == strings.Join(paths, "\n") {
				return false
			}
			rp.SentinelSensitivePaths = paths
			return true
		}) {
			after := RepoOverrideHiveWide
			if paths != nil {
				after = repoOverridePathsLabel(paths)
			}
			changes = append(changes, QualifyRepo(c.Project.Org, repo)+": "+before+" → "+after)
		}
	}
	return changes, nil
}

func repoOverridePathsLabel(paths []string) string {
	return "[" + strings.Join(paths, ", ") + "]"
}

func sortedRepoOverrideKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// editRepoSentinelFencePolicyLocked applies edit to repo's policy entry,
// adding one when the repository has none, and reports whether edit changed
// anything. An entry the edit leaves with no override is removed, unless it
// still carries forge settings (repoPolicyHasNoOverrides does not count
// those). Callers hold repoPauseMu.
func (c *Config) editRepoSentinelFencePolicyLocked(repo string, edit func(*RepoPolicy) bool) bool {
	name, _ := NormalizeRepoForOrg(c.Project.Org, strings.TrimSpace(repo))
	key := repoPauseKey(c.Project.Org, name)
	if key == "" {
		return false
	}
	for i := range c.Project.RepoPolicies {
		if repoPauseKey(c.Project.Org, c.Project.RepoPolicies[i].Repo) != key {
			continue
		}
		if !edit(&c.Project.RepoPolicies[i]) {
			return false
		}
		rp := c.Project.RepoPolicies[i]
		if repoPolicyHasNoOverrides(rp) && rp.Forge == "" && rp.ForgeURL == "" {
			c.Project.RepoPolicies = append(c.Project.RepoPolicies[:i:i], c.Project.RepoPolicies[i+1:]...)
		}
		return true
	}
	rp := RepoPolicy{Repo: name}
	if !edit(&rp) || repoPolicyHasNoOverrides(rp) {
		return false
	}
	c.Project.RepoPolicies = append(c.Project.RepoPolicies, rp)
	return true
}

// MarshalYAML keeps an explicit empty sentinel_sensitive_paths. The field is
// omitempty, which drops an empty list exactly as it drops nil, so without
// this a saved "flag nothing by path" override came back as unset (the
// hive-wide list) on the next load. Every other policy marshals unchanged.
func (rp RepoPolicy) MarshalYAML() (interface{}, error) {
	type plain RepoPolicy
	if rp.SentinelSensitivePaths == nil || len(rp.SentinelSensitivePaths) > 0 {
		return plain(rp), nil
	}
	var node yaml.Node
	if err := node.Encode(plain(rp)); err != nil {
		return nil, err
	}
	if node.Kind != yaml.MappingNode {
		return plain(rp), nil
	}
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "sentinel_sensitive_paths"},
		&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle})
	return &node, nil
}
