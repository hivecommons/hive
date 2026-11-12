package dashboard

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/fixqueue"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Per-contributor capacity, in-flight items and new-vs-rework on the fleet
// snapshot (hivecommons/hive#12502).
//
// The operator asked to see, on each row of "Connected contributor agents":
// (a) the relay's parallel capacity, (b) every work item in flight when that
// capacity is above 1, and (c) whether each item is NEW work or REWORK of one
// of the contributor's own red PRs — "so I can see whether the clankers are
// getting the rework our labels claim they should get". Everything here is
// derived from state the hub already holds (connection tasks, the fix
// scheduler behind stage 072, the stage-label store); nothing calls GitHub.

const (
	// FleetWorkKindNew marks an in-flight item that is a fresh issue claim or
	// any other task that is not repair of the contributor's own PR.
	FleetWorkKindNew = "new"
	// FleetWorkKindRework marks an in-flight item that repairs one of the
	// contributor's own PRs: a `fix-pr` task, or a task whose target the stage
	// store has in a repair stage (fixing / needs-author / returned-to-contributor /
	// adopted-by-hive).
	FleetWorkKindRework = "rework"
)

// FleetInFlightItem is one work item a connected contributor is running.
type FleetInFlightItem struct {
	TaskID string `json:"task_id"`
	// Kind is "new" or "rework" (FleetWorkKind*).
	Kind string `json:"kind"`
	// TaskKind is the task_assign kind ("issue", "fix-pr", "pr-review", ...).
	TaskKind string `json:"task_kind,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Number   int    `json:"number,omitempty"`
	Title    string `json:"title,omitempty"`
	URL      string `json:"url,omitempty"`
	// Stage is the item's stage label as the store last observed it (e.g.
	// "stage/072-returned-to-contributor"); empty when unknown.
	Stage string `json:"stage,omitempty"`
	// Mode is the fix-pr dispatch mode (spawn / preempt / boundary); empty
	// for a primary task.
	Mode      string `json:"mode,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
}

// FleetReworkPR is one of the contributor's OPEN PRs waiting in stage 072
// that has not been dispatched to them.
type FleetReworkPR struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title,omitempty"`
	URL    string `json:"url,omitempty"`
	Stage  string `json:"stage,omitempty"`
	// Since is when the PR entered stage 072 (or was queued), RFC3339.
	Since string `json:"since,omitempty"`
	// Cause is the failing check / review ask, when the fix queue knows it.
	Cause string `json:"cause,omitempty"`
	// State is "adopting (contributor offline)" while the connection is
	// stale and the nudge went unanswered (hivecommons/hive#12436). A PR
	// stage 074 already took over is not the contributor's any more and is
	// not listed at all.
	State string `json:"state,omitempty"`
}

const (
	FleetReworkStateAdopting = "adopting (contributor offline)"
	// FleetReworkStateAdopted names the hand-off in logs and tests; such a
	// PR no longer appears on the contributor's card.
	FleetReworkStateAdopted = "adopted by hive"
)

// fleetReworkSeenWithin bounds how long a stage-store record may go
// unobserved and still count as an open PR. The stage lane re-observes every
// open PR each sweep (about a minute); a record it has not touched for this
// long describes a PR that left the open listing (merged or closed) and is
// only waiting for the store's 14-day prune.
const fleetReworkSeenWithin = time.Hour

// Why a queued fix is not on its way to the relay (FleetRework.PendingReworkBlocked).
const (
	FleetReworkBlockedOffline      = "relay offline: no heartbeat within the presence window; the hive adopts the PR if the nudge stays unanswered"
	FleetReworkBlockedNoFixPR      = "relay does not accept fix-pr tasks (no fix_pr_interrupt_v1 capability); the PR only gets a GitHub comment"
	FleetReworkBlockedScope        = "relay's repository scope is not acknowledged (%s); the task boundary serves nothing until it is"
	FleetReworkBlockedOrphan       = "earlier fix dispatch %s is still booked in flight on a dropped connection; released at the relay's next ready"
	FleetReworkBlockedBackoff      = "retry backoff until %s after %d failed dispatch(es)"
	FleetReworkBlockedFocus        = "primary task protected by the focus window until %s; served at the next task boundary"
	FleetReworkBlockedSlotsBusy    = "all %d fix slot(s) busy"
	FleetReworkBlockedAwaitReady   = "waiting for the relay's next ready (fixes are served before new work)"
	FleetReworkBlockedNotQueued    = "not in this hub's fix queue; queued when the stage engine re-nudges"
	fleetReworkBlockedIdleReasonFm = "%s; relay last reported idle: %s"
)

// FleetRework is the per-contributor rework view spliced onto FleetClanker.
type FleetRework struct {
	// ParallelCapacity is the total number of sessions the relay may run
	// (primary + fix sub-sessions) once `parallel_sessions` is advertised and
	// clamped by contribute.max_parallel_fixes; 1 for a relay that understands
	// fix-pr but runs a single session (preempt). 0 = the relay advertised
	// neither, so the hub can only comment on GitHub.
	ParallelCapacity int `json:"parallel_capacity"`
	// FixMode is how a stage-072 fix reaches this relay: "spawn" or "preempt".
	FixMode string `json:"fix_mode,omitempty"`
	// InFlight lists every item this connection is working: the primary task
	// plus any fix-pr tasks running beside / instead of it.
	InFlight []FleetInFlightItem `json:"in_flight,omitempty"`
	// FixQueueDepth is how many open red PRs wait in the contributor's fix queue.
	FixQueueDepth int `json:"fix_queue_depth"`
	// PendingRework counts the contributor's OPEN PRs in
	// stage/072-returned-to-contributor that have NOT been dispatched to them
	// (fix queue ∪ stage store, minus in-flight, minus anything merged,
	// closed, adopted by the hive or no longer observed by the stage lane).
	// Non-zero while the relay is idle is the operator's tell that rework is
	// not reaching the agent.
	PendingRework    int             `json:"pending_rework"`
	PendingReworkPRs []FleetReworkPR `json:"pending_rework_prs,omitempty"`
	// PendingReworkBlocked says why the pending rework is not on its way to
	// the relay right now (FleetReworkBlocked*); empty when nothing pends.
	PendingReworkBlocked string `json:"pending_rework_blocked,omitempty"`
}

// stageStoreOrShared returns the injected stage store or the process-wide one.
func (h *ContributeWSHub) stageStoreOrShared() *stagelabels.Store {
	if h != nil && h.stageStore != nil {
		return h.stageStore
	}
	return stagelabels.Shared()
}

// prOpenState answers whether repo#number is still open from the forge
// cache the hive already holds (never a GitHub call): known=false when the
// cache has no entry. prOpenFn is the test seam.
func (h *ContributeWSHub) prOpenState(repo string, number int) (open, known bool) {
	if h.prOpenFn != nil {
		return h.prOpenFn(repo, number)
	}
	if h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		return false, false
	}
	fs := h.server.deps.GHClient.ForgeState()
	if fs == nil {
		return false, false
	}
	item, ok := fs.PR(repo, number)
	if !ok {
		return false, false
	}
	return item.Open(), true
}

// reworkCandidateOpen reports whether a rework candidate is still an open
// PR of the contributor's: the forge cache decides when it knows the PR;
// otherwise a stage-store record the lane stopped observing means closed.
// A record stage 074 took over is the hive's now, not the contributor's.
func (h *ContributeWSHub) reworkCandidateOpen(repo string, number int, rec stagelabels.Record, hasRec bool, now time.Time) bool {
	if hasRec && rec.Stage == stagelabels.AdoptedByHive {
		return false
	}
	if open, known := h.prOpenState(repo, number); known {
		return open
	}
	if hasRec && !rec.LastSeen.IsZero() && now.Sub(rec.LastSeen) > fleetReworkSeenWithin {
		return false
	}
	return true
}

// isReworkStage reports whether a stage means "this PR is being repaired".
func isReworkStage(st stagelabels.Stage) bool {
	switch st {
	case stagelabels.Fixing, stagelabels.NeedsAuthor, stagelabels.ReturnedToContributor, stagelabels.AdoptedByHive:
		return true
	}
	return false
}

// workKindFor classifies one task: fix-pr is always rework; otherwise the
// stage store decides by the target's current stage.
func workKindFor(store *stagelabels.Store, taskKind, repo string, number int) (kind, stage string) {
	if store != nil && repo != "" && number > 0 {
		if rec, ok := store.Get(stagelabels.Key(repo, number)); ok && rec.Stage != stagelabels.None {
			stage = rec.Stage.Label()
			if isReworkStage(rec.Stage) {
				kind = FleetWorkKindRework
			}
		}
	}
	if taskKind == taskKindFixPR {
		kind = FleetWorkKindRework
		if stage == "" {
			stage = stagelabels.ReturnedToContributor.Label()
		}
	}
	if kind == "" {
		kind = FleetWorkKindNew
	}
	return kind, stage
}

// fleetReworkFor builds the rework view for one connection. Call with c.mu
// held; it takes only the fix scheduler's and stage store's own locks.
// heldFixIDs are the fix-pr task IDs every live connection of this
// contributor holds (FleetSnapshot gathers them before locking c).
func (h *ContributeWSHub) fleetReworkFor(c *ContributorConnection, stale bool, heldFixIDs map[string]bool) FleetRework {
	var out FleetRework
	store := h.stageStoreOrShared()
	now := time.Now()
	cap := h.fixCapacity(c)
	switch {
	case cap.Parallel:
		out.ParallelCapacity = cap.Max
	case relaySupportsFixPR(c):
		// Understands fix-pr but runs one session: capacity 1, preempt.
		out.ParallelCapacity = 1
	}
	out.FixMode = string(cap.Mode())

	inFlightKeys := map[string]bool{}
	if !stale {
		if t := c.currentTask; t != nil {
			kind, stage := workKindFor(store, t.Kind, t.Repo, t.Number)
			item := FleetInFlightItem{TaskID: t.TaskID, Kind: kind, TaskKind: t.Kind, Repo: t.Repo, Number: t.Number, Title: t.Title, URL: t.URL, Stage: stage}
			if !c.taskAssignedAt.IsZero() {
				item.StartedAt = c.taskAssignedAt.UTC().Format(time.RFC3339)
			}
			out.InFlight = append(out.InFlight, item)
			inFlightKeys[fixqueue.Key(t.Repo, t.Number)] = true
		}
		fixIDs := make([]string, 0, len(c.fixTasks))
		for id := range c.fixTasks {
			fixIDs = append(fixIDs, id)
		}
		sort.Strings(fixIDs)
		for _, id := range fixIDs {
			t := c.fixTasks[id]
			if t == nil {
				continue
			}
			started := ""
			if at, ok := c.fixTasksAt[id]; ok && !at.IsZero() {
				started = at.UTC().Format(time.RFC3339)
			}
			prs := t.FixPRs
			if len(prs) == 0 {
				prs = []WSFixPR{{Repo: t.Repo, Number: t.Number, Title: t.Title, URL: t.URL}}
			}
			for _, pr := range prs {
				_, stage := workKindFor(store, taskKindFixPR, pr.Repo, pr.Number)
				out.InFlight = append(out.InFlight, FleetInFlightItem{
					TaskID: t.TaskID, Kind: FleetWorkKindRework, TaskKind: taskKindFixPR,
					Repo: pr.Repo, Number: pr.Number, Title: pr.Title, URL: pr.URL,
					Stage: stage, Mode: t.Mode, StartedAt: started,
				})
				inFlightKeys[fixqueue.Key(pr.Repo, pr.Number)] = true
			}
		}
	}

	user := ""
	if c.profile != nil {
		user = c.profile.GitHubUsername
	}
	if user == "" {
		return out
	}
	sched := h.fixQueue()
	// Dispatched on ANY of this contributor's connections counts as in flight.
	for _, d := range sched.InFlight(user) {
		for _, it := range d.Items {
			inFlightKeys[it.Key()] = true
		}
	}
	pending := map[string]FleetReworkPR{}
	for _, it := range sched.Queued(user) {
		rec, hasRec := store.Get(stagelabels.Key(it.Repo, it.Number))
		if !h.reworkCandidateOpen(it.Repo, it.Number, rec, hasRec, now) {
			// Merged, closed or adopted while queued: nothing left for the
			// relay to fix, and handing it out would waste the agent's turn.
			sched.Remove(user, it.Repo, it.Number)
			h.forgetFixOwner(user, it.Key())
			continue
		}
		out.FixQueueDepth++
		if inFlightKeys[it.Key()] {
			continue
		}
		cause := it.Cause.Check
		if cause == "" {
			cause = it.Summary
		}
		pr := FleetReworkPR{Repo: it.Repo, Number: it.Number, Title: it.Title, URL: it.URL, Stage: stagelabels.ReturnedToContributor.Label(), Cause: cause}
		if !it.OpenedAt.IsZero() {
			pr.Since = it.OpenedAt.UTC().Format(time.RFC3339)
		}
		pending[it.Key()] = pr
	}
	for _, ai := range store.ByAuthor(user, stagelabels.ReturnedToContributor) {
		if inFlightKeys[ai.Key] {
			continue
		}
		if _, dup := pending[ai.Key]; dup {
			continue
		}
		repo, number := splitStageKey(ai.Key)
		if repo == "" || number == 0 {
			continue
		}
		if !h.reworkCandidateOpen(repo, number, ai.Record, true, now) {
			continue
		}
		pr := FleetReworkPR{Repo: repo, Number: number, Stage: ai.Record.Stage.Label()}
		if !ai.Record.EnteredAt.IsZero() {
			pr.Since = ai.Record.EnteredAt.UTC().Format(time.RFC3339)
		}
		if stale && ai.Record.NudgeCount > 0 {
			pr.State = FleetReworkStateAdopting
		}
		pending[ai.Key] = pr
	}
	keys := make([]string, 0, len(pending))
	for k := range pending {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out.PendingReworkPRs = append(out.PendingReworkPRs, pending[k])
	}
	out.PendingRework = len(out.PendingReworkPRs)
	if out.PendingRework > 0 {
		out.PendingReworkBlocked = h.fleetReworkBlockReason(c, stale, user, cap, sched, heldFixIDs, now)
	}
	return out
}

// fleetReworkBlockReason explains why user's pending rework is not on its
// way to the relay, in the order the dispatcher itself refuses. Call with
// c.mu held.
func (h *ContributeWSHub) fleetReworkBlockReason(c *ContributorConnection, stale bool, user string, cap fixqueue.Capacity, sched *fixqueue.Scheduler, heldFixIDs map[string]bool, now time.Time) string {
	reason := func(r string) string {
		if c.currentTask == nil && c.lastIdleReason != "" {
			return fmt.Sprintf(fleetReworkBlockedIdleReasonFm, r, c.lastIdleReason)
		}
		return r
	}
	if stale {
		return FleetReworkBlockedOffline
	}
	if !relaySupportsFixPR(c) {
		return FleetReworkBlockedNoFixPR
	}
	if gate := repoScopeGateReasonLocked(c); gate != "" {
		return reason(fmt.Sprintf(FleetReworkBlockedScope, gate))
	}
	queued := sched.Queued(user)
	if len(queued) == 0 {
		return reason(FleetReworkBlockedNotQueued)
	}
	inflight := sched.InFlight(user)
	for _, d := range inflight {
		if !heldFixIDs[d.ID] {
			return reason(fmt.Sprintf(FleetReworkBlockedOrphan, d.ID))
		}
	}
	if head := queued[0]; now.Before(head.NotBefore) {
		return reason(fmt.Sprintf(FleetReworkBlockedBackoff, head.NotBefore.UTC().Format(time.RFC3339), head.Attempts))
	}
	st := sched.Status(user, cap)
	if c.currentTask != nil {
		if st.FocusUntil != nil && now.Before(*st.FocusUntil) {
			return fmt.Sprintf(FleetReworkBlockedFocus, st.FocusUntil.UTC().Format(time.RFC3339))
		}
		if len(inflight) >= cap.Slots() {
			return fmt.Sprintf(FleetReworkBlockedSlotsBusy, cap.Slots())
		}
	}
	if len(inflight) >= cap.BoundarySlots() {
		return reason(fmt.Sprintf(FleetReworkBlockedSlotsBusy, cap.BoundarySlots()))
	}
	return reason(FleetReworkBlockedAwaitReady)
}

// splitStageKey inverts stagelabels.Key ("owner/repo#N").
func splitStageKey(key string) (repo string, number int) {
	i := strings.LastIndex(key, "#")
	if i < 0 || i == len(key)-1 {
		return "", 0
	}
	for _, r := range key[i+1:] {
		if r < '0' || r > '9' {
			return "", 0
		}
		number = number*10 + int(r-'0')
	}
	return key[:i], number
}
