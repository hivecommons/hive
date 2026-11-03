package dashboard

import (
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

// FleetReworkPR is one of the contributor's PRs waiting in stage 072 that has
// not been dispatched to them.
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
	// State explains what the hive is doing about a PR the contributor is
	// not working: "adopting (contributor offline)" while the connection is
	// stale and the nudge went unanswered; "adopted by hive" once stage 074
	// took it over (hivecommons/hive#12436).
	State string `json:"state,omitempty"`
}

const (
	FleetReworkStateAdopting = "adopting (contributor offline)"
	FleetReworkStateAdopted  = "adopted by hive"
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
	// FixQueueDepth is how many red PRs wait in the contributor's fix queue.
	FixQueueDepth int `json:"fix_queue_depth"`
	// PendingRework counts the contributor's PRs in
	// stage/072-returned-to-contributor that have NOT been dispatched to them
	// (fix queue ∪ stage store, minus in-flight). Non-zero while the relay is
	// idle is the operator's tell that rework is not reaching the agent.
	PendingRework    int             `json:"pending_rework"`
	PendingReworkPRs []FleetReworkPR `json:"pending_rework_prs,omitempty"`
}

// stageStoreOrShared returns the injected stage store or the process-wide one.
func (h *ContributeWSHub) stageStoreOrShared() *stagelabels.Store {
	if h != nil && h.stageStore != nil {
		return h.stageStore
	}
	return stagelabels.Shared()
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

// openPRIndex is the status snapshot's canonical open-PR set (repoOpenItems:
// actionable, held, drafts and sidelined rows), keyed like the stage store.
// It answers "is this PR closed?" only for a repo whose PR list is complete;
// for any other repo, or with no snapshot yet, a PR counts as open.
type openPRIndex struct {
	repos map[string]bool // lower-cased owner/repo with a complete PR list
	prs   map[string]bool // stagelabels.Key of every listed open PR
}

// openPRIndex builds the index from the hub server's current status. A repo
// whose rows cannot vouch for a departure (ownershipRowsComplete: counts
// incomplete, paused, or never enumerated), or whose counts are stale or carry
// unlisted PRs, is left out: a PR missing from such a listing is not evidence
// that it closed.
func (h *ContributeWSHub) openPRIndex() openPRIndex {
	var idx openPRIndex
	if h == nil || h.server == nil {
		return idx
	}
	h.server.statusMu.RLock()
	status := h.server.status
	h.server.statusMu.RUnlock()
	if status == nil {
		return idx
	}
	idx.repos, idx.prs = map[string]bool{}, map[string]bool{}
	for _, repo := range status.Repos {
		if !ownershipRowsComplete(repo) || repo.CountsStale || repo.UnlistedPRs > 0 {
			continue
		}
		full := nonEmpty(repo.Full, repo.Name)
		idx.repos[strings.ToLower(full)] = true
		for _, it := range repoOpenItems(repo) {
			if it.Kind == "pr" {
				idx.prs[stagelabels.Key(full, it.Number)] = true
			}
		}
	}
	return idx
}

// closed reports whether the snapshot shows repo#number as no longer open:
// the repo's PR list is complete and does not carry it
// (hivecommons/hive#14708).
func (idx openPRIndex) closed(repo string, number int) bool {
	if !idx.repos[strings.ToLower(strings.TrimSpace(repo))] {
		return false
	}
	return !idx.prs[stagelabels.Key(repo, number)]
}

// fleetReworkFor builds the rework view for one connection. Call with c.mu
// held; it takes only the fix scheduler's and stage store's own locks. open
// drops PRs that have merged or closed: stage records outlive a PR whose
// close the stage sweep never observed (hivecommons/hive#14708).
func (h *ContributeWSHub) fleetReworkFor(c *ContributorConnection, stale bool, open openPRIndex) FleetRework {
	var out FleetRework
	store := h.stageStoreOrShared()
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
	queued := sched.Queued(user)
	out.FixQueueDepth = len(queued)
	// Dispatched on ANY of this contributor's connections counts as in flight.
	for _, d := range sched.InFlight(user) {
		for _, it := range d.Items {
			inFlightKeys[it.Key()] = true
		}
	}
	pending := map[string]FleetReworkPR{}
	for _, it := range queued {
		if inFlightKeys[it.Key()] || open.closed(it.Repo, it.Number) {
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
		if repo == "" || number == 0 || open.closed(repo, number) {
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
	for _, ai := range store.ByAuthor(user, stagelabels.AdoptedByHive) {
		if _, dup := pending[ai.Key]; dup {
			continue
		}
		repo, number := splitStageKey(ai.Key)
		if repo == "" || number == 0 || open.closed(repo, number) {
			continue
		}
		pr := FleetReworkPR{Repo: repo, Number: number, Stage: ai.Record.Stage.Label(), State: FleetReworkStateAdopted}
		if !ai.Record.EnteredAt.IsZero() {
			pr.Since = ai.Record.EnteredAt.UTC().Format(time.RFC3339)
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
	return out
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
