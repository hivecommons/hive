package automerge

import (
	"strconv"
	"strings"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

// nativeQueueReasonRejected: GitHub's merge queue removed the hive's own
// entry for this head without merging it, and the head is not behind its
// base, so re-enqueueing the same head would buy the same result. It waits
// for a new head (a push or an update-branch), a person, or
// mergeRefusalRecheck.
const nativeQueueReasonRejected = prstage.AutoMergeReasonNativeQueuePrefix + "-rejected"

// mergeQueueBotLogin is the GraphQL login GitHub's merge queue acts as when
// it removes a PR from its queue.
const mergeQueueBotLogin = "github-merge-queue"

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
// re-arms after mergeRefusalRecheck, and on a new head.
func (c *Engine) knownNativeQueueRejection(repo string, number int, head string) bool {
	if c == nil {
		return false
	}
	key := nativeQueueRejectionKey(repo, number)
	now := c.clock()
	m := c.memory()
	m.mergeRefusalsMu.Lock()
	defer m.mergeRefusalsMu.Unlock()
	r, ok := m.mergeRefusals[key]
	if !ok {
		return false
	}
	if now.Sub(r.at) >= mergeRefusalRecheck || !strings.EqualFold(r.head, strings.TrimSpace(head)) {
		delete(m.mergeRefusals, key)
		return false
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
