package automerge

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

const nativeQueueReason = "native-merge-queued"

type nativeQueueTransport interface {
	NativeMergeQueueAvailable() bool
	ReadNativeMergeQueue(context.Context, string) (hgithub.NativeMergeQueueState, error)
	EnqueueNativePullRequest(context.Context, string, string) (*hgithub.NativeMergeQueueEntry, error)
	DequeueNativePullRequest(context.Context, string) error
}

func (c *Engine) nativeQueue() nativeQueueTransport {
	t, ok := c.transport.(nativeQueueTransport)
	if !ok || !t.NativeMergeQueueAvailable() {
		return nil
	}
	return t
}

// tryNativeQueue runs only after the lane's existing merge gates passed. A
// queued PR is not a merged PR: no merged audit, issue closure or merge event.
func (c *Engine) tryNativeQueue(ctx context.Context, pr *gh.PullRequest, claim effects.Claim) (string, error) {
	t := c.nativeQueue()
	if t == nil || pr.GetNodeID() == "" {
		return "", nil
	}
	state, err := t.ReadNativeMergeQueue(ctx, pr.GetNodeID())
	if err != nil {
		return "native-merge-queue-read", err
	}
	if !state.IsMergeQueueEnabled && !state.IsInMergeQueue {
		return "", nil
	}
	if state.HeadRefOid != claim.Inputs["expect_sha"] {
		return "head-changed-since-eval", nil
	}
	// A queue chooses its own merge method. Preserve the attribution gate
	// that elected a merge commit to retain each author's signed commits.
	if claim.Inputs["method_reason"] == mergeMethodReasonMixedAuthorship {
		mt, ok := c.transport.(interface {
			NativeMergeQueueMethod(context.Context, string, string) (string, error)
		})
		if !ok {
			return "native-merge-queue-method", fmt.Errorf("queue merge method cannot be verified")
		}
		method, err := mt.NativeMergeQueueMethod(ctx, state.ID, pr.GetBase().GetRef())
		if err != nil {
			return "native-merge-queue-method", err
		}
		if !strings.EqualFold(method, "MERGE") {
			return "native-merge-queue-method", fmt.Errorf("mixed authorship requires a merge commit; queue uses %s", method)
		}
	}
	if !state.IsInMergeQueue {
		// Each removal starts a new queue attempt, even at the same head.
		// The previous applied journal entry must never suppress re-enqueue.
		cycle := ""
		if nodes := state.TimelineItems.Nodes; len(nodes) > 0 {
			cycle = nodes[len(nodes)-1].ID
		}
		claim.Inputs = map[string]string{"operation": "enqueue", "expected_head_sha": state.HeadRefOid, "after_removal": cycle}
		observe := func(ctx context.Context) (effects.ExternalState, error) {
			current, err := t.ReadNativeMergeQueue(ctx, pr.GetNodeID())
			if err != nil {
				return effects.ExternalState{}, err
			}
			result := effects.ExternalState{Applied: current.IsInMergeQueue}
			if current.MergeQueueEntry != nil {
				result.Provenance = current.MergeQueueEntry.ID
			}
			return result, nil
		}
		_, err = effects.ExecuteReconciling(ctx, c.mutation, claim, observe, effects.DefaultUnresolvedTTL, func(ctx context.Context) (effects.Result, error) {
			entry, err := t.EnqueueNativePullRequest(ctx, state.ID, state.HeadRefOid)
			if err != nil {
				return effects.Result{}, err
			}
			state.MergeQueueEntry = entry
			return effects.Result{Provenance: entry.ID}, nil
		})
		if err != nil {
			return "native-merge-queue-enqueue", err
		}
	}
	if e := state.MergeQueueEntry; e != nil {
		c.info("automerge queued", "repo", claim.Repo, "pr", pr.GetNumber(), "position", e.Position, "state", e.State)
		return fmt.Sprintf("%s: queued (position %d, %s)", nativeQueueReason, e.Position, e.State), nil
	}
	return nativeQueueReason, nil
}

// reconcileNativeQueueGate runs for the owning lane's rejected candidates,
// including cheap prefilter holds. Never withdraw another actor's entry.
func (c *Engine) reconcileNativeQueueGate(ctx context.Context, lane, owner, repo string, pr *gh.PullRequest, reason string) {
	t := c.nativeQueue()
	if t == nil || pr == nil || pr.GetNodeID() == "" || reason == "" || strings.HasPrefix(reason, nativeQueueReason) {
		return
	}
	// Routing observations are not eligibility decisions by the owning lane.
	switch reason {
	case "not-app-authored", trustedAuthorReasonAppAuthored, trustedBotLaneSkipReason, contributorLaneReasonAppAuthored:
		return
	}
	author := hgithub.SafeGetLogin(pr.GetUser())
	if _, own := c.sweepLaneForAuthor(author); own && lane != stageLaneSelf && lane != "inactive" {
		return
	}
	if lane == stageLaneTrusted && reason == "untrusted-author-role" {
		return
	}
	if lane == stageLaneContributor && c.trustedAuthorDecision(author, c.currentTrustedAuthorPolicy().RequireRole).Allowed {
		return
	}
	login := strings.TrimSpace(c.transport.AppBotLogin())
	if login == "" {
		return
	}
	state, err := t.ReadNativeMergeQueue(ctx, pr.GetNodeID())
	if err == nil && state.IsInMergeQueue && state.MergeQueueEntry != nil && strings.EqualFold(state.MergeQueueEntry.Enqueuer.Login, login) {
		entryID := state.MergeQueueEntry.ID
		claim := effects.Claim{Repo: owner + "/" + repo, Kind: effects.KindPullRequestMerge, Target: strconv.Itoa(pr.GetNumber()), Actor: "automerge", Inputs: map[string]string{"operation": "dequeue", "entry": entryID}}
		observe := func(ctx context.Context) (effects.ExternalState, error) {
			current, err := t.ReadNativeMergeQueue(ctx, pr.GetNodeID())
			if err != nil {
				return effects.ExternalState{}, err
			}
			return effects.ExternalState{Applied: !current.IsInMergeQueue || current.MergeQueueEntry == nil || current.MergeQueueEntry.ID != entryID, Provenance: entryID}, nil
		}
		_, err = effects.ExecuteReconciling(ctx, c.mutation, claim, observe, effects.DefaultUnresolvedTTL, func(ctx context.Context) (effects.Result, error) {
			// Recheck provenance immediately before the write; a person may have
			// removed and re-enqueued the PR since the lane evaluated it.
			current, err := t.ReadNativeMergeQueue(ctx, pr.GetNodeID())
			if err != nil {
				return effects.Result{}, err
			}
			if !current.IsInMergeQueue || current.MergeQueueEntry == nil || current.MergeQueueEntry.ID != entryID || !strings.EqualFold(current.MergeQueueEntry.Enqueuer.Login, login) {
				return effects.Result{}, nil
			}
			if err := t.DequeueNativePullRequest(ctx, state.ID); err != nil {
				return effects.Result{}, err
			}
			return effects.Result{Provenance: entryID}, nil
		})
	}
	if err != nil {
		c.warn("native merge queue gate reconciliation failed", "repo", owner+"/"+repo, "pr", pr.GetNumber(), "reason", reason, "error", err)
	}
}

// Pausing a repo stops new merges, but GitHub may still hold entries from a
// previous pass. Withdraw only this App's entries through the usual boundary.
func (c *Engine) reconcileInactiveNativeQueues(ctx context.Context) {
	if c.nativeQueue() == nil {
		return
	}
	active := map[string]bool{}
	for _, repo := range c.activeRepos() {
		active[repo] = true
	}
	for _, display := range c.transport.Repositories() {
		if (active[display] && c.repoAutoMergeAllowed(display)) || (c.repoOnGitHub != nil && !c.repoOnGitHub(display)) {
			continue
		}
		owner, repo := c.transport.SplitRepo(display)
		opts := &gh.PullRequestListOptions{State: "open", ListOptions: gh.ListOptions{PerPage: 100}}
		for {
			prs, resp, err := c.gh.PullRequests.List(ctx, owner, repo, opts)
			if err != nil {
				c.warn("native merge queue pause reconciliation failed", "repo", display, "error", err)
				break
			}
			for _, pr := range prs {
				c.reconcileNativeQueueGate(ctx, "inactive", owner, repo, pr, "repo-paused-or-disabled")
			}
			if resp == nil || resp.NextPage == 0 {
				break
			}
			opts.Page = resp.NextPage
		}
	}
}
