package automerge

import (
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	hgithub "github.com/hivecommons/hive/pkg/github"
)

// Enqueues GitHub does not accept.
//
// tryNativeQueueAfterRouting used to name one enqueue refusal (unresolved
// review conversations, automerge_review_threads.go). Every other one came
// back as native-merge-queue-enqueue with the raw error: the mutation
// journal held it as an uncertain outcome, nothing was remembered, and the
// lane sent the same enqueue again on every pass (one PR, about 950 refused
// mutations in 16 hours, before the conversation wording it hit was named).
// A base whose rules require approving reviews would do that for every PR
// that passed the hive's own gates before a person approved it: a queue
// base reads mergeable_state "blocked" either way (nativeQueueGatedState),
// so nothing stopped the lane before the enqueue.
//
// The rule is refuse-first: the hive asks GitHub once per head, and what
// GitHub answers decides what happens next.
//
//   - GitHub's reviewDecision for the PR rides on the queue read the lane
//     already makes. It never holds a head on its own: an App the base's
//     rules let past the review is enqueued although the decision reads
//     REVIEW_REQUIRED, and what the decision says on a base that requires no
//     review is not something to bet a merge on. A head whose enqueue
//     GitHub accepts behaves as before, and nothing is remembered for it.
//   - A refusal on one of GitHub's rules for the PR
//     (hgithub.IsGraphQLUnprocessable) while reviewDecision reads
//     REVIEW_REQUIRED or CHANGES_REQUESTED is the review refusal, whatever
//     its wording: GitHub's refusal and GitHub's own review state name the
//     reason between them. So is a refusal in the direct merge's wording
//     (nativeQueueEnqueueRefusedReview) when reviewDecision shows nothing.
//     Either is effects.NotApplied and reports
//     merge-refused:review-required, the direct merge path's reason for the
//     same rule: stage 085, a person is asked for the approval. A refusal
//     of another kind (FORBIDDEN, NOT_FOUND: the App may not enqueue, or
//     the PR is gone) is never read as a missing review, whatever the
//     decision says: it backs off under its own text.
//   - The review refusal is remembered per head in the mark the direct
//     merge path uses (rememberReviewRefusal, reviewRefusalKnown), with the
//     reviewDecision it was refused under. While the mark stands the head
//     is not sent again. It re-arms on a new head, when reviewDecision
//     changes (the approval), when mergeable_state changes, and after
//     mergeRefusalRecheck, the direct path's window, so the 085 note's
//     "within 30 minutes" holds on both paths. The cost is one refused
//     mutation per head, plus that recheck, not one per pass.
//   - Any other failed enqueue backs off on an unchanged head
//     (nativeQueueEnqueueBackoff): the wait doubles from
//     nativeQueueEnqueueBackoffBase, and the reason carries GitHub's error
//     text. A new head, a change of mergeable_state or reviewDecision, or
//     the PR entering the queue starts over. When the error proves GitHub
//     turned the enqueue down (hgithub.IsGraphQLRefusal) it is NotApplied
//     too, is a wait, not an error, to the lane, and may wait up to
//     nativeQueueEnqueueBackoffMax. Anything else (a network error, a 5xx,
//     a rate limit) stays an uncertain outcome and an error on the pass
//     that made the call, and waits nativeQueueEnqueueUncertainMax at most.
//
// Naming the review refusal needs GitHub to report a review decision for
// the PR. Where it reports none and the wording is not the direct merge's,
// the PR waits under the backoff with GitHub's text.
//
// A PR already in the queue is never held by any of this: the marks only
// decide whether to send an enqueue.

const (
	// nativeQueueEnqueueBackoffBase is the wait after the first failed
	// enqueue of a head; each further failure in a row doubles it.
	nativeQueueEnqueueBackoffBase = 2 * time.Minute
	// nativeQueueEnqueueBackoffMax caps the wait after an enqueue GitHub
	// definitely refused: once the wait has grown to it, a refusal nobody
	// has named costs one mutation an hour for an unchanged head, where it
	// cost one per pass.
	nativeQueueEnqueueBackoffMax = time.Hour
	// nativeQueueEnqueueUncertainMax caps the wait after a failure that
	// may or may not have queued the PR (a network error, a 5xx, a rate
	// limit). Those usually pass, and the journal holds the operation as
	// unresolved until the next try reconciles it, so the cap stays well
	// under mutation.DefaultStuckAfter (30 minutes): a waiting retry must
	// not show up as a stuck operation.
	nativeQueueEnqueueUncertainMax = 15 * time.Minute
	// nativeQueueEnqueueDetailMax bounds the error text a backoff reason
	// carries.
	nativeQueueEnqueueDetailMax = 200
)

// nativeQueueEnqueueKeyPrefix starts the failed-enqueue key; the prune
// keeps such marks longer (mergeRefusalPrunable).
const nativeQueueEnqueueKeyPrefix = "queue-enqueue|"

func nativeQueueEnqueueKey(a mergeAttempt) string {
	return nativeQueueEnqueueKeyPrefix + strings.ToLower(a.owner+"/"+a.repo) + "#" + strconv.Itoa(a.number)
}

// nativeQueueMergeAttempt is the enqueue of pr in repo (owner/name) as the
// refusal memory sees it: the head the queue read pinned, the
// mergeable_state the lane read, and GitHub's reviewDecision.
func nativeQueueMergeAttempt(repo string, pr *gh.PullRequest, state hgithub.NativeMergeQueueState) mergeAttempt {
	owner, name, _ := strings.Cut(repo, "/")
	return mergeAttempt{owner: owner, repo: name, base: pr.GetBase().GetRef(), number: pr.GetNumber(),
		head: state.HeadRefOid, state: pr.GetMergeableState(), review: strings.TrimSpace(state.ReviewDecision)}
}

// nativeQueueReviewOutstanding reports whether GitHub's reviewDecision says
// the PR still lacks a review its base requires. An empty decision (the
// base requires no review) and APPROVED do not. It names a refusal GitHub
// already gave; it is never a reason not to ask.
func nativeQueueReviewOutstanding(decision string) bool {
	switch strings.ToUpper(strings.TrimSpace(decision)) {
	case "REVIEW_REQUIRED", "CHANGES_REQUESTED":
		return true
	}
	return false
}

// nativeQueueEnqueueRefusedReview reports whether err is GitHub refusing an
// enqueue for a missing approving review or a standing change request. It
// is a definite refusal: nothing was queued.
//
// The GraphQL enqueuePullRequest wording for this refusal has not been
// observed live. This reuses mergeRefusedReviewRE, the REST merge wording,
// on the guess that GitHub words the rule the same way on both paths, as it
// does for conversation resolution. Nothing depends on the guess: a rule
// refusal while reviewDecision says a review is outstanding is named by
// that decision, whatever its wording, and any other one backs off. The
// wording only names the refusal when reviewDecision shows nothing.
//
// The error's own text carries every form the refusal can arrive in: a
// GraphQL errors array renders each entry's message, a REST error response
// renders its message, and an error flattened to a string is that text.
func nativeQueueEnqueueRefusedReview(err error) bool {
	return err != nil && mergeRefusedReviewRE.MatchString(err.Error())
}

// nativeQueueEnqueueNotApplied reports whether err proves GitHub queued
// nothing, so the journal must not hold the enqueue as an uncertain outcome:
// the unresolved-conversations refusal, the review wording, or a GraphQL
// error GitHub typed as a refusal. It is the one place that decides it, for
// the effect that journals the enqueue and for the lane that reads the
// answer afterwards.
func nativeQueueEnqueueNotApplied(err error) bool {
	return err != nil && (hgithub.IsUnresolvedConversationsError(err) || nativeQueueEnqueueRefusedReview(err) || hgithub.IsGraphQLRefusal(err))
}

// nativeQueueEnqueueBackoff is the wait after the n-th failed enqueue in a
// row on one head. refused says GitHub definitely turned that enqueue down,
// which waits up to nativeQueueEnqueueBackoffMax; a failure with an
// uncertain outcome waits up to nativeQueueEnqueueUncertainMax.
func nativeQueueEnqueueBackoff(n int, refused bool) time.Duration {
	limit := nativeQueueEnqueueUncertainMax
	if refused {
		limit = nativeQueueEnqueueBackoffMax
	}
	wait := nativeQueueEnqueueBackoffBase
	for i := 1; i < n && wait < limit; i++ {
		wait *= 2
	}
	if wait > limit {
		wait = limit
	}
	return wait
}

// nativeQueueEnqueueDetail is err's text as a backoff reason carries it:
// one line and bounded, with no backtick, so it stays inside the code span
// the stage notes print a reason in, and no at-sign, so GitHub's text can
// never mention anyone where a reason is printed bare.
func nativeQueueEnqueueDetail(err error) string {
	text := strings.Join(strings.Fields(strings.NewReplacer("`", "'", "@", "(at)").Replace(err.Error())), " ")
	if r := []rune(text); len(r) > nativeQueueEnqueueDetailMax {
		text = string(r[:nativeQueueEnqueueDetailMax]) + "..."
	}
	return text
}

// nativeQueueEnqueueBackoffReason is the lane reason while a failed enqueue
// is waited out for wait. It starts with nativeQueueReasonEnqueue, so it
// keeps that reason's stage (100) and never withdraws a queue entry, and it
// is the same on every pass of one wait. GitHub's text sits inside it, and
// the reason ends in the hive's own words: the matchers that read a
// reason's tail (cistatus.ReasonIsPending and the like) never see foreign
// text there.
func nativeQueueEnqueueBackoffReason(wait time.Duration, detail string) string {
	return nativeQueueReasonEnqueue + ": last try failed (" + detail + "), next try " + strconv.Itoa(int(wait.Minutes())) + "m after it"
}

// knownNativeQueueEnqueueRefusal returns the reason to skip the enqueue of
// a while an earlier answer from GitHub still stands, or "" when the lane
// may try: merge-refused:review-required for a remembered review refusal
// (reviewRefusalKnown), or the backoff reason while a failed enqueue waits
// out its delay. A head GitHub has not refused is never held here, whatever
// its reviewDecision. A backoff mark whose wait has passed stays, so a try
// that fails again waits longer; one whose head, mergeable_state or
// reviewDecision changed is dropped.
func (c *Engine) knownNativeQueueEnqueueRefusal(a mergeAttempt) string {
	if c == nil {
		return ""
	}
	now := c.clock()
	m := c.memory()
	m.mergeRefusalsMu.Lock()
	defer m.mergeRefusalsMu.Unlock()
	if c.reviewRefusalKnown(a, now) {
		return autoMergeReasonMergeRefusedReview
	}
	key := nativeQueueEnqueueKey(a)
	r, ok := m.mergeRefusals[key]
	if !ok {
		return ""
	}
	if r.rearmedBy(a) {
		delete(m.mergeRefusals, key)
		return ""
	}
	wait := nativeQueueEnqueueBackoff(r.retries, r.refused)
	if now.Sub(r.at) >= wait {
		return ""
	}
	return nativeQueueEnqueueBackoffReason(wait, r.detail)
}

// noteNativeQueueEnqueueFailure remembers that the enqueue of a failed with
// err, a failure no other rule names, and returns the reason for the wait
// it starts. refused says GitHub definitely turned the enqueue down. The
// step is logged here, once: the passes that wait it out send nothing and
// log only their skip.
func (c *Engine) noteNativeQueueEnqueueFailure(a mergeAttempt, err error, refused bool) string {
	key := nativeQueueEnqueueKey(a)
	n := 1
	m := c.memory()
	m.mergeRefusalsMu.Lock()
	if r, ok := m.mergeRefusals[key]; ok && !r.rearmedBy(a) {
		n = r.retries + 1
	}
	m.mergeRefusalsMu.Unlock()
	detail, wait := nativeQueueEnqueueDetail(err), nativeQueueEnqueueBackoff(n, refused)
	c.rememberMergeRefusal(key, mergeRefusal{head: strings.TrimSpace(a.head), state: comparableMergeableState(a.state), review: strings.TrimSpace(a.review), retries: n, detail: detail, refused: refused})
	args := []any{"repo", a.owner + "/" + a.repo, "pr", a.number, "head", strings.TrimSpace(a.head), "failed_tries", n, "next_try_after", wait.String(), "error", detail}
	if refused {
		// The lane gets a wait, not an error, so this is the one line
		// that says GitHub refused something the hive has no name for.
		c.warn("automerge native queue enqueue refused, backing off", args...)
	} else {
		c.info("automerge native queue enqueue failed, backing off", args...)
	}
	return nativeQueueEnqueueBackoffReason(wait, detail)
}

// noteNativeQueueReviewRefusal remembers that GitHub refused the enqueue of
// a for a missing review (rememberReviewRefusal), with the reviewDecision
// it was refused under, and logs GitHub's own words: once per mark, since
// the passes that find the mark send nothing. The words are kept in the
// log only. The stage reason has to be the bare
// merge-refused:review-required the classifier and the 085 note match.
func (c *Engine) noteNativeQueueReviewRefusal(a mergeAttempt, err error) {
	c.rememberReviewRefusal(a)
	c.info("automerge native queue enqueue refused for a missing review", "repo", a.owner+"/"+a.repo, "pr", a.number, "head", strings.TrimSpace(a.head),
		"review_decision", a.review, "next_try_after", mergeRefusalRecheck.String(), "error", nativeQueueEnqueueDetail(err))
}

// forgetNativeQueueEnqueueFailure drops the backoff of a: GitHub accepted
// the enqueue, the PR is in the queue, or GitHub answered with a refusal
// that has its own memory.
func (c *Engine) forgetNativeQueueEnqueueFailure(a mergeAttempt) {
	m := c.memory()
	m.mergeRefusalsMu.Lock()
	defer m.mergeRefusalsMu.Unlock()
	delete(m.mergeRefusals, nativeQueueEnqueueKey(a))
}
