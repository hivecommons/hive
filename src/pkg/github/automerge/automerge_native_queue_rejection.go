package automerge

import (
	"context"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

// nativeQueueReasonRejected: GitHub's merge queue removed the hive's own
// entry for this head without merging it, and the head is not behind its
// base, so re-enqueueing the same head would buy the same result. It waits
// for a new head (a push or an update-branch), a person, or
// mergeRefusalRecheck. A failed_checks rejection of a behind head does not
// re-arm on time (nativeQueueRejectedHeadBehind), and the recheck of any
// other failed_checks rejection is spent once per head and base tip
// (nativeQueueReasonRejectedRepeatedly).
const nativeQueueReasonRejected = prstage.AutoMergeReasonNativeQueuePrefix + "-rejected"

// nativeQueueReasonRejectedRepeatedly: the merge queue failed the hive's
// entry for this head with failed_checks more than once on the same base
// tip, so the 30-minute retry is spent. The PR waits at stage 085, where a
// person sees it, and the hive retries the same head only on the slow
// backoff (nativeQueueRepeatedRecheckFirst, then nativeQueueRepeatedRecheck),
// or at once when the head or the base tip changes or a person approves the
// head after the failure (nativeQueueRetryApproved).
const nativeQueueReasonRejectedRepeatedly = prstage.AutoMergeReasonNativeQueueRejectedRepeatedly

// The slow retry of a repeated rejection (see prstage for why these
// numbers): the hive keeps trying on its own instead of waiting for a
// person to remember to queue the PR.
const (
	nativeQueueRepeatedRecheckFirst = prstage.NativeQueueRepeatedRecheckFirst
	nativeQueueRepeatedRecheck      = prstage.NativeQueueRepeatedRecheck
)

// mergeQueueBotLogin is the GraphQL login GitHub's merge queue acts as when
// it removes a PR from its queue.
const mergeQueueBotLogin = "github-merge-queue"

// nativeQueueRemovalFailedChecks is the removal reason GitHub records when a
// required check failed on the merge group.
const nativeQueueRemovalFailedChecks = "failed_checks"

// A queue rejection is remembered per head, the way a queue-method refusal
// is (knownNativeQueueMethodRefusal). projectbluefin/utah-packages#1199,
// 2026-11-18: with skip_current_base_gate on, the queue removed the hive's
// entry because the PR was behind a pipeline change, and the hive queued
// the same behind head again eight minutes later. Each round cost a full
// merge-group CI run, and only a person's update-branch broke the loop.
//
// Three marks share the merge-refusal memory:
//   - queue-own: the hive's App holds (or just made) the queue entry for a
//     head; state is the ID of the last removal event seen before it;
//   - queue-rejected: a later removal by the merge queue itself took that
//     entry out without merging it; state is GitHub's removal reason;
//   - queue-retried: a failed_checks rejection of head on base tip base
//     re-armed on time, so that head and tip have had their 30-minute
//     retry; retries counts the slow retries since.
//
// A removal by the App (reconcileNativeQueueGate withdrawing its own entry)
// or by a person is not a rejection, and an entry another actor queued is
// never marked queue-own.

func nativeQueueOwnEntryKey(repo string, number int) string {
	return "queue-own|" + strings.ToLower(repo) + "#" + strconv.Itoa(number)
}

func nativeQueueRejectionKey(repo string, number int) string {
	return "queue-rejected|" + strings.ToLower(repo) + "#" + strconv.Itoa(number)
}

// nativeQueueRetryKeyPrefix starts the queue-retried key; the prune keeps
// such marks longer (mergeRefusalPrunable).
const nativeQueueRetryKeyPrefix = "queue-retried|"

func nativeQueueRetryKey(repo string, number int) string {
	return nativeQueueRetryKeyPrefix + strings.ToLower(repo) + "#" + strconv.Itoa(number)
}

// latestNativeQueueRemoval returns the PR's latest removal from the queue,
// or nil when it has none.
func latestNativeQueueRemoval(state hgithub.NativeMergeQueueState) *hgithub.NativeMergeQueueRemoval {
	nodes := state.TimelineItems.Nodes
	if len(nodes) == 0 {
		return nil
	}
	return &nodes[len(nodes)-1]
}

// noteNativeQueueOwnEntry remembers that the hive's App queued head, after
// the removal event removal ("" when the PR was never removed).
func (c *Engine) noteNativeQueueOwnEntry(repo string, number int, head, removal string) {
	if c == nil || strings.TrimSpace(head) == "" {
		return
	}
	c.rememberMergeRefusal(nativeQueueOwnEntryKey(repo, number), mergeRefusal{head: strings.TrimSpace(head), state: removal})
}

// knownNativeQueueRejection reports whether the merge queue already
// rejected the hive's entry for head. Like a queue-method refusal it
// re-arms after mergeRefusalRecheck, and on a new head; a failed_checks
// rejection of a head found behind its base re-arms on a new head only,
// and a repeated one on a new head or base tip, or on the slow backoff.
func (c *Engine) knownNativeQueueRejection(repo string, number int, head string) bool {
	_, ok := c.nativeQueueRejection(repo, number, head)
	return ok
}

// nativeQueueRejection returns the remembered rejection of head, dropping
// one that has re-armed.
//
// The 30-minute re-arm of a head that is not behind is the retry for a
// flaky merge-group check, and each (head, base tip) gets it once. When a
// failed_checks rejection whose base tip is known re-arms on time, the
// queue-retried mark records that head and tip; a later failed_checks
// rejection of the same head on the same tip is repeated
// (noteNativeQueueRepeatedRejection, noteNativeQueueRejectionBase) and
// re-arms only on the slow backoff (nativeQueueRepeatedRecheckFirst, then
// nativeQueueRepeatedRecheck), or at once on a new head or base tip.
// Before, a merge-group check that failed for a reason the PR does not
// carry (a broken dependency, a red base) had the hive queue the same head
// on the same base every 30 to 40 minutes, one full merge-group CI run each
// time, while the PR sat at 100 looking healthy.
func (c *Engine) nativeQueueRejection(repo string, number int, head string) (mergeRefusal, bool) {
	if c == nil {
		return mergeRefusal{}, false
	}
	key := nativeQueueRejectionKey(repo, number)
	now := c.clock()
	m := c.memory()
	// A person's approval is read outside the memory's lock: the store
	// takes its own.
	m.mergeRefusalsMu.Lock()
	peek, peeked := m.mergeRefusals[key]
	m.mergeRefusalsMu.Unlock()
	retryNow := peeked && peek.repeated && c.nativeQueueRetryApproved(repo, number, peek.head, peek.at)
	m.mergeRefusalsMu.Lock()
	defer m.mergeRefusalsMu.Unlock()
	r, ok := m.mergeRefusals[key]
	if !ok {
		return mergeRefusal{}, false
	}
	if !strings.EqualFold(r.head, strings.TrimSpace(head)) {
		delete(m.mergeRefusals, key)
		return mergeRefusal{}, false
	}
	if retryNow && (!r.at.Equal(peek.at) || !strings.EqualFold(r.head, peek.head)) {
		// Replaced between the two reads: judge the new mark next pass.
		retryNow = false
	}
	if r.behind || (!retryNow && now.Sub(r.at) < r.recheck()) {
		return r, true
	}
	if retryNow {
		c.info("automerge native queue retry approved by a person", "repo", repo, "pr", number, "head", r.head)
	}
	delete(m.mergeRefusals, key)
	base, retries := r.base, 0
	if r.repeated {
		// A slow retry: the next failure on this head and tip waits longer.
		retries = r.retries + 1
		if base == "" {
			base = r.priorBase
		}
	}
	if strings.EqualFold(r.state, nativeQueueRemovalFailedChecks) && base != "" {
		m.mergeRefusals[nativeQueueRetryKey(repo, number)] = mergeRefusal{head: r.head, base: base, retries: retries, at: now}
	}
	return mergeRefusal{}, false
}

// nativeQueueRetryApproved reports whether a person approved head after
// since, the time its repeated rejection was seen: the dashboard's
// "Retry now" (an Approve on the 085 row) or a `/hive decision approve`
// comment. Both land in the human change request store the engine already
// reads (review.HumanChangeRequestStore), so this costs no GitHub call. A
// plain approving review on GitHub is not recorded there (only one that
// clears a change request the hive recorded), so it does not count. It re-arms a repeated rejection at once:
// the person says the cause outside the PR is fixed.
func (c *Engine) nativeQueueRetryApproved(repo string, number int, head string, since time.Time) bool {
	if c == nil || c.humanChangeRequests == nil || strings.TrimSpace(head) == "" {
		return false
	}
	req, ok := c.humanChangeRequests.For(repo, number, strings.TrimSpace(head))
	return ok && req.IsApproval() && req.RecordedAt.After(since)
}

// recheck is how long a native-queue rejection that is not behind waits
// before the same head is queued again: mergeRefusalRecheck, or the slow
// backoff once it is repeated.
func (r mergeRefusal) recheck() time.Duration {
	switch {
	case !r.repeated:
		return mergeRefusalRecheck
	case r.retries == 0:
		return nativeQueueRepeatedRecheckFirst
	}
	return nativeQueueRepeatedRecheck
}

// noteNativeQueueRepeatedRejection marks r, a failed_checks rejection of
// head just observed, repeated when head already spent its 30-minute retry,
// so a failed slow retry keeps the slow backoff and is back at 085 on the
// next pass, once the compare confirms the tip (laneReason). The base tip of
// that retry is kept as priorBase: the lane's compare confirms it, or finds
// the tip moved and makes this a first rejection on the new tip
// (noteNativeQueueRejectionBase).
func (c *Engine) noteNativeQueueRepeatedRejection(repo string, number int, r mergeRefusal) mergeRefusal {
	if c == nil || !strings.EqualFold(r.state, nativeQueueRemovalFailedChecks) {
		return r
	}
	m := c.memory()
	m.mergeRefusalsMu.Lock()
	defer m.mergeRefusalsMu.Unlock()
	retry, ok := m.mergeRefusals[nativeQueueRetryKey(repo, number)]
	if !ok || retry.base == "" || !strings.EqualFold(retry.head, r.head) {
		return r
	}
	r.repeated, r.priorBase, r.retries = true, retry.base, retry.retries
	return r
}

// noteNativeQueueRejectionBase records tip, the base tip the lane's compare
// read for a rejected head that is not behind. The first tip recorded is
// the one the rejection is judged against: a repeated rejection whose
// retry was on another tip is a first rejection on this one. A repeated
// mark re-arms as soon as the tip moves; any other mark keeps its
// 30-minute recheck.
func (c *Engine) noteNativeQueueRejectionBase(repo string, number int, head, tip string) {
	head, tip = strings.TrimSpace(head), strings.TrimSpace(tip)
	if c == nil || head == "" || tip == "" {
		return
	}
	key := nativeQueueRejectionKey(repo, number)
	m := c.memory()
	m.mergeRefusalsMu.Lock()
	defer m.mergeRefusalsMu.Unlock()
	r, ok := m.mergeRefusals[key]
	if !ok || r.behind || !strings.EqualFold(r.head, head) {
		return
	}
	switch {
	case r.base == "":
		r.base = tip
		if r.repeated && !strings.EqualFold(r.priorBase, tip) {
			// The retry was on another base tip: this is the first
			// failure on this one, with its own 30-minute retry.
			r.repeated, r.retries = false, 0
		}
		if r.repeated {
			c.info("automerge native queue rejected head repeatedly", "repo", repo, "pr", number, "head", head, "base_sha", tip, "removal_reason", r.state, "slow_retries", r.retries, "next_retry_after", r.recheck().String())
		}
		// Written in place: the rejection's clock keeps running.
		m.mergeRefusals[key] = r
	case r.repeated && !strings.EqualFold(r.base, tip):
		// The base moved under a repeated rejection: the next merge group
		// tests something new.
		delete(m.mergeRefusals, key)
	}
}

// nativeQueueRejectedHeadBehind reports whether the merge queue rejected
// pr's head and that head is behind its base. The lanes update such a head
// instead of queueing it again. They cannot wait for mergeable_state
// "behind": a queue base whose rules do not require up-to-date branches
// reports a behind PR as "blocked". projectbluefin/utah-packages#1217,
// 2026-11-19: the merge group's Canary failed because the head was behind
// main's pipeline changes, the lane never updated it, and the hive queued
// the same head again each time the 30-minute rejection mark re-armed.
//
// A head does not stop being behind while it stays the same, so a
// failed_checks rejection of a behind head is kept until the head changes:
// the base gap is a likely cause of the failure, and a person or a
// successful update-branch is what moves it. When the update is refused
// (workflow-permission, fork-not-editable and the like), the lane's reason
// says so. Other removal reasons keep the 30-minute recheck.
//
// For a head that is not behind, the same compare names the base tip
// (noteNativeQueueRejectionBase), so bounding the retry costs no extra
// call: the compare already runs on each pass while a rejection is held.
func (c *Engine) nativeQueueRejectedHeadBehind(ctx context.Context, repo string, pr *gh.PullRequest) bool {
	if c == nil || c.gh == nil || pr == nil {
		return false
	}
	number, head, base := pr.GetNumber(), strings.TrimSpace(pr.GetHead().GetSHA()), strings.TrimSpace(pr.GetBase().GetRef())
	r, ok := c.nativeQueueRejection(repo, number, head)
	if !ok || head == "" || base == "" {
		return false
	}
	if r.behind {
		return true
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return false
	}
	cmp, _, err := c.gh.Repositories.CompareCommits(hgithub.WithRESTCaller(ctx, "hive:automerge_native_queue_rejected"), owner, name, base, head, &gh.ListOptions{PerPage: 1})
	if err != nil {
		return false
	}
	if cmp.GetBehindBy() <= 0 {
		c.noteNativeQueueRejectionBase(repo, number, head, cmp.GetBaseCommit().GetSHA())
		return false
	}
	if strings.EqualFold(r.state, nativeQueueRemovalFailedChecks) {
		// A repeated rejection whose base moved past the head is a behind
		// one now: the lane updates the head, as for any rejected behind
		// head.
		r.behind, r.repeated = true, false
		c.rememberMergeRefusal(nativeQueueRejectionKey(repo, number), r)
	}
	return true
}

// nativeQueueNoteRejectedBaseTip is the compare nativeQueueRejectedHeadBehind
// runs, for a lane that has no update-branch step: the human label-queued
// lane (trySweepQueuedPR). It records the base tip of a held rejection so
// the retry bound holds there too, and does nothing else: a behind head is
// not marked behind (nothing here would update it), and its new tip makes
// the rejection a first one on that tip, or re-arms a repeated one at
// once. It compares only while a rejection is held and the tip still
// matters: until the base is recorded, and while the mark is repeated (to
// see the tip move). A first rejection whose tip is known waits out its
// 30-minute recheck without a call. This lane made no compare before, so
// this is its one REST compare per pass while it holds such a rejection.
func (c *Engine) nativeQueueNoteRejectedBaseTip(ctx context.Context, repo string, pr *gh.PullRequest) {
	if c == nil || c.gh == nil || pr == nil {
		return
	}
	number, head, base := pr.GetNumber(), strings.TrimSpace(pr.GetHead().GetSHA()), strings.TrimSpace(pr.GetBase().GetRef())
	r, ok := c.nativeQueueRejection(repo, number, head)
	if !ok || r.behind || head == "" || base == "" || (r.base != "" && !r.repeated) {
		return
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return
	}
	cmp, _, err := c.gh.Repositories.CompareCommits(hgithub.WithRESTCaller(ctx, "hive:automerge_native_queue_rejected"), owner, name, base, head, &gh.ListOptions{PerPage: 1})
	if err != nil {
		return
	}
	c.noteNativeQueueRejectionBase(repo, number, head, cmp.GetBaseCommit().GetSHA())
}

// nativeQueueHeadRejected reads a fresh queue state for the PR and returns
// the lane reason when the merge queue rejected the hive's own entry for
// state's head (a rejection already remembered, or one observed now), or ""
// when it did not. It also keeps the queue-own mark current while the
// App's entry sits in the queue, so an entry queued before a restart is
// still recognised.
func (c *Engine) nativeQueueHeadRejected(repo string, number int, state hgithub.NativeMergeQueueState) string {
	if c == nil {
		return ""
	}
	head := strings.TrimSpace(state.HeadRefOid)
	if state.IsInMergeQueue {
		// Queued again (by the hive or a person): whatever the queue said
		// before about this head is no longer the current answer.
		if slug := botSlug(c.transport.AppBotLogin()); slug != "" && ownNativeQueueEntry(state.MergeQueueEntry, slug) {
			removal := ""
			if r := latestNativeQueueRemoval(state); r != nil {
				removal = r.ID
			}
			c.noteNativeQueueOwnEntry(repo, number, head, removal)
		}
		return ""
	}
	if r, ok := c.nativeQueueRejection(repo, number, head); ok {
		return r.laneReason(c.clock())
	}
	removal := latestNativeQueueRemoval(state)
	if removal == nil {
		return ""
	}
	m := c.memory()
	m.mergeRefusalsMu.Lock()
	ownKey := nativeQueueOwnEntryKey(repo, number)
	own, ok := m.mergeRefusals[ownKey]
	if !ok || !strings.EqualFold(own.head, head) || own.state == removal.ID {
		// Not the hive's entry for this head, or no removal since it was
		// queued.
		m.mergeRefusalsMu.Unlock()
		return ""
	}
	// The hive's entry for this head has left the queue: consume the mark,
	// so one removal is judged once.
	delete(m.mergeRefusals, ownKey)
	m.mergeRefusalsMu.Unlock()
	if removal.Actor.Typename != "Bot" || !strings.EqualFold(botSlug(removal.Actor.Login), mergeQueueBotLogin) {
		// The App withdrew its own entry, or a person dequeued it.
		return ""
	}
	r := c.noteNativeQueueRepeatedRejection(repo, number, mergeRefusal{head: head, state: removal.Reason})
	c.rememberMergeRefusal(nativeQueueRejectionKey(repo, number), r)
	c.info("automerge native queue rejected head", "repo", repo, "pr", number, "head", head, "removal_reason", removal.Reason, "repeated", r.repeated)
	return r.laneReason(c.clock())
}

// nativeQueueRejectedRepeatedlyCause is the 085 entry comment's cause for
// nativeQueueReasonRejectedRepeatedly. repo (owner/name) links the
// repository's merge-group runs; "" names them without a link. The merge
// group runs on a gh-readonly-queue ref, not on the PR head, so the PR's
// own checks do not list it.
func nativeQueueRejectedRepeatedlyCause(repo string) string {
	runs := "the repository's Actions runs for the `merge_group` event"
	if strings.Contains(repo, "/") && !strings.HasPrefix(repo, "/") && !strings.HasSuffix(repo, "/") {
		runs = "the repository's [merge-group runs](https://github.com/" + repo + "/actions?query=event%3Amerge_group)"
	}
	return "GitHub's merge queue removed this PR with `failed_checks` more than once at the same head and the same base commit, so the hive stopped queueing it every " + strconv.Itoa(int(mergeRefusalRecheck.Minutes())) + " minutes: the same merge keeps failing the same way. Find the failed run in " + runs +
		" and fix its cause; it is often in another repository. You do not need to queue it again by hand: the hive retries this head on its own " + strconv.Itoa(int(nativeQueueRepeatedRecheckFirst.Hours())) +
		" hours after the second failure and then every " + strconv.Itoa(int(nativeQueueRepeatedRecheck.Hours())) +
		" hours, and at once when a new commit is pushed, the base branch changes, or a maintainer asks for it after the failure: **Retry now** on the hive dashboard's row for this PR, or a `/hive decision approve` comment. An approving review on GitHub does not start a retry. A retry that passes merges as usual"
}

// nativeQueueTipUnconfirmedAfter is how long a repeated rejection waits
// for the lane's compare to confirm its base tip before it is reported as
// repeated on the last known tip anyway: a few lane passes, so a compare
// that keeps failing cannot leave the PR at 100 on the slow backoff with no
// person told.
const nativeQueueTipUnconfirmedAfter = 10 * time.Minute

// laneReason is the lane reason at now for a held native-queue rejection.
// A repeated one says so (stage 085) once the lane's compare confirmed the
// base tip: until then the base may have moved under the retry, and a
// false 085 would tell a person the same merge failed again. If the tip is
// still unread after nativeQueueTipUnconfirmedAfter, the last known tip
// (priorBase) stands in. The mark stays repeated meanwhile, so the slow
// backoff applies either way.
func (r mergeRefusal) laneReason(now time.Time) string {
	if r.repeated && (r.base != "" || now.Sub(r.at) >= nativeQueueTipUnconfirmedAfter) {
		return nativeQueueReasonRejectedRepeatedly
	}
	return nativeQueueReasonRejected
}
