package automerge

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

const (
	nativeQueueReason        = prstage.AutoMergeReasonNativeQueued
	nativeQueueReasonRead    = prstage.AutoMergeReasonNativeQueuePrefix + "-read"
	nativeQueueReasonEnqueue = prstage.AutoMergeReasonNativeQueuePrefix + "-enqueue"
	nativeQueueReasonMethod  = prstage.AutoMergeReasonNativeQueueMethod

	// Lanes passed to reconcileNativeQueueGate by callers outside the three
	// authoring lanes.
	nativeQueueLaneQueued   = "queued"
	nativeQueueLaneInactive = "inactive"
)

// inactiveQueueInterval bounds how often a paused repo's open PRs are re-read;
// each read costs GraphQL points per PR.
const inactiveQueueInterval = 10 * time.Minute

type nativeQueueTransport interface {
	NativeMergeQueueAvailable() bool
	ReadNativeMergeQueue(context.Context, string) (hgithub.NativeMergeQueueState, error)
	EnqueueNativePullRequest(context.Context, string, string) (*hgithub.NativeMergeQueueEntry, error)
	DequeueNativePullRequest(context.Context, string) error
	NativeMergeQueueMethod(context.Context, string, string) (string, error)
}

// botSlug normalises a bot login: REST reports "<slug>[bot]" while GraphQL's
// Bot.login omits the suffix.
func botSlug(login string) string {
	return strings.TrimSuffix(strings.TrimSpace(login), "[bot]")
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
		return nativeQueueReasonRead, err
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
		if c.knownNativeQueueMethodRefusal(claim, state.HeadRefOid) {
			return nativeQueueReasonMethod, nil
		}
		method, err := t.NativeMergeQueueMethod(ctx, state.ID, pr.GetBase().GetRef())
		if err != nil {
			// A transient read failure is not a merge-method refusal; it
			// must not park the PR at stage/085.
			return nativeQueueReasonRead, err
		}
		if !strings.EqualFold(method, "MERGE") {
			c.noteNativeQueueMethodRefusal(claim, state.HeadRefOid)
			return nativeQueueReasonMethod, fmt.Errorf("mixed authorship requires a merge commit; queue uses %s", method)
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
			return nativeQueueReasonEnqueue, err
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
	if t == nil || pr == nil || pr.GetNodeID() == "" || reason == "" || strings.HasPrefix(reason, prstage.AutoMergeReasonNativeQueuePrefix) {
		return
	}
	// Routing observations are not eligibility decisions by the owning lane.
	switch reason {
	case "not-app-authored", trustedAuthorReasonAppAuthored, trustedBotLaneSkipReason, contributorLaneReasonAppAuthored:
		return
	}
	author := hgithub.SafeGetLogin(pr.GetUser())
	if _, own := c.sweepLaneForAuthor(author); own && lane != stageLaneSelf && lane != nativeQueueLaneInactive {
		return
	}
	if lane == stageLaneTrusted && reason == "untrusted-author-role" {
		return
	}
	if lane == stageLaneContributor && c.trustedAuthorDecision(author, c.currentTrustedAuthorPolicy().RequireRole).Allowed {
		return
	}
	login := botSlug(c.transport.AppBotLogin())
	if login == "" {
		return
	}
	state, err := t.ReadNativeMergeQueue(ctx, pr.GetNodeID())
	if err == nil && state.IsInMergeQueue && state.MergeQueueEntry != nil && strings.EqualFold(botSlug(state.MergeQueueEntry.Enqueuer.Login), login) {
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
			if !current.IsInMergeQueue || current.MergeQueueEntry == nil || current.MergeQueueEntry.ID != entryID || !strings.EqualFold(botSlug(current.MergeQueueEntry.Enqueuer.Login), login) {
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
		if !c.inactiveQueueDue(display) {
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
				c.reconcileNativeQueueGate(ctx, nativeQueueLaneInactive, owner, repo, pr, "repo-paused-or-disabled")
			}
			if resp == nil || resp.NextPage == 0 {
				break
			}
			opts.Page = resp.NextPage
		}
	}
}

func (c *Engine) inactiveQueueDue(repo string) bool {
	c.inactiveQueueMu.Lock()
	defer c.inactiveQueueMu.Unlock()
	now := c.now()
	if last, ok := c.inactiveQueueLast[repo]; ok && now.Sub(last) < inactiveQueueInterval {
		return false
	}
	if c.inactiveQueueLast == nil {
		c.inactiveQueueLast = map[string]time.Time{}
	}
	c.inactiveQueueLast[repo] = now
	return true
}
