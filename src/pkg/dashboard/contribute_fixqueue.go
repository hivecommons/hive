package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/fixqueue"
	"github.com/hivecommons/hive/pkg/forgestate"
	"github.com/hivecommons/hive/pkg/github"
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

	// fixPRStateRESTCaller tags the one PR read a fix dispatch may make
	// when the state cache cannot answer.
	fixPRStateRESTCaller = "hive:contribute_fix_dispatch"
	// fixPRStateTimeout bounds that read.
	fixPRStateTimeout = 10 * time.Second
)

// fixPRState is what the hub knows about a queued PR right before it is
// handed to a relay.
type fixPRState int

const (
	// fixPRStateUnchecked: no GitHub client or no owner/name to read (a
	// hub built without one, as in unit tests); dispatch as before.
	fixPRStateUnchecked fixPRState = iota
	fixPRStateOpen
	// fixPRStateGone: merged, closed or deleted; the fix is dropped.
	fixPRStateGone
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
	// RerouteFrom / RerouteBy / RerouteReason are set when this is another
	// contributor's PR an operator rerouted to this relay (contribute_reroute.go).
	RerouteFrom   string `json:"rerouted_from,omitempty"`
	RerouteBy     string `json:"rerouted_by,omitempty"`
	RerouteReason string `json:"reroute_reason,omitempty"`
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
	if len(prs) > 0 && prs[0].RerouteFrom != "" {
		b.WriteString(fixPRRerouteInstruction(prs[0]))
	} else {
		b.WriteString("\nWork on the EXISTING PR branch: check it out, fix the cause, and push a new head to the same branch with the same DCO sign-off. ")
		b.WriteString("Do not open a new PR. When the new head is green the PR returns to the normal review → merge path automatically.\n")
	}
	switch mode {
	case fixqueue.ModeSpawn:
		b.WriteString("\nThis task runs in a parallel fix session; your primary session keeps its current task.\n")
	case fixqueue.ModePreempt:
		b.WriteString("\nThis task preempts your current work: park it (its claim stays alive), fix the PR, then resume.\n")
	}
	return b.String()
}

// fixPRRerouteInstruction is the work paragraph of a rerouted fix: the PR
// is another contributor's, whose fork branch this relay most likely cannot
// push to, so the fix lands as a replacement PR that carries the original
// commits and sign-off.
func fixPRRerouteInstruction(pr WSFixPR) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nThis PR is NOT yours: it belongs to @%s, whose hive has not acted on it", pr.RerouteFrom)
	if pr.RerouteBy != "" {
		fmt.Fprintf(&b, ", and @%s rerouted the follow-up work to you", pr.RerouteBy)
	}
	b.WriteString(".")
	if pr.RerouteReason != "" {
		fmt.Fprintf(&b, " Return reason: %s.", pr.RerouteReason)
	}
	b.WriteString("\n\nFetch the PR's head and work from it. If you can push to its branch (same-repo head, or maintainer_can_modify), fix the cause there and push a new head with your DCO sign-off added to your commits only. ")
	fmt.Fprintf(&b, "Otherwise open a replacement PR from a branch that carries @%s's commits UNCHANGED (their authorship and Signed-off-by stay), add your fix commits with your own sign-off on top, and put `Supersedes %s#%d` in the body so the hive links and closes the original when yours merges. ", pr.RerouteFrom, pr.Repo, pr.Number)
	b.WriteString("Do not rewrite or squash the original author's commits.\n")
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
			args := []any{"username", user, "dispatch", d.ID, "mode", d.Mode, "prs", prs, "queued", queued}
			if d.Reason != "" {
				args = append(args, "reason", d.Reason)
			}
			h.logger.Info("[contribute-ws] fix queue "+event, args...)
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
		Repo: nudgeFixRepo(n), Number: n.Number, HeadSHA: n.HeadSHA, Title: n.Title, URL: n.URL,
		OpenedAt: n.OpenedAt, HumanChangesRequested: n.HumanChangesRequested,
		Cause:   fixqueue.Cause{Check: firstOf(n.FailingChecks), Signature: n.Reason},
		Summary: strings.Join(append(append([]string(nil), n.FailingChecks...), n.ReviewAsks...), "; "),
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
	if _, held := h.FixWorkFor(author, item.Repo, item.Number); !held {
		// The dispatch check found the PR merged or closed and dropped it.
		out.Mode = string(fixqueue.ModeComment)
	}
	st := sched.Status(author, fixqueue.Capacity{Max: 1})
	out.QueueDepth = st.Queued
	return out
}

// nudgeFixRepo is the owner/name a nudged PR is queued under. Resolve keys
// on owner/name, and the relay records the fix task's repo as the one it
// is authorized for, so the queue never takes the bare configured name the
// nudge's Display carries on a hive whose repos are listed without the org
// (that spelling left a merged PR in the queue: Resolve could not find it).
func nudgeFixRepo(n automerge.ContributorNudge) string {
	owner, repo := strings.TrimSpace(n.Owner), strings.TrimSpace(n.Repo)
	if owner != "" && repo != "" {
		return owner + "/" + repo
	}
	if display := strings.TrimSpace(n.Display); display != "" {
		return display
	}
	return owner + "/" + repo
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
		freed := sched.RemoveFor(user, full, number, fixRemoveLeftStage072)
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
	msg, _ := h.dispatchFixesHeld(user, conns, boundary)
	return msg
}

// dispatchFixesHeld is dispatchFixes that also reports whether dispatch
// stopped because a queued PR's state could not be read (dropClosedFixes).
func (h *ContributeWSHub) dispatchFixesHeld(user string, conns []*ContributorConnection, boundary bool) (*WSMessage, bool) {
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
		return nil, false
	}
	sched := h.fixQueue()
	cap := h.fixCapacity(target)
	var first *WSMessage
	held := false
	for {
		next := sched.Next(user, cap, boundary)
		if len(next) == 0 {
			break
		}
		dropped, hold := h.dropClosedFixes(user, next)
		if hold {
			held = true
			break
		}
		if dropped {
			// The queue changed under the preview: look again.
			continue
		}
		var d *fixqueue.Dispatch
		if boundary {
			d = sched.DispatchAtBoundary(user, cap)
		} else {
			d = sched.Dispatch(user, cap)
		}
		if d == nil {
			break
		}
		// At a boundary the `ready` handler ships the returned message
		// itself; pushing it here as well delivered the same task twice,
		// and the relay ran it both as its primary task and in a parallel
		// fix session.
		msg := h.sendFixTask(target, user, d, !boundary)
		if first == nil {
			first = &msg
		}
		if boundary {
			// One fix per `ready`: the relay asks again when it is done.
			break
		}
	}
	return first, held
}

// dropClosedFixes checks the PRs about to be dispatched to user. A PR that
// merged or closed since it was queued leaves the queue (logged), so no
// relay is sent to rework it. hold is true when a PR's state could not be
// read: no fix is dispatched this tick (the relay gets ordinary work at a
// boundary, see selectFixTask), and the next one asks again.
func (h *ContributeWSHub) dropClosedFixes(user string, items []fixqueue.Item) (dropped, hold bool) {
	sched := h.fixQueue()
	for _, it := range items {
		state, reason, err := h.fixPRStateOf(context.Background(), it.Repo, it.Number)
		if err != nil {
			h.logger.Warn("[contribute-ws] fix queue: could not read the PR's state; holding the dispatch", "username", user, "pr", it.Key(), "error", err)
			return dropped, true
		}
		if state != fixPRStateGone {
			continue
		}
		sched.RemoveFor(user, it.Repo, it.Number, fixRemovePRClosed)
		h.forgetFixOwner(user, it.Key())
		h.logger.Info("[contribute-ws] fix queue: dropped a PR that is no longer open", "username", user, "pr", it.Key(), "state", reason, "head", shortSHA(it.HeadSHA))
		dropped = true
	}
	return dropped, false
}

// fixPRStateOf answers whether repo#number is still open, from the state
// cache when it holds the repo (a cached copy inside its freshness bound
// costs nothing; an older one is re-read once), else with one PR read.
// reason says "merged" or "closed" for a gone PR. A read that fails is an
// error, never "open".
func (h *ContributeWSHub) fixPRStateOf(ctx context.Context, repo string, number int) (fixPRState, string, error) {
	if h.fixPRStateHook != nil {
		return h.fixPRStateHook(ctx, repo, number)
	}
	if h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		return fixPRStateUnchecked, "", nil
	}
	owner, name, ok := strings.Cut(strings.TrimSpace(repo), "/")
	if !ok || owner == "" || name == "" || number <= 0 {
		return fixPRStateUnchecked, "", nil
	}
	client := h.server.deps.GHClient
	ctx, cancel := context.WithTimeout(github.WithRESTCaller(ctx, fixPRStateRESTCaller), fixPRStateTimeout)
	defer cancel()
	if view := client.ForgeStateView(); view != nil {
		if _, held := view.Snapshot(repo); held {
			_, open, err := view.ReadThrough(ctx, repo, forgestate.KindPR, number, 0)
			switch {
			case err == nil && open:
				return fixPRStateOpen, "", nil
			case err == nil:
				// The cache holds open PRs only: absent after a fresh read
				// means merged, closed or deleted.
				return fixPRStateGone, "merged or closed", nil
			case !errors.Is(err, forgestate.ErrStale) && !errors.Is(err, forgestate.ErrNotReady) && !errors.Is(err, forgestate.ErrUnknownRepo):
				return fixPRStateUnchecked, "", err
			}
			// A stale or not-yet-ready cache made no call: read the PR below.
		}
	}
	gh := client.GoGitHub()
	if gh == nil {
		return fixPRStateUnchecked, "", nil
	}
	pr, resp, err := gh.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		if resp != nil && resp.Response != nil && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone) {
			return fixPRStateGone, "not found", nil
		}
		return fixPRStateUnchecked, "", err
	}
	switch {
	case pr.GetMerged() || !pr.GetMergedAt().Time.IsZero():
		return fixPRStateGone, "merged", nil
	case !strings.EqualFold(pr.GetState(), "open"):
		return fixPRStateGone, "closed", nil
	}
	return fixPRStateOpen, "", nil
}

// sendFixTask builds the fix-pr task_assign for d and, when push is true,
// sends it on c (a boundary dispatch is sent by the `ready` handler).
func (h *ContributeWSHub) sendFixTask(c *ContributorConnection, user string, d *fixqueue.Dispatch, push bool) WSMessage {
	prs := make([]WSFixPR, 0, len(d.Items))
	for _, it := range d.Items {
		pr := WSFixPR{Repo: it.Repo, Number: it.Number, URL: it.URL, HeadSHA: it.HeadSHA, Title: it.Title, Reason: it.Cause.Signature,
			RerouteFrom: it.RerouteFrom, RerouteBy: it.RerouteBy, RerouteReason: it.RerouteReason}
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
	// A dispatched fix is work: the relay is no longer idle on an empty queue.
	c.clearIdleReasonLocked()
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
	if push && c.ws != nil {
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
	// A `ready` says nothing is running on this relay. A fix dispatch the
	// scheduler still books for this contributor that no live connection
	// holds (its socket dropped before the relay reported on it) would
	// otherwise occupy the only slot forever: nothing dispatched, new work
	// refused with fix_pr_in_flight. Put it back at the head of the queue.
	h.releaseOrphanedFixes(user, sched)
	if !sched.Pending(user) {
		return nil
	}
	msg, held := h.dispatchFixesHeld(user, []*ContributorConnection{c}, true)
	if msg != nil {
		return msg
	}
	if !sched.Pending(user) {
		// Every queued fix was for a PR that has since merged or closed.
		return nil
	}
	if held {
		// A queued PR's state could not be read this tick (GitHub trouble or
		// a low API budget). The fix stays queued and is tried again on the
		// next ready; meanwhile the relay takes ordinary work rather than
		// sitting idle. The worst case is a fix that starts a little late.
		return nil
	}
	// Fixes are queued but every slot is busy: no new work until one
	// finishes.
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
	if c.profile == nil {
		return ok
	}
	user := c.profile.GitHubUsername
	sched := h.fixQueue()
	var prs []WSFixPR
	if ok {
		prs = assign.FixPRs
	} else {
		// The relay finished a fix it was handed on an earlier socket (the
		// 1006 flap): this connection never saw the task_assign, but the
		// scheduler still books the dispatch under this contributor. Settle
		// it there rather than leaving the slot occupied for good.
		for _, d := range sched.InFlight(user) {
			if d.ID != taskID {
				continue
			}
			ok = true
			for _, it := range d.Items {
				prs = append(prs, WSFixPR{Repo: it.Repo, Number: it.Number})
			}
		}
		if !ok {
			return false
		}
	}
	if sched.CompleteDispatch(user, taskID) {
		for _, pr := range prs {
			h.forgetFixOwner(user, fixqueue.Key(pr.Repo, pr.Number))
		}
		// Refill the freed slot with the next queued fix.
		if conns, online := h.liveConnectionsFor(user, time.Now()); online {
			h.dispatchFixes(user, conns, false)
		}
	}
	return true
}

// heldFixTaskIDs returns the fix-pr task IDs every live connection of user
// currently holds.
func (h *ContributeWSHub) heldFixTaskIDs(user string) map[string]bool {
	held := map[string]bool{}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.connections {
		c.mu.Lock()
		if c.profile != nil && strings.EqualFold(c.profile.GitHubUsername, user) {
			for id := range c.fixTasks {
				held[id] = true
			}
		}
		c.mu.Unlock()
	}
	return held
}

// releaseOrphanedFixes returns to user's queue every dispatch the scheduler
// books in flight that no live connection holds. Called at a task boundary,
// when the relay has just said it is running nothing.
func (h *ContributeWSHub) releaseOrphanedFixes(user string, sched *fixqueue.Scheduler) {
	var held map[string]bool
	for _, d := range sched.InFlight(user) {
		if held == nil {
			held = h.heldFixTaskIDs(user)
		}
		if held[d.ID] {
			continue
		}
		if items := sched.Release(user, d.ID); len(items) > 0 {
			h.logger.Warn("[contribute-ws] fix-pr dispatch orphaned by a dropped connection; requeued", "username", user, "task", d.ID, "prs", len(items))
		}
	}
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
	if relayDidNotTakeFix(msg) {
		// The relay never started the fix (it was polling another hive):
		// nothing about the PR failed, so no attempt is booked toward
		// fixTaskMaxAttempts.
		returned := sched.Return(user, msg.TaskID, fixTaskRetryBackoff)
		for _, it := range returned {
			h.logger.Info("[contribute-ws] fix-pr task not taken by the relay; requeued without counting an attempt", "username", user, "task", msg.TaskID, "pr", it.Key(), "attempts", it.Attempts, "retry_after", it.NotBefore, "reason", reason)
		}
		if len(returned) == 0 {
			h.logger.Info("[contribute-ws] fix-pr task failed", "username", user, "task", msg.TaskID, "reason", reason)
		} else {
			h.retryReturnedFixAfter(user, fixTaskRetryBackoff)
		}
		h.pulseFixFailed(msg.TaskID, reason)
		return true
	}
	requeued := sched.Requeue(user, msg.TaskID, fixTaskRetryBackoff)
	if len(requeued) == 0 {
		h.logger.Info("[contribute-ws] fix-pr task failed", "username", user, "task", msg.TaskID, "reason", reason)
	}
	for _, it := range requeued {
		if it.Attempts >= fixTaskMaxAttempts {
			sched.RemoveFor(user, it.Repo, it.Number, fixRemoveRelayGaveUp)
			h.forgetFixOwner(user, it.Key())
			h.logger.Warn("[contribute-ws] fix-pr task failed; giving up on relay delivery", "username", user, "task", msg.TaskID, "pr", it.Key(), "attempts", it.Attempts, "reason", reason)
			continue
		}
		h.logger.Info("[contribute-ws] fix-pr task failed; requeued", "username", user, "task", msg.TaskID, "pr", it.Key(), "attempts", it.Attempts, "retry_after", it.NotBefore, "reason", reason)
	}
	h.pulseFixFailed(msg.TaskID, reason)
	return true
}

// relayRefusedNotActiveSlot is the task_failed reason a relay sends for an
// assignment from a hive it is not polling (bin/contributor-relay.js, the
// task_assign handler). Relays from before projectbluefin/utah-packages#972
// send it for a fix-pr too.
const relayRefusedNotActiveSlot = "Hub is not the active polling slot"

// relayDidNotTakeFix reports a fix-pr task_failed that says the relay never
// started the task because it was polling another hive, not that the fix
// was tried. On projectbluefin/utah-packages#972 one such refusal booked
// attempts=1, a third of the way to handing the PR to adoption.
func relayDidNotTakeFix(msg WSMessage) bool {
	return strings.EqualFold(strings.TrimSpace(msg.Reason), relayRefusedNotActiveSlot)
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

// FixWorkFor reports where the fix work for repo#number stands in
// username's fix queue: waiting behind other fixes (Queued, with how many
// are ahead) or handed to their relay as a fix-pr task (InFlight, with when
// and in which mode). The stage tracker reads it as the rework-pickup
// signal on a waiting-on row (hivecommons/hive#15280). ok is false when the
// PR is in neither.
func (h *ContributeWSHub) FixWorkFor(username, repo string, number int) (StageTrackerFixWork, bool) {
	username = strings.TrimSpace(username)
	if h == nil || username == "" || repo == "" || number <= 0 {
		return StageTrackerFixWork{}, false
	}
	key := fixqueue.Key(repo, number)
	sched := h.fixQueue()
	for _, d := range sched.InFlight(username) {
		for _, it := range d.Items {
			if it.Key() == key {
				return StageTrackerFixWork{InFlight: true, DispatchedAt: d.At, Mode: string(d.Mode)}, true
			}
		}
	}
	for i, it := range sched.Queued(username) {
		if it.Key() == key {
			return StageTrackerFixWork{Queued: true, QueuedAt: it.EnqueuedAt, Ahead: i}, true
		}
	}
	return StageTrackerFixWork{}, false
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
