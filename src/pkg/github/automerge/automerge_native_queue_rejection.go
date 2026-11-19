package automerge

import (
	"context"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

// nativeQueueReasonRejected: GitHub's merge queue removed the hive's own
// entry for this head without merging it, and the head is not behind its
// base, so re-enqueueing the same head would buy the same result. It waits
// for a new head (a push or an update-branch), a person, or
// mergeRefusalRecheck. A failed_checks rejection of a behind head does not
// re-arm on time (nativeQueueRejectedHeadBehind).
const nativeQueueReasonRejected = prstage.AutoMergeReasonNativeQueuePrefix + "-rejected"

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
// Two marks share the merge-refusal memory:
//   - queue-own: the hive's App holds (or just made) the queue entry for a
//     head; state is the ID of the last removal event seen before it;
//   - queue-rejected: a later removal by the merge queue itself took that
//     entry out without merging it; state is GitHub's removal reason.
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
// rejection of a head found behind its base re-arms on a new head only.
func (c *Engine) knownNativeQueueRejection(repo string, number int, head string) bool {
	_, ok := c.nativeQueueRejection(repo, number, head)
	return ok
}

// nativeQueueRejection returns the remembered rejection of head, dropping
// one that has re-armed.
func (c *Engine) nativeQueueRejection(repo string, number int, head string) (mergeRefusal, bool) {
	if c == nil {
		return mergeRefusal{}, false
	}
	key := nativeQueueRejectionKey(repo, number)
	now := c.clock()
	m := c.memory()
	m.mergeRefusalsMu.Lock()
	defer m.mergeRefusalsMu.Unlock()
	r, ok := m.mergeRefusals[key]
	if !ok {
		return mergeRefusal{}, false
	}
	if (!r.behind && now.Sub(r.at) >= mergeRefusalRecheck) || !strings.EqualFold(r.head, strings.TrimSpace(head)) {
		delete(m.mergeRefusals, key)
		return mergeRefusal{}, false
	}
	return r, true
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
	if err != nil || cmp.GetBehindBy() <= 0 {
		return false
	}
	if strings.EqualFold(r.state, nativeQueueRemovalFailedChecks) {
		r.behind = true
		c.rememberMergeRefusal(nativeQueueRejectionKey(repo, number), r)
	}
	return true
}

// nativeQueueHeadRejected reads a fresh queue state for the PR and reports
// whether the merge queue rejected the hive's own entry for state's head:
// a rejection already remembered, or one observed now. It also keeps the
// queue-own mark current while the App's entry sits in the queue, so an
// entry queued before a restart is still recognised.
func (c *Engine) nativeQueueHeadRejected(repo string, number int, state hgithub.NativeMergeQueueState) bool {
	if c == nil {
		return false
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
		return false
	}
	if c.knownNativeQueueRejection(repo, number, head) {
		return true
	}
	removal := latestNativeQueueRemoval(state)
	if removal == nil {
		return false
	}
	m := c.memory()
	m.mergeRefusalsMu.Lock()
	ownKey := nativeQueueOwnEntryKey(repo, number)
	own, ok := m.mergeRefusals[ownKey]
	if !ok || !strings.EqualFold(own.head, head) || own.state == removal.ID {
		// Not the hive's entry for this head, or no removal since it was
		// queued.
		m.mergeRefusalsMu.Unlock()
		return false
	}
	// The hive's entry for this head has left the queue: consume the mark,
	// so one removal is judged once.
	delete(m.mergeRefusals, ownKey)
	m.mergeRefusalsMu.Unlock()
	if removal.Actor.Typename != "Bot" || !strings.EqualFold(botSlug(removal.Actor.Login), mergeQueueBotLogin) {
		// The App withdrew its own entry, or a person dequeued it.
		return false
	}
	c.rememberMergeRefusal(nativeQueueRejectionKey(repo, number), mergeRefusal{head: head, state: removal.Reason})
	c.info("automerge native queue rejected head", "repo", repo, "pr", number, "head", head, "removal_reason", removal.Reason)
	return true
}
