package automerge

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// mergeCallConflicting reports whether GitHub says pr cannot merge for
// conflicts: mergeable_state dirty, or, while the state is still unknown,
// mergeable explicitly false. A known state other than dirty wins over the
// bool, as in hgithub.MergeableFromState.
func mergeCallConflicting(pr *gh.PullRequest) bool {
	if prMergeableStateConflicting(pr) {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(pr.GetMergeableState())) {
	case "", "unknown":
		return pr.Mergeable != nil && !pr.GetMergeable()
	}
	return false
}

// laneMergedBranchesContextKey carries the lane branches one sweep pass has
// already merged into, so the max_merges loop never merges two PRs into one
// lane branch from one validation (#10889, Inv. 7).
type laneMergedBranchesContextKey struct{}

func contextWithLaneMergedBranches(ctx context.Context) context.Context {
	return context.WithValue(ctx, laneMergedBranchesContextKey{}, map[string]bool{})
}

func laneMergedBranchesFromContext(ctx context.Context) map[string]bool {
	merged, _ := ctx.Value(laneMergedBranchesContextKey{}).(map[string]bool)
	return merged
}

// serializedLane reports, from configuration alone, whether repo merges
// through the serialized lane (#10889), and the lane gate to consult. A
// transport without the capability (the sweep's own fakes) is direct.
func (c *Engine) serializedLane(repo string) (bool, hgithub.SerializedLaneGate) {
	provider, ok := c.transport.(interface {
		SerializedLane() (func(repo string) string, hgithub.SerializedLaneGate)
	})
	if !ok {
		return false, nil
	}
	strategy, gate := provider.SerializedLane()
	return hgithub.SerializedStrategy(strategy, repo), gate
}

// rejectPrefilteredBeforeLane keeps cheap sweep rejections cheap for direct
// repos, while letting a serialized front leave immediately. Queued issue list
// entries lack the base branch, so only that path needs an extra PR fetch:
// on a hive-serialized repo every prefiltered queued PR (label-removed,
// exempt-label, sentinel or label block) costs one PullRequests.Get per sweep,
// because the sweep cannot tell from the issue entry whether it is the front.
func (c *Engine) rejectPrefilteredBeforeLane(ctx context.Context, displayRepo, owner, repo string, number int, pr *gh.PullRequest, path, reason string) {
	serialized, gate := c.serializedLane(displayRepo)
	if !serialized {
		return
	}
	if pr == nil {
		var err error
		pr, _, err = c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, number)
		if err != nil {
			c.warn("automerge sweep: could not fetch a prefiltered PR for the serialized lane", "repo", owner+"/"+repo, "pr", number, "reason", reason, "error", err)
			return
		}
	}
	c.rejectBeforeLane(ctx, serialized, gate, owner, repo, number, pr, path, reason)
}

// rejectBeforeLane returns a sweep path's own rejection of pr. In a
// hive-serialized repo the PR is first handed to the lane front-only
// (#11023): when it is the current front, the lane re-evaluates it and lets
// it leave with its own reason (for example a failed required check), so a
// front the path stopped feeding does not hold the lane until the front
// timeout. Any other PR is untouched, and the lane never merges here.
func (c *Engine) rejectBeforeLane(ctx context.Context, serialized bool, gate hgithub.SerializedLaneGate, owner, repo string, number int, pr *gh.PullRequest, path, reason string) (AutoMergeSweepEvent, string, error) {
	if !serialized || integrationEligibilityProbe(ctx) {
		return AutoMergeSweepEvent{}, reason, nil
	}
	res, err := hgithub.RunSerializedLane(ctx, gate, hgithub.LaneMergeRequest{
		Repo:      owner + "/" + repo,
		Branch:    pr.GetBase().GetRef(),
		Number:    number,
		Path:      path,
		FrontOnly: true,
	})
	switch {
	case err != nil:
		c.warn("automerge sweep: serialized lane could not re-evaluate a rejected PR", "repo", owner+"/"+repo, "pr", number, "reason", reason, "error", err)
	case res.Outcome == hgithub.LaneOutcomeLeft:
		c.info("automerge sweep: rejected PR left the serialized lane front", "repo", owner+"/"+repo, "pr", number, "reason", reason, "lane_reason", res.Reason)
	}
	return AutoMergeSweepEvent{}, reason, nil
}

// releaseGoneLaneFronts asks the serialized lane, once per pass and
// hive-serialized repo, to release a front PR that was closed, merged outside
// the lane, made a draft or retargeted. The lanes above list open PRs only, so
// no path calls in for such a front again: without this it held every waiter
// until the front timeout (up to 4x that while its checks were last pending).
// The lane reads one PR per front and never merges here.
func (c *Engine) releaseGoneLaneFronts(ctx context.Context, targets SweepTargets) {
	for _, repo := range c.activeRepos() {
		owner, name := c.transport.SplitRepo(repo)
		fullRepo := owner + "/" + name
		if !targets.selectsRepo(fullRepo) {
			continue
		}
		serialized, gate := c.serializedLane(repo)
		if !serialized || gate == nil {
			continue
		}
		res, err := hgithub.RunSerializedLane(ctx, gate, hgithub.LaneMergeRequest{
			Repo:              fullRepo,
			Path:              hgithub.PRAuditPathSweep,
			ReleaseGoneFronts: true,
		})
		switch {
		case err != nil:
			c.warn("automerge sweep: serialized lane could not check its fronts", "repo", fullRepo, "error", err)
		case res.Outcome == hgithub.LaneOutcomeLeft:
			c.info("automerge sweep: a closed, merged or retargeted PR left the serialized lane front", "repo", fullRepo, "lane_reason", res.Reason)
		}
	}
}

// mergeThroughLane hands a PR that passed every gate of its sweep path to the
// serialized lane. Only the lane's front PR merges, after the lane's final
// re-check and authorize (the path's own authorization for the exact head).
// Any other outcome is a skip with the lane's reason; "deferred: not at the
// front of the lane" is expected, not an error.
func (c *Engine) mergeThroughLane(ctx context.Context, gate hgithub.SerializedLaneGate, owner, repo, branch, path, method string, event AutoMergeSweepEvent, authorize func(context.Context, string) error) (AutoMergeSweepEvent, string, error) {
	fullRepo := owner + "/" + repo
	passKey := fullRepo + "@" + branch
	merged := laneMergedBranchesFromContext(ctx)
	if merged[passKey] {
		return AutoMergeSweepEvent{}, "lane-branch-merged-this-pass", nil
	}
	res, err := hgithub.RunSerializedLane(ctx, gate, hgithub.LaneMergeRequest{
		Repo:      fullRepo,
		Branch:    branch,
		Number:    event.Number,
		Path:      path,
		Method:    method,
		Authorize: authorize,
	})
	if err != nil {
		return AutoMergeSweepEvent{}, res.Reason, err
	}
	switch {
	case res.Merged():
	case res.Outcome == hgithub.LaneOutcomeUpdated && path == hgithub.PRAuditPathSweep:
		return AutoMergeSweepEvent{}, "updated-branch", nil
	default:
		c.info("automerge sweep: serialized lane did not merge PR", "repo", event.Repo, "pr", event.Number, "outcome", res.Outcome, "reason", res.Reason)
		return AutoMergeSweepEvent{}, res.Reason, nil
	}
	if merged != nil {
		merged[passKey] = true
	}
	if res.Method != "" {
		method = res.Method
	}
	c.transport.RecordPRMergedAudit(fullRepo, event.Number, method, res.SHA, path)
	event.MergeSHA = res.SHA
	c.info("automerge sweep merged PR through the serialized lane", "repo", event.Repo, "pr", event.Number, "path", path, "merge_sha", event.MergeSHA)
	return event, "", nil
}

// queueApprovalStillHolds is the lgtm path's authorization for the lane's
// final re-check: the latest Hive queue approval still covers exactly head
// and its queuer is still a trusted merger.
func (c *Engine) queueApprovalStillHolds(ctx context.Context, owner, repo string, number int, head string) error {
	approval, ok, reason, err := c.latestHiveQueueApproval(ctx, owner, repo, number)
	if err != nil {
		return err
	}
	if !ok {
		if reason == "" {
			reason = autoMergeReasonNoHiveQueueApproval
		}
		return fmt.Errorf("no Hive queue approval: %s", reason)
	}
	if approval.HeadSHA != head {
		return fmt.Errorf("the queue approval covers head %s, not %s", approval.HeadSHA, head)
	}
	if trusted, configured := c.isTrustedMerger(approval.QueuedBy); !configured || !trusted {
		c.noteUntrustedMerger(ctx, owner, repo, number, head, approval.QueuedBy)
		return fmt.Errorf("queuer %s is not a trusted merger", approval.QueuedBy)
	}
	return nil
}

// branchUpdateWorkflowRefusalReason is the lane reason for a workflow-file
// refusal of update-branch: a fork head is out of the App's reach whatever it
// is granted, a same-repo head lacked the workflows permission.
func branchUpdateWorkflowRefusalReason(fork bool) string {
	if fork {
		return autoMergeReasonForkWorkflowUpdate
	}
	return autoMergeReasonWorkflowPermission
}

// branchUpdateWorkflowRefusedKey namespaces, in heldRunApprovals, GitHub's
// workflow-file refusal of update-branch for one head on one base, as
// mergeForwardWorkflowRefusedKey does for the merge-forward.
func branchUpdateWorkflowRefusedKey(prKey, headSHA, baseSHA string) string {
	return prKey + "@" + headSHA + "#update-branch-workflow-refused@" + baseSHA
}

func (c *Engine) branchUpdateWorkflowRefused(owner, repo string, number int, headSHA, baseSHA string) bool {
	if strings.TrimSpace(headSHA) == "" {
		return false
	}
	key := branchUpdateWorkflowRefusedKey(c.evaluatedHeadKey(owner, repo, number), headSHA, baseSHA)
	c.heldRunApprovalsMu.Lock()
	defer c.heldRunApprovalsMu.Unlock()
	return c.heldRunApprovals[key]
}

func (c *Engine) markBranchUpdateWorkflowRefused(owner, repo string, number int, headSHA, baseSHA string) {
	if strings.TrimSpace(headSHA) == "" {
		return
	}
	c.markMergeForwardOnce(branchUpdateWorkflowRefusedKey(c.evaluatedHeadKey(owner, repo, number), headSHA, baseSHA))
}

func (c *Engine) commitGreenForQueuedPR(ctx context.Context, owner, repo, branch, sha string, prNumber int, headPushedAt time.Time, expectedChecks *expectedCheckCache) (bool, string, error) {
	return c.commitGreenForPRWith(ctx, owner, repo, branch, sha, prNumber, headPushedAt, expectedChecks, false, nil)
}

func (c *Engine) commitGreenForPRWith(ctx context.Context, owner, repo, branch, sha string, prNumber int, headPushedAt time.Time, expectedChecks *expectedCheckCache, serverEnforceUnknown bool, skippedByDesign map[string]bool) (bool, string, error) {
	st, _, err := c.commitCIForPRSkippingWith(ctx, owner, repo, branch, sha, prNumber, headPushedAt, expectedChecks, serverEnforceUnknown, skippedByDesign)
	return st.Green, st.Reason, err
}

// mergeQueueMemory returns the engine's merge-queue base memory, creating
// a private one for an engine built without Options.MergeQueueBases.
func (c *Engine) mergeQueueMemory() *MergeQueueBases {
	c.mergeQueueBasesMu.Lock()
	defer c.mergeQueueBasesMu.Unlock()
	if c.mergeQueueBases == nil {
		c.mergeQueueBases = NewMergeQueueBases()
	}
	return c.mergeQueueBases
}

// mergeQueueBaseHoldsRefresh reports whether the current-base gate must not
// update a behind head on base because the base refused a direct merge as
// merge-queue-only within mergeQueueBaseRefreshHold.
func (c *Engine) mergeQueueBaseHoldsRefresh(owner, repo, base string) bool {
	if c == nil {
		return false
	}
	return c.mergeQueueMemory().HoldBaseRefresh(owner+"/"+repo, base, c.clock())
}
