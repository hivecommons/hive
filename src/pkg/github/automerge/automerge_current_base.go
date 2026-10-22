package automerge

import (
	"context"
	"strings"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

const (
	// autoMergeReasonCurrentBaseCheck: auto_merge.require_current_base is on
	// for the repo but the base...head comparison could not be read. The
	// merge waits rather than landing a head of unknown freshness.
	autoMergeReasonCurrentBaseCheck = "current-base-check"
	// currentBaseUpdateReason is the update-branch claim reason recorded when
	// a green head is refreshed because the base moved under it.
	currentBaseUpdateReason = "base-moved"
)

func (c *Engine) requireCurrentBaseFor(repo string) bool {
	if c == nil || c.requireCurrentBase == nil {
		return false
	}
	return c.requireCurrentBase(repo)
}

// currentBaseGate enforces auto_merge.require_current_base
// (hivecommons/hive#13634). GitHub only reports mergeable_state "behind" when
// branch protection requires up-to-date branches, so a head that was green
// against an older base otherwise merges untested against everything landed
// since; a semantic conflict then breaks the base for every open PR. When the
// repo opts in, the head must contain the current base tip: if it does not,
// the branch is updated (a merge commit, no human commit rewritten) and the
// merge waits for CI on the new head. holdReason is returned instead when
// the caller may not update the branch this tick.
//
// stop is false when the merge may proceed; otherwise the caller returns the
// event, reason and error as-is.
func (c *Engine) currentBaseGate(ctx context.Context, displayRepo, owner, repo string, number int, baseBranch, headSHA string, branchUpdateAllowed bool, holdReason string) (AutoMergeSweepEvent, string, bool, error) {
	if !c.requireCurrentBaseFor(owner+"/"+repo) || c.gh == nil {
		return AutoMergeSweepEvent{}, "", false, nil
	}
	baseBranch = strings.TrimSpace(baseBranch)
	if baseBranch == "" || strings.TrimSpace(headSHA) == "" {
		return AutoMergeSweepEvent{}, autoMergeReasonCurrentBaseCheck, true, nil
	}
	cmp, _, err := c.gh.Repositories.CompareCommits(hgithub.WithRESTCaller(ctx, "hive:automerge_current_base"), owner, repo, baseBranch, headSHA, &gh.ListOptions{PerPage: 1})
	if err != nil {
		return AutoMergeSweepEvent{}, autoMergeReasonCurrentBaseCheck, true, err
	}
	if cmp.GetBehindBy() <= 0 {
		return AutoMergeSweepEvent{}, "", false, nil
	}
	if !branchUpdateAllowed {
		return AutoMergeSweepEvent{}, holdReason, true, nil
	}
	c.info("automerge head is behind the current base; updating before merge", "repo", displayRepo, "pr", number, "base", baseBranch, "behind_by", cmp.GetBehindBy())
	event, reason, err := c.updateBranchAndSkip(ctx, displayRepo, owner, repo, number, headSHA, currentBaseUpdateReason)
	return event, reason, true, err
}
