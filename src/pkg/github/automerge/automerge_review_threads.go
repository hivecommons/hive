package automerge

import (
	"context"
	"strconv"
	"strings"
	"time"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

// Unresolved review conversations.
//
// A base whose branch protection or ruleset requires conversation
// resolution refuses a merge, or a merge-queue enqueue, while any review
// thread on the PR is unresolved ("All comments must be resolved", or "A
// conversation must be resolved before this pull request can be merged.";
// hgithub.IsUnresolvedConversationsError matches each wording). The
// enqueue refusal used to surface as "effect outcome uncertain,
// reconciliation required" and was retried on every sweep, forever, even
// when the only open threads were the hive's own non-blocking notes
// (projectbluefin/gutenprint-printer-app#254, projectbluefin/ps-printer-app#221,
// 2026-11-20).
//
// It is now a named refusal, handled the same way on the native-queue
// enqueue, the direct REST merge (refusedMerge) and the serialized lane
// (mergeThroughLane):
//
//   - every unresolved thread is the hive's own and marked non-blocking:
//     the lane resolves them (hgithub.Client.ResolveOwnReviewThreadsForMerge)
//     and the next pass merges or enqueues the same head
//     (review-threads-resolved, stage 100). A thread the PR's author
//     replied in, and nobody else, counts once the thread is outdated, or
//     the note is from an earlier head and the hive's review approved this
//     one after the reply (ownThreadsEvidence);
//   - a thread only a person can clear is open (another author's thread, a
//     hive thread someone answered, a blocking hive finding): the PR parks at
//     085 with unresolved-review-threads, and the lane does not call GitHub
//     for it again until the head moves or mergeRefusalRecheck (30 minutes)
//     has passed;
//   - neither (the thread read failed, GitHub still counts a thread a person
//     just resolved, or every resolve mutation failed): the PR waits at 100
//     with review-threads-pending and the lane tries again after
//     unresolvedThreadsRetryAfter. After unresolvedThreadsRetryLimit such
//     tries on one head it parks as above, so a refusal nothing explains
//     costs at most a few calls per head before a person is asked. When a
//     park expires the head keeps its count at the limit, so a later
//     unexplained refusal parks again at once: one merge call per 30
//     minutes, no stage flapping.
//
// A hive with review.severity.block_at: P3 (every finding blocks) has no
// non-blocking hive threads, so every such refusal parks at 085: that is
// the opt-out.

const (
	autoMergeReasonUnresolvedReviewThreads = prstage.AutoMergeReasonUnresolvedReviewThreads
	autoMergeReasonReviewThreadsResolved   = prstage.AutoMergeReasonReviewThreadsResolved
	autoMergeReasonReviewThreadsPending    = prstage.AutoMergeReasonReviewThreadsPending
)

const (
	// unresolvedThreadsRetryAfter is how long an unexplained refusal waits
	// before the lane asks GitHub again.
	unresolvedThreadsRetryAfter = 5 * time.Minute
	// unresolvedThreadsRetryLimit is how many unexplained refusals one head
	// gets before it parks for a person.
	unresolvedThreadsRetryLimit = 3
	// unresolvedThreadsParked is the memo state of a parked head; a retry
	// is "retry:<n>".
	unresolvedThreadsParked = "park"
	unresolvedThreadsRetry  = "retry:"
)

// ownReviewThreadResolver is implemented by the GitHub client; a transport
// without it (a test fake, a client without GraphQL) gets the unexplained
// path: retries, then a park.
type ownReviewThreadResolver interface {
	ResolveOwnReviewThreadsForMerge(ctx context.Context, repo string, number int, evidence hgithub.OwnThreadsEvidence) (hgithub.OwnReviewThreadsOutcome, error)
}

// The engine finds the resolver by a type assertion on its transport, so a
// signature that drifts would silently turn every refusal into the
// unexplained path. This keeps the client honest at compile time.
var _ ownReviewThreadResolver = (*hgithub.Client)(nil)

// ownThreadsEvidence is what the engine knows about head of repo
// (owner/name) #number for the client's author-reply rule: the head itself
// and, when the hive's own review of exactly that head is an approving,
// merge-eligible verdict, when its reports came in
// (ContributorLaneVerdict.ApprovedAt). It reads the reviewer verdict as
// recorded, not a lane's reading of it: a person's decision or a
// trusted-bot review skip is not the hive's review of the head. With no
// verdict reader, or no approve, only the head is named and the client
// accepts an outdated thread alone.
func (c *Engine) ownThreadsEvidence(repo string, number int, head string) hgithub.OwnThreadsEvidence {
	ev := hgithub.OwnThreadsEvidence{HeadSHA: strings.TrimSpace(head)}
	if c == nil || c.contributorLaneVerdict == nil || ev.HeadSHA == "" || number <= 0 {
		return ev
	}
	if v := c.contributorLaneVerdict(repo, number, ev.HeadSHA); v.Approved {
		ev.HeadApprovedAt = v.ApprovedAt
	}
	return ev
}

func unresolvedThreadsKey(repo string, number int) string {
	return "threads|" + strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
}

// knownUnresolvedThreads returns the reason to skip the merge call for
// head of repo#number while an earlier unresolved-conversations refusal
// still stands, or "" when the lane may try. A park re-arms on a new head
// and after mergeRefusalRecheck; a pending retry after
// unresolvedThreadsRetryAfter (its count is kept until the head moves).
func (c *Engine) knownUnresolvedThreads(repo string, number int, head string) string {
	if c == nil {
		return ""
	}
	key := unresolvedThreadsKey(repo, number)
	now := c.clock()
	m := c.memory()
	m.mergeRefusalsMu.Lock()
	defer m.mergeRefusalsMu.Unlock()
	r, ok := m.mergeRefusals[key]
	if !ok {
		return ""
	}
	if !strings.EqualFold(r.head, strings.TrimSpace(head)) {
		delete(m.mergeRefusals, key)
		return ""
	}
	if r.state == unresolvedThreadsParked {
		if now.Sub(r.at) >= mergeRefusalRecheck {
			// Try once more, but keep the head's unexplained count at the
			// limit: another refusal nothing explains parks again at once
			// instead of starting three fresh retries (and flipping the
			// stage 085 → 100 → 085). A new head still starts over.
			r.state = unresolvedThreadsRetry + strconv.Itoa(unresolvedThreadsRetryLimit)
			m.mergeRefusals[key] = r
			return ""
		}
		return autoMergeReasonUnresolvedReviewThreads
	}
	if now.Sub(r.at) >= unresolvedThreadsRetryAfter {
		return ""
	}
	return autoMergeReasonReviewThreadsPending
}

// unresolvedThreadsRetries is how many unexplained refusals head already
// had.
func (c *Engine) unresolvedThreadsRetries(repo string, number int, head string) int {
	m := c.memory()
	m.mergeRefusalsMu.Lock()
	defer m.mergeRefusalsMu.Unlock()
	r, ok := m.mergeRefusals[unresolvedThreadsKey(repo, number)]
	if !ok || !strings.EqualFold(r.head, strings.TrimSpace(head)) {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimPrefix(r.state, unresolvedThreadsRetry))
	if err != nil || !strings.HasPrefix(r.state, unresolvedThreadsRetry) {
		return 0
	}
	return n
}

func (c *Engine) forgetUnresolvedThreads(repo string, number int) {
	m := c.memory()
	m.mergeRefusalsMu.Lock()
	defer m.mergeRefusalsMu.Unlock()
	delete(m.mergeRefusals, unresolvedThreadsKey(repo, number))
}

// unresolvedThreadsRefusal answers GitHub's "All comments must be resolved"
// for repo (owner/name) #number at head; see the comment at the top of the
// file for the three outcomes.
func (c *Engine) unresolvedThreadsRefusal(ctx context.Context, repo string, number int, head string) string {
	if c == nil {
		return autoMergeReasonUnresolvedReviewThreads
	}
	head = strings.TrimSpace(head)
	needsPerson := false
	if r, ok := c.transport.(ownReviewThreadResolver); ok {
		out, err := r.ResolveOwnReviewThreadsForMerge(ctx, repo, number, c.ownThreadsEvidence(repo, number, head))
		switch {
		case err != nil:
			c.warn("automerge: could not read review threads after an unresolved-conversations refusal", "repo", repo, "pr", number, "error", err)
		case out.Cleared():
			c.forgetUnresolvedThreads(repo, number)
			c.info("automerge resolved the hive's own review threads", "repo", repo, "pr", number, "head", head, "resolved", out.Resolved, "own_author_answered", out.OwnAuthorAnswered)
			return autoMergeReasonReviewThreadsResolved
		default:
			// A repo paused meanwhile resolves nothing however often the
			// lane retries, so a person is asked at once.
			needsPerson = out.NeedsPerson() || out.Skipped == "paused"
			c.info("automerge: unresolved review threads hold the merge", "repo", repo, "pr", number, "head", head,
				"unresolved", out.Unresolved, "others", out.Others, "own_answered", out.OwnAnswered,
				"own_blocking", out.OwnBlocking, "own_author_answered", out.OwnAuthorAnswered,
				"resolved", out.Resolved, "failed", out.Failed, "skipped", out.Skipped)
		}
	}
	if !needsPerson {
		n := c.unresolvedThreadsRetries(repo, number, head) + 1
		if n < unresolvedThreadsRetryLimit {
			c.rememberMergeRefusal(unresolvedThreadsKey(repo, number), mergeRefusal{head: head, state: unresolvedThreadsRetry + strconv.Itoa(n)})
			return autoMergeReasonReviewThreadsPending
		}
	}
	c.rememberMergeRefusal(unresolvedThreadsKey(repo, number), mergeRefusal{head: head, state: unresolvedThreadsParked})
	return autoMergeReasonUnresolvedReviewThreads
}
