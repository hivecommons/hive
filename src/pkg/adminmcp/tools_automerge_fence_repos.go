package adminmcp

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// auto_merge.update's contributor_lane.workflow_fence_scope_repos: one
// repository's own workflow fence scope (project.repo_policies[].
// workflow_fence_scope). Unlike the other merge settings, the endpoint takes
// it per repository ({"owner/repo": "all" | "human_merge_paths" | null}) and
// leaves repositories it is not sent alone, so the op sends only the
// repositories that change. The hive reports the overrides as a list under
// the same name; the preview reads that list as a repository → scope map.

const (
	amFenceReposKey      = "contributor_lane.workflow_fence_scope_repos"
	amFenceSettableKey   = "contributor_lane.workflow_fence_scope_repos_settable"
	amFenceHiveWideKey   = "contributor_lane.workflow_fence_scope"
	amFenceHumanPathsKey = "human_merge_paths"
)

func amFenceReposSchema() map[string]any {
	scope := map[string]any{"type": "string", "enum": config.ContributorLaneWorkflowFenceScopes}
	return map[string]any{"type": "object", "description": "owner/repo to that repository's own workflow fence scope: all fences every workflow / action edit, human_merge_paths only edits to files in its human_merge_paths. null removes the repository's override so it uses the hive-wide contributor_lane.workflow_fence_scope again. Repositories you do not name keep theirs; the hive-wide value is not changed.", "additionalProperties": map[string]any{"anyOf": []any{scope, map[string]any{"type": "null"}}}}
}

// amFenceRepoOverrides reads the overrides as the hive reported them, a list
// of {repo, workflow_fence_scope}, or as a map this file built.
func amFenceRepoOverrides(v any) map[string]string {
	out := map[string]string{}
	switch x := v.(type) {
	case map[string]string:
		for repo, scope := range x {
			out[repo] = scope
		}
	case []any:
		for _, item := range x {
			m, _ := item.(map[string]any)
			repo, _ := m["repo"].(string)
			scope, _ := m["workflow_fence_scope"].(string)
			if strings.TrimSpace(repo) != "" && scope != "" {
				out[repo] = scope
			}
		}
	}
	return out
}

func amFenceRepoKey(m map[string]string, repo string) (string, bool) {
	for k := range m {
		if strings.EqualFold(k, repo) {
			return k, true
		}
	}
	return "", false
}

// amFenceReposApply sets or clears the named repositories' scopes on top of
// before and returns every override after the change.
func amFenceReposApply(before, raw any, projectRepos []string) (map[string]string, error) {
	change, ok := raw.(map[string]any)
	if !ok || len(change) == 0 {
		return nil, fmt.Errorf("%s takes {\"owner/repo\": \"all\" | \"human_merge_paths\" | null}", amFenceReposKey)
	}
	out := amFenceRepoOverrides(before)
	touched := map[string]bool{}
	for _, key := range SortKeys(change) {
		repo := qualifyProjectRepo(strings.TrimSpace(key), projectRepos)
		watched := false
		for _, full := range projectRepos {
			if strings.EqualFold(full, repo) {
				repo, watched = full, true
			}
		}
		if !watched {
			return nil, fmt.Errorf("%s: %q is not a repository in project.repos", amFenceReposKey, key)
		}
		if touched[strings.ToLower(repo)] {
			return nil, fmt.Errorf("%s names %s twice", amFenceReposKey, repo)
		}
		touched[strings.ToLower(repo)] = true
		existing, has := amFenceRepoKey(out, repo)
		if change[key] == nil {
			if !has {
				return nil, fmt.Errorf("%s: %s has no override to remove; it already uses the hive-wide scope", amFenceReposKey, repo)
			}
			delete(out, existing)
			continue
		}
		s, _ := change[key].(string)
		s = strings.ToLower(strings.TrimSpace(s))
		if s != config.ContributorLaneWorkflowFenceScopeAll && s != config.ContributorLaneWorkflowFenceScopeHumanMergePaths {
			return nil, fmt.Errorf("%s[%s] must be one of %s, or null to use the hive-wide scope", amFenceReposKey, repo, strings.Join(config.ContributorLaneWorkflowFenceScopes, ", "))
		}
		if has {
			repo = existing
		}
		out[repo] = s
	}
	return out, nil
}

// amFenceReposPatch is the body the endpoint takes: only the repositories
// whose override changes, null for one that goes back to the hive-wide scope.
func amFenceReposPatch(before, after map[string]string) map[string]any {
	patch := map[string]any{}
	for repo, scope := range after {
		if b, ok := before[repo]; !ok || b != scope {
			patch[repo] = scope
		}
	}
	for repo := range before {
		if _, ok := after[repo]; !ok {
			patch[repo] = nil
		}
	}
	return patch
}

func amFenceScopeLabel(m map[string]string, repo, hiveWide string) string {
	if s, ok := m[repo]; ok {
		return s
	}
	return "hive-wide (" + hiveWide + ")"
}

func amFenceEffective(m map[string]string, repo, hiveWide string) string {
	if s, ok := m[repo]; ok {
		return s
	}
	return hiveWide
}

// amFenceReposRows is one preview row per repository whose override changes.
func amFenceReposRows(c amPlanned, hiveWide string) []amRow {
	before := amFenceRepoOverrides(c.before)
	after := amFenceRepoOverrides(c.after)
	var rows []amRow
	for _, repo := range SortKeys(amFenceReposPatch(before, after)) {
		rows = append(rows, amRow{field: c.field.key + "[" + repo + "]", before: amFenceScopeLabel(before, repo, hiveWide), after: amFenceScopeLabel(after, repo, hiveWide)})
	}
	return rows
}

// amFenceReposFinish completes a plan that changes per-repository fence
// scopes: it refuses a hive that cannot take them, names what loosens, adds
// the fail-closed reminder, and adds the hive-wide scope and human-merge paths
// the disclosure was built from to the confirm's fingerprint. A plan without
// that change is returned untouched.
func amFenceReposFinish(plan *amPlan, current map[string]any, beforeValues map[string]any) error {
	idx := -1
	for i := range plan.changes {
		if plan.changes[i].field.kind == amFenceScopeRepos {
			idx = i
		}
	}
	if idx < 0 {
		return nil
	}
	if settable, _ := lookupSetting(current, amFenceSettableKey); settable != true {
		return fmt.Errorf("this hive reports %s but cannot set it through the merge settings endpoint (it runs an older build); upgrade it, or set project.repo_policies[].workflow_fence_scope in hive.yaml", amFenceReposKey)
	}
	hiveWide, _ := lookupSetting(current, amFenceHiveWideKey)
	hiveWideScope, _ := hiveWide.(string)
	if hiveWideScope == "" {
		hiveWideScope = config.ContributorLaneWorkflowFenceScopeAll
	}
	paths := repoPathsValue(current[amFenceHumanPathsKey])
	if sent, ok := plan.body[amFenceHumanPathsKey]; ok {
		paths = repoPathsValue(sent)
	}
	hasPaths := func(repo string) bool {
		for k, patterns := range paths {
			if strings.EqualFold(k, repo) && len(patterns) > 0 {
				return true
			}
		}
		return false
	}
	c := &plan.changes[idx]
	c.fenceHiveWide = hiveWideScope
	before := amFenceRepoOverrides(c.before)
	after := amFenceRepoOverrides(c.after)
	var named, noPaths []string
	for _, repo := range SortKeys(amFenceReposPatch(before, after)) {
		named = append(named, repo)
		if !hasPaths(repo) {
			noPaths = append(noPaths, repo)
		}
		was, now := amFenceEffective(before, repo, hiveWideScope), amFenceEffective(after, repo, hiveWideScope)
		if was == config.ContributorLaneWorkflowFenceScopeHumanMergePaths || now != config.ContributorLaneWorkflowFenceScopeHumanMergePaths {
			continue
		}
		note := c.field.key + "[" + repo + "] " + amFenceScopeLabel(before, repo, hiveWideScope) + " → " + amFenceScopeLabel(after, repo, hiveWideScope) + ": workflow edits outside human-merge paths in " + repo + " merge without an owner Approve"
		if !hasPaths(repo) {
			note += " (" + repo + " has no human_merge_paths entry yet, so it keeps the full fence until one is set)"
		}
		c.notes = append(c.notes, note)
	}
	reminder := "A repository whose scope is human_merge_paths but which has no human_merge_paths entry keeps the full fence: every workflow edit there still waits for an owner Approve."
	if len(noPaths) > 0 {
		reminder += " Of the repositories named here, " + strings.Join(noPaths, ", ") + " " + amFenceHasHave(len(noPaths)) + " no human_merge_paths entry."
	} else {
		reminder += " Every repository named here has a human_merge_paths entry."
	}
	plan.effects = append(plan.effects,
		"Sets project.repo_policies[].workflow_fence_scope for "+strings.Join(named, ", ")+" only. The hive-wide workflow_fence_scope ("+hiveWideScope+") and every other repository keep their values; a repository set back to null uses the hive-wide scope again.",
		reminder)
	beforeValues[amFenceHiveWideKey] = hiveWideScope
	beforeValues[amFenceHumanPathsKey] = current[amFenceHumanPathsKey]
	return nil
}

func amFenceHasHave(n int) string {
	if n == 1 {
		return "has"
	}
	return "have"
}
