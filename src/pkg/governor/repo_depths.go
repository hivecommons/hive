package governor

import ghpkg "github.com/hivecommons/hive/pkg/github"

// RepoDepthsFromActionable derives per-repo ACTIONABLE queue depths from the
// same filtered issue and PR items that feed the aggregate governor counts.
// TotalByRepo contributes only the roster of successfully scanned repos, so a
// zero-actionable repo is represented as IDLE while repos skipped after a fetch
// error are not silently invented as idle.
func RepoDepthsFromActionable(actionable *ghpkg.ActionableResult) map[string]RepoSnapshot {
	if actionable == nil {
		return map[string]RepoSnapshot{}
	}
	depths := make(map[string]RepoSnapshot, len(actionable.TotalByRepo))
	for repo := range actionable.TotalByRepo {
		depths[repo] = RepoSnapshot{}
	}
	for _, issue := range actionable.Issues.Items {
		depth := depths[issue.Repo]
		depth.Issues++
		depths[issue.Repo] = depth
	}
	for _, pr := range actionable.PRs.Items {
		depth := depths[pr.Repo]
		depth.PRs++
		depths[pr.Repo] = depth
	}
	return depths
}
