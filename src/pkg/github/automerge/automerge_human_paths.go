package automerge

import (
	"context"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/intent"
)

// Skip reasons reported by the human-merge-path gate (#11038).
const (
	// autoMergeReasonHumanMergePath: the PR touches a path listed in
	// auto_merge.human_merge_paths for its repo; it is held for a person.
	autoMergeReasonHumanMergePath = "human-merge-path"
	// autoMergeReasonHumanMergePathEvidence: the repo has human-merge paths
	// but the complete changed-file list could not be fetched. Fails CLOSED.
	autoMergeReasonHumanMergePathEvidence = "human-merge-path-evidence"
)

// SetHumanMergePaths installs (or, with nil, removes) the per-repo
// auto_merge.human_merge_paths lookup every sweep lane consults. fn is called
// on every evaluation with "owner/repo", so a config reload takes effect
// without restarting the sweep.
func (c *Engine) SetHumanMergePaths(fn func(repo string) []string) {
	if c == nil {
		return
	}
	c.humanMergePathsMu.Lock()
	defer c.humanMergePathsMu.Unlock()
	c.humanMergePaths = fn
}

func (c *Engine) currentHumanMergePaths(repo string) []string {
	if c == nil {
		return nil
	}
	c.humanMergePathsMu.RLock()
	fn := c.humanMergePaths
	c.humanMergePathsMu.RUnlock()
	if fn == nil {
		return nil
	}
	return fn(repo)
}

// humanMergePathGate refuses a PR that touches a path a person must merge.
// It is independent of intent tier and of IntentGate/intent.enforce: the App
// self-merge contract (EvaluateForAppSelfMerge) authorizes Tier 2/3 without a
// person by design (#6258), so this list is the operator's only way to keep a
// path out of every App merge lane.
//
// It returns ("", nil) when the merge may proceed. An unconfigured repo costs
// no API call. When the repo is configured and the complete changed-file list
// cannot be fetched it fails closed with a non-nil error. On a match it adds
// `hold`, posts the single marker-stamped comment (not reposted on later
// ticks) and returns autoMergeReasonHumanMergePath.
func (c *Engine) humanMergePathGate(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest) (string, error) {
	patterns := c.currentHumanMergePaths(owner + "/" + repo)
	if len(patterns) == 0 {
		return "", nil
	}
	number := pr.GetNumber()
	files, err := ListChangedFiles(ctx, c.gh, owner, repo, pr)
	if err != nil {
		return autoMergeReasonHumanMergePathEvidence, err
	}
	matches := intent.HumanMergePathMatches(files, patterns)
	if len(matches) == 0 {
		return "", nil
	}
	c.info("automerge sweep held PR touching human-merge paths", "repo", displayRepo, "pr", number, "paths", matches, "config", hgithub.HumanMergePathsConfigKey)
	if err := hgithub.HoldForHumanMergePaths(ctx, c.gh, owner, repo, number, matches); err != nil {
		return autoMergeReasonHumanMergePath, err
	}
	return autoMergeReasonHumanMergePath, nil
}
