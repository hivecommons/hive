package adminmcp

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// sentinel.settings' repo_sensitive_paths: one repository's own Sentinel
// sensitive-path list (project.repo_policies[].sentinel_sensitive_paths). The
// security endpoint takes it per repository under
// sentinel.repoSensitivePaths ({"owner/repo": [globs] | null}) and leaves
// repositories it is not sent alone, so the op sends only the repositories
// that change. The preview's fingerprint already covers every repository's
// list (sentinelState.RepoOverrides), so a list changed after the preview
// refuses the confirm.

func sentinelRepoPathsSchema() map[string]any {
	list := map[string]any{"type": "array", "items": stringSchema()}
	return map[string]any{"type": "object", "description": "owner/repo to every glob that repository's own sensitive-path list should hold; it replaces the hive-wide sensitive_paths for that repository's pull requests. [] flags nothing by path there; null removes the repository's list so it uses the hive-wide list again. Repositories you do not name keep theirs; the hive-wide list is not changed.", "additionalProperties": map[string]any{"anyOf": []any{list, map[string]any{"type": "null"}}}}
}

// parseSentinelRepoPathsArg reads repo_sensitive_paths: nil when absent, a
// nil value for a repository to clear.
func parseSentinelRepoPathsArg(args map[string]any, where string) (map[string]*[]string, error) {
	m, err := objectArg(args, "repo_sensitive_paths", where)
	if err != nil || m == nil {
		return nil, err
	}
	name := where + ".repo_sensitive_paths"
	if len(m) == 0 {
		return nil, fmt.Errorf("%s names no repository", name)
	}
	out := make(map[string]*[]string, len(m))
	for _, repo := range SortKeys(m) {
		if strings.TrimSpace(repo) == "" {
			return nil, fmt.Errorf("%s has an empty repository name", name)
		}
		if m[repo] == nil {
			out[repo] = nil
			continue
		}
		field := name + "[" + repo + "]"
		list, err := namedStringList(m[repo], field)
		if err != nil {
			return nil, err
		}
		paths, err := config.NormalizeRepoSentinelSensitivePaths(field, list)
		if err != nil {
			return nil, err
		}
		out[repo] = &paths
	}
	return out, nil
}

func sentinelRepoOverrideIndex(list []sentinelRepoPathsOverride, repo string) int {
	for i, o := range list {
		if strings.EqualFold(o.Repo, repo) {
			return i
		}
	}
	return -1
}

// applySentinelRepoPaths sets or clears the named repositories' lists on
// after, adds the per-repository patch to body, and returns the preview rows
// and notes.
func applySentinelRepoPaths(after *sentinelState, before sentinelState, req map[string]*[]string, body map[string]any) ([]settingChange, []string, error) {
	const field = "sentinel.repo_sensitive_paths"
	if !before.repoPathsSettable {
		return nil, nil, fmt.Errorf("this hive cannot set %s through Settings → Security (it runs an older build); upgrade it, or set project.repo_policies[].sentinel_sensitive_paths in hive.yaml", field)
	}
	next := append([]sentinelRepoPathsOverride(nil), before.RepoOverrides...)
	patch := map[string]any{}
	var changes []settingChange
	var named []string
	touched := map[string]bool{}
	for _, key := range slices.Sorted(maps.Keys(req)) {
		repo := qualifyProjectRepo(strings.TrimSpace(key), before.projectRepos)
		watched := false
		for _, full := range before.projectRepos {
			if strings.EqualFold(full, repo) {
				repo, watched = full, true
			}
		}
		if !watched {
			return nil, nil, fmt.Errorf("%s: %q is not a repository in project.repos", field, key)
		}
		if touched[strings.ToLower(repo)] {
			return nil, nil, fmt.Errorf("%s names %s twice", field, repo)
		}
		touched[strings.ToLower(repo)] = true
		idx := sentinelRepoOverrideIndex(next, repo)
		row := field + "[" + repo + "]"
		if req[key] == nil {
			if idx < 0 {
				return nil, nil, fmt.Errorf("%s: %s has no list of its own to remove; it already uses the hive-wide list", field, repo)
			}
			changes = append(changes, settingChange{Field: row, Before: next[idx].SensitivePaths, After: "hive-wide"})
			next = append(next[:idx:idx], next[idx+1:]...)
			patch[repo] = nil
			named = append(named, repo)
			continue
		}
		paths := *req[key]
		if idx >= 0 {
			if sameList(next[idx].SensitivePaths, paths, foldExact) {
				continue
			}
			changes = append(changes, settingChange{Field: row, Before: next[idx].SensitivePaths, After: paths})
			next[idx] = sentinelRepoPathsOverride{Repo: next[idx].Repo, SensitivePaths: paths}
		} else {
			changes = append(changes, settingChange{Field: row, Before: "hive-wide", After: paths})
			next = append(next, sentinelRepoPathsOverride{Repo: repo, SensitivePaths: paths})
		}
		patch[repo] = paths
		named = append(named, repo)
	}
	if len(patch) == 0 {
		return nil, nil, nil
	}
	body["repoSensitivePaths"] = patch
	after.RepoOverrides = next
	slices.Sort(named)
	return changes, []string{"Sets project.repo_policies[].sentinel_sensitive_paths for " + strings.Join(named, ", ") + " only. The hive-wide sensitive_paths and every other repository keep their lists; a repository set to null uses the hive-wide list again, and an empty list flags no path there."}, nil
}

// sentinelRepoPathsDisclosure compares, for each repository whose own list
// the write sets or clears, the globs Sentinel checks there before and after:
// a glob dropped is a widening ("Sentinel stops flagging … in owner/repo").
// A write that changes no repository's list returns nothing.
func sentinelRepoPathsDisclosure(before, after sentinelState) (fewer, more []string) {
	for _, repo := range sentinelRepoOverridesChanged(before, after) {
		effBefore, effAfter := before.SensitivePaths, after.SensitivePaths
		if i := sentinelRepoOverrideIndex(before.RepoOverrides, repo); i >= 0 {
			effBefore = before.RepoOverrides[i].SensitivePaths
		}
		ownEmpty := false
		if i := sentinelRepoOverrideIndex(after.RepoOverrides, repo); i >= 0 {
			effAfter = after.RepoOverrides[i].SensitivePaths
			ownEmpty = len(effAfter) == 0
		}
		added, removed := listDiff(effBefore, effAfter, foldExact)
		if len(removed) > 0 {
			line := "Sentinel stops flagging " + listLabel(removed) + " in " + repo
			if ownEmpty {
				line += " (its own list is empty, so the sensitive_path rule flags no path there)"
			}
			fewer = append(fewer, line)
		}
		if len(added) > 0 {
			more = append(more, "Sentinel flags "+listLabel(added)+" in "+repo)
		}
	}
	return fewer, more
}

// sentinelRepoOverridesChanged names the repositories whose own list differs
// between before and after (set, changed or cleared).
func sentinelRepoOverridesChanged(before, after sentinelState) []string {
	var out []string
	for _, o := range after.RepoOverrides {
		i := sentinelRepoOverrideIndex(before.RepoOverrides, o.Repo)
		if i < 0 || !sameList(before.RepoOverrides[i].SensitivePaths, o.SensitivePaths, foldExact) {
			out = append(out, o.Repo)
		}
	}
	for _, o := range before.RepoOverrides {
		if sentinelRepoOverrideIndex(after.RepoOverrides, o.Repo) < 0 {
			out = append(out, o.Repo)
		}
	}
	return out
}

// sentinelRepoOverridesUntouched is after's per-repository lists less the ones
// this write sets or changes: the lists already in force that it leaves alone.
func sentinelRepoOverridesUntouched(before, after sentinelState) []sentinelRepoPathsOverride {
	changed := map[string]bool{}
	for _, repo := range sentinelRepoOverridesChanged(before, after) {
		changed[strings.ToLower(repo)] = true
	}
	var out []sentinelRepoPathsOverride
	for _, o := range after.RepoOverrides {
		if !changed[strings.ToLower(o.Repo)] {
			out = append(out, o)
		}
	}
	return out
}
