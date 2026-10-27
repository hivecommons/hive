package dashboard

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/fixqueue"
	"github.com/hivecommons/hive/pkg/github/automerge"
)

// Stage-072 contributor nudge → relay `fix-pr` task (hivecommons/hive#12436).
//
// The automerge stage engine calls Nudge when a contributor's PR enters
// stage/072-returned-to-contributor. The hub resolves the author to their
// live relay connection(s), decides presence from the relay heartbeat, and —
// through the per-contributor fixqueue.Scheduler — dispatches a `fix-pr`
// task_assign with priority `interrupt` as either a parallel sub-session
// (`spawn`, relay advertised parallel_sessions) or a preemption of the
// primary task (`preempt`). Beyond the contributor's capacity the PR waits in
// their fix queue; queued fixes are handed out BEFORE any new issue at every
// task boundary ("contributors should clean up their mess before taking on
// new stuff").

const (
	// capFixPRInterrupt is the relay→hub capability token that says the relay
	// understands `fix-pr` task_assign messages. Relays without it only get
	// the GitHub comment; the queue still orders their next task.
	capFixPRInterrupt = "fix_pr_interrupt_v1"
	// capParallelSessions is the relay→hub token for "I can run a fix
	// sub-session beside the primary session" (spawn mode).
	capParallelSessions = "parallel_sessions"

	// taskKindFixPR is the task_assign kind for a red-PR fix.
	taskKindFixPR = "fix-pr"
	// taskPriorityInterrupt marks a task_assign that must not wait for the
	// relay's next `ready`.
	taskPriorityInterrupt = "interrupt"
	// taskPriorityQueued marks a fix handed out at a task boundary.
	taskPriorityQueued = "queued"

	// taskUnavailableFixInFlight is the task_unavailable reason when a
	// contributor's queued fixes exist but every slot is busy: new work waits.
	taskUnavailableFixInFlight = "fix_pr_in_flight"

	// fixTaskAckGrace is how long Nudge waits after an interrupt dispatch for
	// the relay to refuse it before reporting the nudge as delivered; relays
	// that cannot take the task fail it within ~100ms (hivecommons/hive#14668).
	fixTaskAckGrace = time.Second
	// fixTaskRetryBackoff keeps a failed fix from being interrupted straight
	// back into a relay that may be mid-turn; a task boundary still serves it.
	fixTaskRetryBackoff = 5 * time.Minute
	// fixTaskMaxAttempts is how many failed dispatches a PR gets before it
	// leaves the fix queue and the stage engine's re-nudge / adoption path
	// takes over.
	fixTaskMaxAttempts = 3
)

// WSFixPR is one red PR inside a fix-pr task_assign.
type WSFixPR struct {
	Repo          string   `json:"repo"`
	Number        int      `json:"number"`
	URL           string   `json:"url,omitempty"`
	HeadSHA       string   `json:"head_sha"`
	Title         string   `json:"title,omitempty"`
	FailingChecks []string `json:"failing_checks,omitempty"`
	ReviewAsks    []string `json:"review_asks,omitempty"`
	Reason        string   `json:"reason,omitempty"`
}

// fixPRInstruction is the prompt shipped with a fix-pr task.
func fixPRInstruction(prs []WSFixPR, mode fixqueue.Mode) string {
	var b strings.Builder
	if len(prs) == 1 {
		fmt.Fprintf(&b, "Your PR %s#%d is off the happy path and the hive has returned it to you.\n\n", prs[0].Repo, prs[0].Number)
	} else {
		fmt.Fprintf(&b, "%d of your PRs are off the happy path for the same cause and the hive has returned them to you.\n\n", len(prs))
	}
	for _, pr := range prs {
		fmt.Fprintf(&b, "- %s#%d (head %s)", pr.Repo, pr.Number, shortSHA(pr.HeadSHA))
		if pr.URL != "" {
			fmt.Fprintf(&b, " %s", pr.URL)
		}
		b.WriteString("\n")
		for _, c := range pr.FailingChecks {
			fmt.Fprintf(&b, "  - failing check: %s\n", c)
		}
		for _, a := range pr.ReviewAsks {
			fmt.Fprintf(&b, "  - review ask: %s\n", a)
		}
	}
	b.WriteString("\nWork on the EXISTING PR branch: check it out, fix the cause, and push a new head to the same branch with the same DCO sign-off. ")
	b.WriteString("Do not open a new PR. When the new head is green the PR returns to the normal review → merge path automatically.\n")
	switch mode {
	case fixqueue.ModeSpawn:
		b.WriteString("\nThis task runs in a parallel fix session; your primary session keeps its current task.\n")
	case fixqueue.ModePreempt:
		b.WriteString("\nThis task preempts your current work: park it (its claim stays alive), fix the PR, then resume.\n")
	}
	return b.String()
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// fixQueue returns the hub's scheduler, creating it lazily so tests that build
// a hub by struct literal still get one.
func (h *ContributeWSHub) fixQueue() *fixqueue.Scheduler {
	h.fixQueueMu.Lock()
	defer h.fixQueueMu.Unlock()
	if h.fixSched == nil {
		h.fixSched = fixqueue.New(fixqueue.Options{MinFocusWindow: h.minFocusWindow()})
		h.fixSched.SetLogger(func(event, user string, d fixqueue.Dispatch, queued int) {
			prs := make([]string, 0, len(d.Items))
			for _, it := range d.Items {
				prs = append(prs, it.Key())
			}
			h.logger.Info("[contribute-ws] fix queue "+event, "username", user, "dispatch", d.ID, "mode", d.Mode, "prs", prs, "queued", queued)
		})
	}
	h.fixSched.SetMinFocusWindow(h.minFocusWindow())
	return h.fixSched
}

func (h *ContributeWSHub) minFocusWindow() time.Duration {
	if h.server != nil && h.server.deps != nil && h.server.deps.Config != nil {
		return h.server.deps.Config.Contribute.EffectiveMinFocusWindow()
	}
	return fixqueue.DefaultMinFocusWindow
}

func (h *ContributeWSHub) presenceWindow() time.Duration {
	if h.server != nil && h.server.deps != nil && h.server.deps.Config != nil {
		return h.server.deps.Config.Governor.Labels.Stage.EffectivePresenceWindow()
	}
	return 15 * time.Minute
}

func (h *ContributeWSHub) maxParallelFixes() int {
	if h.server != nil && h.server.deps != nil && h.server.deps.Config != nil {
		return h.server.deps.Config.Contribute.EffectiveMaxParallelFixes()
	}
	return 2
}

// fixCapacity derives the contributor's fix capacity from what their relay
// advertised, bounded by contribute.max_parallel_fixes. The relay's
// `max_parallel_fixes` counts fix sub-sessions BESIDE its primary session
// (the relay's HIVE_MAX_PARALLEL_FIXES), so total sessions = declared+1; the
// config leaf is the ceiling on that same count. An unadvertised relay reads
// as capacity 1 without parallel sessions (preempt mode).
func (h *ContributeWSHub) fixCapacity(c *ContributorConnection) fixqueue.Capacity {
	cap := fixqueue.Capacity{Max: 1}
	if c == nil || c.capabilities == nil {
		return cap
	}
	if c.capabilities.DeclaresCapability(capParallelSessions) {
		fixes := c.capabilities.MaxParallelFixes
		if fixes < 1 {
			fixes = 1
		}
		if ceiling := h.maxParallelFixes(); fixes > ceiling {
			fixes = ceiling
		}
		if fixes >= 1 {
			cap.Parallel = true
			cap.Max = fixes + 1
		}
	}
	return cap
}

// relaySupportsFixPR reports whether the relay understands fix-pr tasks.
func relaySupportsFixPR(c *ContributorConnection) bool {
	return c != nil && c.capabilities != nil && c.capabilities.DeclaresCapability(capFixPRInterrupt)
}

// liveConnectionsFor returns the author's live connections, freshest pong
// first, and whether any heartbeat is within the presence window.
func (h *ContributeWSHub) liveConnectionsFor(username string, now time.Time) (conns []*ContributorConnection, online bool) {
	window := h.presenceWindow()
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.connections {
		c.mu.Lock()
		match := c.profile != nil && strings.EqualFold(c.profile.GitHubUsername, username)
		pong := c.lastPong
		c.mu.Unlock()
		if !match {
			continue
		}
		conns = append(conns, c)
		if !pong.IsZero() && now.Sub(pong) <= window {
			online = true
		}
	}
	return conns, online
}

// isRelayContributor reports whether username has a contributor profile on
// this hive (so a fix can wait in their queue even while they are offline).
func isRelayContributor(username string) bool {
	p, err := loadContributorProfile(username)
	return err == nil && p != nil && p.GitHubUsername != "" && p.TrustTier != "revoked"
}

var _ automerge.ContributorPresenceSource = (*ContributeWSHub)(nil)

// Presence implements automerge.ContributorPresenceSource: whether username
// has a contributor profile here, whether a relay heartbeat is within the
// presence window now, and the freshest heartbeat seen on any live
// connection (falls back to the profile's last_active for a relay that has
// since disconnected).
func (h *ContributeWSHub) Presence(_ context.Context, username string) automerge.ContributorPresence {
	username = strings.TrimSpace(username)
	if username == "" {
		return automerge.ContributorPresence{}
	}
	now := time.Now()
	conns, online := h.liveConnectionsFor(username, now)
	out := automerge.ContributorPresence{Online: online, Registered: len(conns) > 0}
	for _, c := range conns {
		c.mu.Lock()
		pong := c.lastPong
		c.mu.Unlock()
		if pong.After(out.LastSeen) {
			out.LastSeen = pong
		}
	}
	if p, err := loadContributorProfile(username); err == nil && p != nil && p.GitHubUsername != "" && p.TrustTier != "revoked" {
		out.Registered = true
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(p.LastActive)); err == nil && t.After(out.LastSeen) {
			out.LastSeen = t
		}
	}
	return out
}

// Nudge implements automerge.ContributorNudgeSink.
func (h *ContributeWSHub) Nudge(_ context.Context, n automerge.ContributorNudge) automerge.ContributorNudgeOutcome {
	out := automerge.ContributorNudgeOutcome{Mode: string(fixqueue.ModeComment)}
	author := strings.TrimSpace(n.Author)
	if author == "" {
		return out
	}
	now := time.Now()
	conns, online := h.liveConnectionsFor(author, now)
	out.Online = online
	if len(conns) == 0 && !isRelayContributor(author) {
		// External contributor: GitHub comment only.
		return out
	}
	item := fixqueue.Item{
		Repo: n.Display, Number: n.Number, HeadSHA: n.HeadSHA, Title: n.Title, URL: n.URL,
		OpenedAt: n.OpenedAt, HumanChangesRequested: n.HumanChangesRequested,
		Cause:   fixqueue.Cause{Check: firstOf(n.FailingChecks), Signature: n.Reason},
		Summary: strings.Join(append(append([]string(nil), n.FailingChecks...), n.ReviewAsks...), "; "),
	}
	if item.Repo == "" {
		item.Repo = n.Owner + "/" + n.Repo
	}
	sched := h.fixQueue()
	sched.Enqueue(author, item)
	h.rememberFixOwner(author, item.Key())
	out.Mode = "queued"
	if online {
		failed := h.beginFixWait()
		defer h.endFixWait()
		if msg := h.dispatchFixes(author, conns, false); msg != nil {
			out.Mode = msg.Mode
			if reason, refused := h.awaitFixTaskRefusal(msg.TaskID, failed); refused {
				out.Mode = "queued"
				out.RelayFailed = true
				out.RelayError = reason
			}
		}
	}
	st := sched.Status(author, fixqueue.Capacity{Max: 1})
	out.QueueDepth = st.Queued
	return out
}

// Resolve implements automerge.ContributorNudgeSink: the PR left stage 072.
func (h *ContributeWSHub) Resolve(_ context.Context, owner, repo string, number int) {
	full := owner + "/" + repo
	h.fixQueueMu.Lock()
	sched := h.fixSched
	owners := make(map[string]bool)
	for user := range h.fixOwners {
		if h.fixOwners[user][fixqueue.Key(full, number)] {
			owners[user] = true
		}
	}
	h.fixQueueMu.Unlock()
	if sched == nil {
		return
	}
	for user := range owners {
		freed := sched.Remove(user, full, number)
		h.forgetFixOwner(user, fixqueue.Key(full, number))
		if freed {
			if conns, online := h.liveConnectionsFor(user, time.Now()); online {
				h.dispatchFixes(user, conns, false)
			}
		}
	}
}

func firstOf(list []string) string {
	if len(list) == 0 {
		return ""
	}
	return list[0]
}

// rememberFixOwner records that user's queue holds key so Resolve can find it.
func (h *ContributeWSHub) rememberFixOwner(user, key string) {
	h.fixQueueMu.Lock()
	defer h.fixQueueMu.Unlock()
	if h.fixOwners == nil {
		h.fixOwners = map[string]map[string]bool{}
	}
	u := strings.ToLower(user)
	if h.fixOwners[u] == nil {
		h.fixOwners[u] = map[string]bool{}
	}
	h.fixOwners[u][key] = true
}

func (h *ContributeWSHub) forgetFixOwner(user, key string) {
	h.fixQueueMu.Lock()
	defer h.fixQueueMu.Unlock()
	u := strings.ToLower(user)
	if h.fixOwners[u] != nil {
		delete(h.fixOwners[u], key)
	}
}

// dispatchFixes hands user as many queued fixes as their capacity allows,
// to the freshest fix-pr-capable connection. boundary=true means the relay
// asked for its next task (nothing running); the first dispatched message is
// returned so the caller can reply with it instead of an issue.
func (h *ContributeWSHub) dispatchFixes(user string, conns []*ContributorConnection, boundary bool) *WSMessage {
	var target *ContributorConnection
	var targetPong time.Time
	for _, c := range conns {
		if !relaySupportsFixPR(c) {
			continue
		}
		c.mu.Lock()
		pong := c.lastPong
		c.mu.Unlock()
		if target == nil || pong.After(targetPong) {
			target, targetPong = c, pong
		}
	}
	if target == nil {
		return nil
	}
	sched := h.fixQueue()
	cap := h.fixCapacity(target)
	var first *WSMessage
	for {
		var d *fixqueue.Dispatch
		if boundary {
			d = sched.DispatchAtBoundary(user, cap)
		} else {
			d = sched.Dispatch(user, cap)
		}
		if d == nil {
			break
		}
		msg := h.sendFixTask(target, user, d)
		if first == nil {
			first = &msg
		}
		if boundary {
			// One fix per `ready`: the relay asks again when it is done.
			break
		}
	}
	return first
}

// sendFixTask builds and pushes the fix-pr task_assign for d.
func (h *ContributeWSHub) sendFixTask(c *ContributorConnection, user string, d *fixqueue.Dispatch) WSMessage {
	prs := make([]WSFixPR, 0, len(d.Items))
	for _, it := range d.Items {
		pr := WSFixPR{Repo: it.Repo, Number: it.Number, URL: it.URL, HeadSHA: it.HeadSHA, Title: it.Title, Reason: it.Cause.Signature}
		if it.Cause.Check != "" {
			pr.FailingChecks = []string{it.Cause.Check}
		}
		if it.Summary != "" && it.Cause.Check == "" {
			pr.ReviewAsks = []string{it.Summary}
		}
		prs = append(prs, pr)
		h.rememberFixOwner(user, it.Key())
	}
	priority := taskPriorityInterrupt
	if d.Mode == fixqueue.ModeBoundary {
		priority = taskPriorityQueued
	}
	assign := &WSTaskAssign{
		TaskID: d.ID, Kind: taskKindFixPR, Priority: priority, Mode: string(d.Mode),
		Repo: prs[0].Repo, Number: prs[0].Number, Title: prs[0].Title, URL: prs[0].URL, FixPRs: prs,
	}
	c.mu.Lock()
	if c.fixTasks == nil {
		c.fixTasks = map[string]*WSTaskAssign{}
	}
	c.fixTasks[d.ID] = assign
	if c.fixTasksAt == nil {
		c.fixTasksAt = map[string]time.Time{}
	}
	c.fixTasksAt[d.ID] = time.Now()
	c.mu.Unlock()
	msg := WSMessage{
		Type: "task_assign", Seq: h.nextSeq(), TaskID: d.ID, TaskGen: h.nextTaskGen(),
		Kind: taskKindFixPR, Priority: priority, Mode: string(d.Mode),
		Repo: assign.Repo, Number: assign.Number, Title: assign.Title, URL: assign.URL,
		Forge: repoScopeDefaultForge, Host: h.assignmentHost(),
		FixPRs: prs, Prompt: fixPRInstruction(prs, d.Mode),
	}
	if h.fixDispatched != nil {
		h.fixDispatched(c, msg)
	}
	if c.ws != nil {
		if err := c.send(msg); err != nil {
			h.logger.Warn("[contribute-ws] fix-pr task send failed", "username", user, "task", d.ID, "error", err)
		}
	}
	h.logger.Info("[contribute-ws] fix-pr task dispatched", "username", user, "task", d.ID, "mode", d.Mode, "priority", priority, "prs", len(prs))
	return msg
}

// selectFixTask is the task-boundary hook: queued fixes are served before any
// new issue. nil means "no fix to serve, select an issue".
func (h *ContributeWSHub) selectFixTask(c *ContributorConnection) *WSMessage {
	if c == nil || c.profile == nil || !relaySupportsFixPR(c) {
		return nil
	}
	user := c.profile.GitHubUsername
	sched := h.fixQueue()
	if !sched.Pending(user) {
		return nil
	}
	if msg := h.dispatchFixes(user, []*ContributorConnection{c}, true); msg != nil {
		return msg
	}
	// Fixes are queued but every slot is busy: no new work until one finishes.
	return h.taskUnavailable(taskUnavailableFixInFlight)
}

// completeFixTask settles a fix-pr task the relay reported done/failed. It
// returns false when taskID is not a fix task on this connection.
func (h *ContributeWSHub) completeFixTask(c *ContributorConnection, taskID string) bool {
	c.mu.Lock()
	assign, ok := c.fixTasks[taskID]
	if ok {
		delete(c.fixTasks, taskID)
		delete(c.fixTasksAt, taskID)
	}
	c.mu.Unlock()
	if !ok || c.profile == nil {
		return ok
	}
	user := c.profile.GitHubUsername
	sched := h.fixQueue()
	if sched.CompleteDispatch(user, taskID) {
		for _, pr := range assign.FixPRs {
			h.forgetFixOwner(user, fixqueue.Key(pr.Repo, pr.Number))
		}
		// Refill the freed slot with the next queued fix.
		if conns, online := h.liveConnectionsFor(user, time.Now()); online {
			h.dispatchFixes(user, conns, false)
		}
	}
	return true
}

// failFixTask settles a fix-pr task the relay reported failed: its PRs go
// back in the queue with backoff, and a PR that has failed
// fixTaskMaxAttempts times leaves the queue for the stage engine's re-nudge
// and adoption path. The freed slot is not refilled here — that would hand
// the same fix straight back. It returns false when msg.TaskID is not a fix
// task on this connection.
func (h *ContributeWSHub) failFixTask(c *ContributorConnection, msg WSMessage) bool {
	c.mu.Lock()
	_, ok := c.fixTasks[msg.TaskID]
	if ok {
		delete(c.fixTasks, msg.TaskID)
		delete(c.fixTasksAt, msg.TaskID)
	}
	c.mu.Unlock()
	if !ok {
		return false
	}
	reason := relayFailureReason(msg)
	if c.profile == nil {
		h.logger.Info("[contribute-ws] fix-pr task failed", "username", identityOf(c), "task", msg.TaskID, "reason", reason)
		return true
	}
	user := c.profile.GitHubUsername
	sched := h.fixQueue()
	requeued := sched.Requeue(user, msg.TaskID, fixTaskRetryBackoff)
	if len(requeued) == 0 {
		h.logger.Info("[contribute-ws] fix-pr task failed", "username", user, "task", msg.TaskID, "reason", reason)
	}
	for _, it := range requeued {
		if it.Attempts >= fixTaskMaxAttempts {
			sched.Remove(user, it.Repo, it.Number)
			h.forgetFixOwner(user, it.Key())
			h.logger.Warn("[contribute-ws] fix-pr task failed; giving up on relay delivery", "username", user, "task", msg.TaskID, "pr", it.Key(), "attempts", it.Attempts, "reason", reason)
			continue
		}
		h.logger.Info("[contribute-ws] fix-pr task failed; requeued", "username", user, "task", msg.TaskID, "pr", it.Key(), "attempts", it.Attempts, "retry_after", it.NotBefore, "reason", reason)
	}
	h.pulseFixFailed(msg.TaskID, reason)
	return true
}

// relayFailureReason is the relay's own words for a task_failed.
func relayFailureReason(msg WSMessage) string {
	for _, r := range []string{msg.Message, msg.Reason, msg.FailureKind} {
		if r = strings.TrimSpace(r); r != "" {
			return r
		}
	}
	return ""
}

// beginFixWait registers a Nudge waiting to learn whether its dispatch is
// refused and returns the channel closed at the next fix-pr failure. Call it
// BEFORE dispatching so a failure racing the dispatch is not missed; pair it
// with endFixWait. Failures are only remembered while someone waits.
func (h *ContributeWSHub) beginFixWait() <-chan struct{} {
	h.fixQueueMu.Lock()
	defer h.fixQueueMu.Unlock()
	h.fixWaiters++
	return h.fixFailedSignalLocked()
}

func (h *ContributeWSHub) endFixWait() {
	h.fixQueueMu.Lock()
	defer h.fixQueueMu.Unlock()
	if h.fixWaiters > 0 {
		h.fixWaiters--
	}
	if h.fixWaiters == 0 {
		h.fixFailures = nil
	}
}

func (h *ContributeWSHub) fixFailedSignalLocked() chan struct{} {
	if h.fixFailedSig == nil {
		h.fixFailedSig = make(chan struct{})
	}
	return h.fixFailedSig
}

// pulseFixFailed records why taskID failed (while a Nudge waits) and wakes
// every waiter.
func (h *ContributeWSHub) pulseFixFailed(taskID, reason string) {
	h.fixQueueMu.Lock()
	defer h.fixQueueMu.Unlock()
	if h.fixWaiters > 0 {
		if h.fixFailures == nil {
			h.fixFailures = map[string]string{}
		}
		h.fixFailures[taskID] = reason
	}
	if h.fixFailedSig != nil {
		close(h.fixFailedSig)
		h.fixFailedSig = nil
	}
}

// takeFixFailure reports (and forgets) whether taskID failed, and why.
func (h *ContributeWSHub) takeFixFailure(taskID string) (string, bool) {
	h.fixQueueMu.Lock()
	defer h.fixQueueMu.Unlock()
	reason, ok := h.fixFailures[taskID]
	delete(h.fixFailures, taskID)
	return reason, ok
}

// awaitFixTaskRefusal waits up to the ack grace for the relay to fail the
// fix-pr task it was just handed. refused=true means it did, so the nudge
// was not delivered as a task.
func (h *ContributeWSHub) awaitFixTaskRefusal(taskID string, failed <-chan struct{}) (reason string, refused bool) {
	grace := h.fixAckGrace
	if grace <= 0 {
		grace = fixTaskAckGrace
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	for {
		if r, ok := h.takeFixFailure(taskID); ok {
			return r, true
		}
		select {
		case <-failed:
			h.fixQueueMu.Lock()
			failed = h.fixFailedSignalLocked()
			h.fixQueueMu.Unlock()
		case <-timer.C:
			return h.takeFixFailure(taskID)
		}
	}
}

// FixQueueStatus is the contributor's fix-queue view for the profile API.
func (h *ContributeWSHub) FixQueueStatus(username string) fixqueue.Status {
	conns, _ := h.liveConnectionsFor(username, time.Now())
	cap := fixqueue.Capacity{Max: 1}
	if len(conns) > 0 {
		cap = h.fixCapacity(conns[0])
	}
	return h.fixQueue().Status(username, cap)
}

// FixQueueItems returns the contributor's queued red PRs (dispatch order).
func (h *ContributeWSHub) FixQueueItems(username string) []fixqueue.Item {
	return h.fixQueue().Queued(username)
}

// ContributorFixQueue is the profile-API view of a contributor's fix budget.
type ContributorFixQueue struct {
	fixqueue.Status
	// PRs are the queued (not yet dispatched) red PRs, oldest first.
	PRs []ContributorFixPR `json:"prs,omitempty"`
}

// ContributorFixPR is one queued red PR as shown on the contribute page.
type ContributorFixPR struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	URL    string `json:"url,omitempty"`
	Title  string `json:"title,omitempty"`
	Cause  string `json:"cause,omitempty"`
}

// contributorFixQueue returns nil when the contributor has nothing queued or
// in flight.
func (s *Server) contributorFixQueue(username string) *ContributorFixQueue {
	if s == nil || s.contributeHub == nil {
		return nil
	}
	st := s.contributeHub.FixQueueStatus(username)
	if st.Queued == 0 && st.InFlight == 0 {
		return nil
	}
	out := &ContributorFixQueue{Status: st}
	for _, it := range s.contributeHub.FixQueueItems(username) {
		cause := it.Cause.Check
		if cause == "" {
			cause = it.Summary
		}
		out.PRs = append(out.PRs, ContributorFixPR{Repo: it.Repo, Number: it.Number, URL: it.URL, Title: it.Title, Cause: cause})
	}
	return out
}
