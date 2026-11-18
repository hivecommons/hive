package automerge

import (
	"context"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// relayNativeQueueLane is the lane recorded on a relayed request's enqueue
// claim.
const relayNativeQueueLane = "relay"

// RelayNativeQueueMerge is the merge-request relay's native-queue path
// (hgithub.NativeQueueMergeFunc). The relay merged with a direct
// PUT /pulls/{n}/merge even on a repo opted into
// auto_merge.native_merge_queue, and GitHub refused it with 405 "Changes
// must be made through the merge queue" (projectbluefin/utah-packages#1199
// and #1201, 2026-11-18). It runs after every relay gate passed, and
// enqueues through tryNativeQueueAfterRouting, the lanes' own path: the
// queue probe, the head pin (expectedHeadOid), the mixed-authorship merge
// method check, the journaled enqueue and the rejected-head memory.
//
// OptedIn is false when the repo or the PR's base is outside the policy,
// or the transport cannot enqueue (no App auth); the relay then merges
// directly as before. With OptedIn true, Queued means the queue holds the
// PR; an empty Reason without Queued means the base has no queue (merge
// directly); any other Reason is a failed attempt. Integration routing is
// already settled by RelayMergeRefusal, so no execution authority is
// consulted here.
func (c *Engine) RelayNativeQueueMerge(ctx context.Context, displayRepo string, pr *gh.PullRequest, expectSHA string) (hgithub.NativeQueueMergeResult, error) {
	if !c.ready() || pr == nil {
		return hgithub.NativeQueueMergeResult{}, nil
	}
	owner, repo := c.transport.SplitRepo(displayRepo)
	fullRepo := owner + "/" + repo
	if c.nativeQueue(fullRepo) == nil || !c.nativeQueuePolicy().baseAllowed(fullRepo, pr.GetBase().GetRef()) {
		return hgithub.NativeQueueMergeResult{}, nil
	}
	head := strings.TrimSpace(expectSHA)
	if head == "" {
		head = pr.GetHead().GetSHA()
	}
	method, methodReason := c.mergeMethodWithReason(ctx, owner, repo, pr)
	claim := effects.Claim{
		Repo:   fullRepo,
		Kind:   effects.KindPullRequestMerge,
		Target: strconv.Itoa(pr.GetNumber()),
		Actor:  "automerge",
		Inputs: mergeClaimInputs(method, methodReason, head, relayNativeQueueLane),
	}
	reason, err := c.tryNativeQueueAfterRouting(ctx, pr, claim, false)
	return hgithub.NativeQueueMergeResult{OptedIn: true, Queued: err == nil && strings.HasPrefix(reason, nativeQueueReason), Reason: reason}, err
}
