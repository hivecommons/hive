package automerge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	// currentBaseFallbackMessage is the one-per-PR WARN emitted when a repo
	// gated only by auto_merge.require_current_base_default has a behind head
	// the App cannot update, so the head merges as it was tested.
	currentBaseFallbackMessage = "automerge current-base gate: head is behind the base but the branch cannot be updated; merging as tested (list the repo in auto_merge.require_current_base to park it instead)"
)

func (c *Engine) requireCurrentBaseFor(repo string) bool {
	if c == nil || c.requireCurrentBase == nil {
		return false
	}
	return c.requireCurrentBase(repo)
}

func (c *Engine) requireCurrentBaseStrictFor(repo string) bool {
	if c == nil || c.requireCurrentBaseStrict == nil {
		return false
	}
	return c.requireCurrentBaseStrict(repo)
}

// currentBaseUpdateUnavailable reports whether an update-branch attempt
// failed because the App may not write the head at all — a fork head without
// maintainer edits, or a same-repo 403 (branch protection or a missing
// contents:write) — as opposed to a transient or conflict failure. Only
// these outcomes let a default-gated repo fall back to merging as tested;
// a conflict or an unknown error still stops the merge.
func currentBaseUpdateUnavailable(reason string, err error) bool {
	if reason == autoMergeReasonForkNotEditable {
		return true
	}
	var ghErr *gh.ErrorResponse
	return err != nil && errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusForbidden
}

// warnCurrentBaseFallbackOnce logs, once per repo#pr, that the default gate
// is letting a behind head merge as tested because the App cannot update it.
// Later ticks for the same PR log at Debug so a parked PR does not repeat
// the line every sweep.
func (c *Engine) warnCurrentBaseFallbackOnce(displayRepo string, number int, reason string, err error) {
	if c == nil {
		return
	}
	key := fmt.Sprintf("%s#%d", strings.ToLower(displayRepo), number)
	c.currentBaseFallbackWarnedMu.Lock()
	if c.currentBaseFallbackWarned == nil || len(c.currentBaseFallbackWarned) > 4096 {
		c.currentBaseFallbackWarned = map[string]struct{}{}
	}
	_, seen := c.currentBaseFallbackWarned[key]
	c.currentBaseFallbackWarned[key] = struct{}{}
	c.currentBaseFallbackWarnedMu.Unlock()
	args := []any{"repo", displayRepo, "pr", number, "reason", reason, "error", err, "opt_in_key", "auto_merge.require_current_base"}
	if seen {
		c.debug(currentBaseFallbackMessage, args...)
		return
	}
	c.warn(currentBaseFallbackMessage, args...)
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
func (c *Engine) currentBaseGate(ctx context.Context, displayRepo, owner, repo string, number int, baseBranch, headSHA string, branchUpdateAllowed bool, holdReason string, fork bool) (AutoMergeSweepEvent, string, bool, error) {
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
	event, reason, err := c.updateBranchAndSkip(ctx, displayRepo, owner, repo, number, headSHA, currentBaseUpdateReason, fork)
	if currentBaseUpdateUnavailable(reason, err) && !c.requireCurrentBaseStrictFor(owner+"/"+repo) {
		// Gate inherited from the hive-wide default (#15335) on a head the App
		// may not write: parking it would turn every non-editable fork PR
		// into a stall a human has to clear, so keep the pre-gate behaviour
		// for this PR and say so once. Strict (explicitly listed) repos
		// still park.
		c.warnCurrentBaseFallbackOnce(displayRepo, number, reason, err)
		return AutoMergeSweepEvent{}, "", false, nil
	}
	return event, reason, true, err
}
