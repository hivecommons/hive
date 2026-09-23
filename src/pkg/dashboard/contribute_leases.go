package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/celtrigger"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hooks"
	"github.com/hivecommons/hive/pkg/timeline"
	"github.com/hivecommons/hive/pkg/worksource"
)

// taskLease is the server-authoritative record of a task the hub issued to a
// contributor identity (hivecommons/hive C4). It binds the assignment to the
// {profile, task, repo, generation} tuple and an expiry so a reconnecting relay's
// task_progress can only RE-ADOPT a task the hub actually assigned to it, under the
// exact generation it was assigned, and only until the lease expires. It is minted
// by recordLease at assignment and cleared by revokeLease on every release path; a
// resume that does not match an unexpired lease here is rejected outright.
//
// The registry holds one lease PER TASK, keyed by leaseKey(identity, taskID)
// (hivecommons/hive#7774). It was keyed by identity alone when it was written, on
// the premise that an identity holds one task at a time — but the concurrency
// gate in selectTask lets an identity hold max_concurrent tasks across its live
// connections (2 for contributor, 5 for trusted/merger), so a second assignment
// silently evicted the first task's lease. A flap on the connection working the
// first task then found only the second's lease, the resume was rejected, and a
// healthy agent was revoked mid-turn and its issue requeued as failed. Every
// task the hub has issued and not released is now independently re-adoptable.
type taskLease struct {
	identity string
	taskID   string
	repo     string
	number   int
	// key is the canonical, source-aware work-item identity (worksource.Ref.Key —
	// the same spelling WSTaskAssign.identityKey produces). It is carried so the
	// double-assignment guard in selectTask can tell which ITEM a lease holds
	// without re-deriving it from repo/number, which is wrong for external work:
	// Linear and Jira items deliberately carry Number == 0 and put their identity
	// in Key (#4245), so every zero-numbered item in a repo would collide as
	// "repo#0" (#5120).
	key             string
	title           string
	tier            string
	stage           string
	gen             uint64
	triageVerdict   string
	triageRationale string
	// restored marks a lease loadLeases read from disk at startup rather than one
	// recordLease minted in this process (#5681). It is deliberately NOT persisted:
	// it means "issued by the PREVIOUS process, whose holder has not reconnected
	// here yet", which is only ever true for the current boot. It used to gate the
	// double-assignment guard's hold on the item to a post-restart grace window;
	// since #7773 every unexpired lease is a hold (leasedIssueKeys), so this is
	// diagnostic — it says where a lease came from, not what it does.
	restored  bool
	expiresAt time.Time
	// claimedBy / claimExpiresAt / claimPosted record the issue claim the hub
	// asserted for this task (hivecommons/hive#8380): who, until when, and
	// whether the claim comment reached the forge or lives on this lease only
	// (a tier below the comment rung). Zero when claims are off.
	claimedBy      string
	claimExpiresAt time.Time
	claimPosted    bool
}

// leaseTTL is how long a hub-issued task lease remains re-adoptable after the last
// time the relay proved it was still working (hivecommons/hive C4). It is aligned
// with wsTaskTimeout (the wedged-task backstop) and, since #4260, is measured from
// the SAME event: recordLease stamps it at assignment and renewLease re-stamps it on
// every accepted task_progress, exactly where reclaimExpiredLeases re-stamps
// lastLeaseRenew. The two clocks therefore expire together, which is what makes the
// intended invariant true — a task past its re-adoption window is also one the
// wedged-task backstop has reclaimed, and a task the backstop considers alive is
// still re-adoptable.
//
// #4260: before that renewal existed, expiresAt was stamped once at assignment and
// never moved, so the alignment was only nominal. A perfectly healthy task that had
// been reporting progress for longer than leaseTTL was NEVER reclaimed (correctly —
// it was alive) yet its lease had silently expired, so the first socket drop after
// that point could not be resumed: lookupLease rejected the reconnecting relay,
// the hub sent task_revoke, and the same issue was re-assigned as a new task,
// typing a fresh prompt into a pane whose CLI was still mid-turn.
//
// A resume presented after this window is treated as a stale/forged claim and
// rejected; the relay simply asks for fresh work via "ready".
const leaseTTL = wsTaskTimeout

const (
	StageSpec      = "spec"
	StagePlan      = "plan"
	StageImplement = "implement"
)

var orderedLeaseStages = []string{StageSpec, StagePlan, StageImplement}

// leaseStageMutation selects which stage move mutateLeaseStage performs. The
// three moves share one locked read-validate-persist-audit-emit path and differ
// only in which target stages they accept.
type leaseStageMutation int

const (
	// leaseStageAdvance moves to exactly the next stage in orderedLeaseStages.
	leaseStageAdvance leaseStageMutation = iota
	// leaseStageRetry keeps the current stage and only re-mints the generation.
	leaseStageRetry
	// leaseStageReset moves to a strictly EARLIER stage (#8350). Owner-only:
	// it is reachable solely through handleRunReset and the reason it records
	// is the operator's, never a contributor's.
	leaseStageReset
)

// Sentinel lease errors let the owner-facing route (handleRunReset) map a
// refused move to the right status without parsing message text. They wrap
// the task-specific detail with %w so errors.Is still matches.
var (
	errLeaseNotFound     = errors.New("lease not found")
	errLeaseExpired      = errors.New("lease expired")
	errLeaseStageInvalid = errors.New("invalid lease stage transition")
	errLeaseResetReason  = errors.New("lease stage reset requires a reason")
)

func validStage(s string) bool {
	return leaseStageIndex(s) >= 0
}

// leaseStageIndex is a stage's position in orderedLeaseStages, or -1 when the
// name is not a stage. Ordering is what makes "earlier" and "next" checkable.
func leaseStageIndex(s string) int {
	for i, stage := range orderedLeaseStages {
		if s == stage {
			return i
		}
	}
	return -1
}

func nextLeaseStage(from string) string {
	for i, stage := range orderedLeaseStages {
		if stage == from && i+1 < len(orderedLeaseStages) {
			return orderedLeaseStages[i+1]
		}
	}
	return ""
}

// leaseKey is the registry key for one task held by one identity (#7774). The
// separator is a control character neither half can contain: identities are
// ContributorID or ContributorID#session (sanitized labels), task ids are
// hub-minted. Both halves are also stored on the lease itself, so nothing ever
// has to parse a key back apart.
func leaseKey(identity, taskID string) string {
	return identity + "\x1f" + taskID
}

// runKey is the canonical work-item identity a lease holds: the source-aware
// key when the assignment carried one, else the repo#number spelling. It is the
// key the runs API exposes and the one handleRunReset resolves back to a lease.
func (l *taskLease) runKey() string {
	if l.key != "" {
		return l.key
	}
	return worksource.Ref{Repo: l.repo, Number: l.number}.Key()
}

// leaseForLocked returns the lease this identity holds for taskID, or nil. It is
// the one place the composite key is looked up, so callers — and tests — never
// spell it themselves. The caller must hold leaseMu.
func (h *ContributeWSHub) leaseForLocked(identity, taskID string) *taskLease {
	if h.leases == nil {
		return nil
	}
	return h.leases[leaseKey(identity, taskID)]
}

// recordLease registers (or replaces) the server-authoritative lease for one
// task issued to an identity (hivecommons/hive C4). It stores the
// exact {task, repo, generation, tier} the hub issued plus an expiry, so a later
// reconnect can be validated against what the server actually handed out — never
// reconstructed from client-supplied fields. Called from selectTask under the new
// assignment's generation.
func (h *ContributeWSHub) recordLease(identity, taskID, repo string, number int, tier string, gen uint64, now time.Time) error {
	return h.recordLeaseForKey(identity, taskID, repo, number, "", tier, gen, now)
}

// recordLeaseForKey is recordLease plus the assignment's canonical work-item key.
// selectTask calls this form with chosen.ref.Key() so an EXTERNAL item's lease
// carries its real identity; an empty key falls back to the repo#number spelling,
// which is exact for GitHub work and is what the plain recordLease form records.
//
// A new lease never touches the identity's OTHER leases (#7774): an identity
// holding task X on one connection and being assigned task Y on another keeps
// both, and each stays re-adoptable on its own. Re-recording the SAME task
// replaces that task's lease, as before.
//
// A lease that did not reach disk is NOT a lease (#8287). The registry file is
// what the next process boots from, so a grant that lives only in memory
// vanishes on restart and the contributor resumes against a hub with no record
// of it. When the persist fails the new entry is withdrawn again — the task's
// previous lease, if any, is put back — and the error is returned so the caller
// refuses the grant instead of reporting success for a record that does not
// exist. In-memory and on-disk state therefore never disagree about a grant.
func (h *ContributeWSHub) recordLeaseForKey(identity, taskID, repo string, number int, key, tier string, gen uint64, now time.Time) error {
	return h.recordLeaseForKeyStage(identity, taskID, repo, number, key, tier, "", gen, now)
}

func (h *ContributeWSHub) recordLeaseForKeyStage(identity, taskID, repo string, number int, key, tier, stage string, gen uint64, now time.Time) error {
	if identity == "" || taskID == "" {
		return nil
	}
	if stage != "" && !validStage(stage) {
		return fmt.Errorf("invalid lease stage %q", stage)
	}
	if key == "" {
		key = worksource.Ref{Repo: repo, Number: number}.Key()
	}
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	if h.leases == nil {
		h.leases = make(map[string]*taskLease)
	}
	k := leaseKey(identity, taskID)
	prev := h.leases[k]
	triageVerdict, triageRationale := "", ""
	removedAdmissions := map[string]*taskLease{}
	if stage != "" {
		for admissionKey, l := range h.leases {
			if l != nil && l.identity == runAdmissionIdentity && l.key == key && l.stage == stage {
				triageVerdict, triageRationale = l.triageVerdict, l.triageRationale
				copyLease := *l
				removedAdmissions[admissionKey] = &copyLease
				delete(h.leases, admissionKey)
			}
		}
	}
	h.leases[k] = &taskLease{
		identity:        identity,
		taskID:          taskID,
		repo:            repo,
		number:          number,
		key:             key,
		tier:            tier,
		stage:           stage,
		gen:             gen,
		triageVerdict:   triageVerdict,
		triageRationale: triageRationale,
		expiresAt:       now.Add(leaseTTL),
	}
	// #5681: a lease the hub issued must outlive the process that issued it.
	if err := h.saveLeasesLocked(); err != nil {
		if prev != nil {
			h.leases[k] = prev
		} else {
			delete(h.leases, k)
		}
		for admissionKey, l := range removedAdmissions {
			h.leases[admissionKey] = l
		}
		return fmt.Errorf("persisting lease for %s: %w", taskID, err)
	}
	return nil
}

func (h *ContributeWSHub) advanceLeaseStage(identity, taskID, to string, now time.Time) (taskLease, error) {
	return h.mutateLeaseStage(identity, taskID, to, leaseStageAdvance, "", now)
}

func (h *ContributeWSHub) retryLeaseStage(identity, taskID string, now time.Time) (taskLease, error) {
	return h.mutateLeaseStage(identity, taskID, "", leaseStageRetry, "", now)
}

// resetLeaseStage moves a run's lease BACK to an earlier stage (#8350): for
// example implement back to plan when the plan was rejected. It is the one
// sanctioned backwards move; advanceLeaseStage refuses both skipping and going
// back, and retryLeaseStage only re-mints the current stage.
//
// It is OWNER-ONLY by construction: nothing a contributor relay sends reaches
// it. The caller (handleRunReset) has already passed requireOwnerRole, and the
// reason is required so the record of WHY the run stepped back travels with
// the transition into the agent audit sink, the lifecycle timeline, and the
// hook payload, where the runs API surfaces it as stage history.
//
// Like every stage move it mints a new generation via the shared generator, so
// the relay that held the run under the previous generation can no longer
// resume it (lookupLease matches on the exact generation), renews the lease
// window from now, and persists before anything observes the change: a save
// failure rolls the in-memory record back and surfaces the error, so the
// registry on disk and in memory never disagree about which stage a run is in.
// `to` must be a strictly earlier stage; the same stage is refused (that is a
// retry) and a later stage is refused (that is an advance).
func (h *ContributeWSHub) resetLeaseStage(identity, taskID, to, reason string, now time.Time) (taskLease, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return taskLease{}, errLeaseResetReason
	}
	return h.mutateLeaseStage(identity, taskID, to, leaseStageReset, reason, now)
}

func (h *ContributeWSHub) mutateLeaseStage(identity, taskID, to string, mode leaseStageMutation, reason string, now time.Time) (taskLease, error) {
	if identity == "" || taskID == "" {
		return taskLease{}, fmt.Errorf("identity and taskID are required")
	}
	var out taskLease
	var auditAction, from string

	h.leaseMu.Lock()
	l := h.leaseForLocked(identity, taskID)
	if l == nil {
		h.leaseMu.Unlock()
		return taskLease{}, fmt.Errorf("%w for %s", errLeaseNotFound, taskID)
	}
	// An ADVANCE or RESET needs a live lease: a stage can only complete, or be
	// stepped back by an owner, while someone holds it. A RETRY is the reclaim
	// path (#8303): it exists precisely because the generation lapsed without
	// reaching final, so an expired lease is its expected input and gets a
	// fresh window under its new generation.
	if l.expiresAt.IsZero() || (mode != leaseStageRetry && now.After(l.expiresAt)) {
		h.leaseMu.Unlock()
		return taskLease{}, fmt.Errorf("%w for %s", errLeaseExpired, taskID)
	}
	if l.stage == "" {
		h.leaseMu.Unlock()
		return taskLease{}, fmt.Errorf("%w: lease for %s has no stage", errLeaseStageInvalid, taskID)
	}
	switch mode {
	case leaseStageRetry:
		to = l.stage
		auditAction = agent.AuditLeaseStageRetried
	case leaseStageAdvance:
		if !validStage(to) {
			h.leaseMu.Unlock()
			return taskLease{}, fmt.Errorf("%w: unknown stage %q", errLeaseStageInvalid, to)
		}
		if want := nextLeaseStage(l.stage); to != want {
			h.leaseMu.Unlock()
			return taskLease{}, fmt.Errorf("%w: advance from %q to %q", errLeaseStageInvalid, l.stage, to)
		}
		auditAction = agent.AuditLeaseStageAdvanced
	case leaseStageReset:
		if !validStage(to) {
			h.leaseMu.Unlock()
			return taskLease{}, fmt.Errorf("%w: unknown stage %q", errLeaseStageInvalid, to)
		}
		if leaseStageIndex(to) >= leaseStageIndex(l.stage) {
			h.leaseMu.Unlock()
			return taskLease{}, fmt.Errorf("%w: reset from %q to %q is not a move to an earlier stage", errLeaseStageInvalid, l.stage, to)
		}
		auditAction = agent.AuditLeaseStageReset
	default:
		h.leaseMu.Unlock()
		return taskLease{}, fmt.Errorf("unknown lease stage mutation %d", mode)
	}

	prevStage, prevGen, prevExpires := l.stage, l.gen, l.expiresAt
	from = prevStage
	l.stage = to
	l.gen = h.nextTaskGen()
	for l.gen <= prevGen {
		l.gen = h.nextTaskGen()
	}
	l.expiresAt = now.Add(leaseTTL)
	if err := h.saveLeasesLocked(); err != nil {
		l.stage, l.gen, l.expiresAt = prevStage, prevGen, prevExpires
		h.leaseMu.Unlock()
		return taskLease{}, fmt.Errorf("persisting lease stage for %s: %w", taskID, err)
	}
	out = *l
	h.leaseMu.Unlock()

	h.recordLeaseStageAudit(auditAction, taskID, from, to, reason, out.gen)
	h.emitLeaseStageTransition(from, to, reason, mode == leaseStageReset, out)
	return out, nil
}

// recordLeaseStageAudit books a stage move on the agent audit sink. reason is
// only set for a reset; agent.Fields drops empty strings, so advance and retry
// entries keep their original shape.
func (h *ContributeWSHub) recordLeaseStageAudit(action, taskID, from, to, reason string, gen uint64) {
	if h == nil || h.server == nil {
		return
	}
	h.server.AgentAuditSink().Record("system", action, taskID,
		agent.Fields("stage_from", from, "stage_to", to, "reason", reason, "gen", gen))
}

// emitLeaseStageTransition records a persisted stage move on the lifecycle
// timeline and fires the stage_completed hook/CEL transition. A reset (#8350)
// rides the same transition rather than a new catalog entry: it carries
// `reason` and `attrs.reset = "true"` so a hook's `when:` can tell a step
// back from a hand-off (`t.attrs.reset == "true"`), and the runs API reads
// the reason back out of the timeline event as stage history.
func (h *ContributeWSHub) emitLeaseStageTransition(from, to, reason string, reset bool, l taskLease) {
	h.emitLeaseStageTransitionAt(from, to, reason, reset, l, time.Time{})
}

func (h *ContributeWSHub) emitLeaseStageTransitionAt(from, to, reason string, reset bool, l taskLease, at time.Time) {
	if h == nil || h.server == nil {
		return
	}
	attrs := map[string]string{
		"stage_from": from,
		"stage_to":   to,
		"gen":        strconv.FormatUint(l.gen, 10),
	}
	if l.key != "" {
		attrs["issue_ref"] = l.key
	}
	if reason != "" {
		attrs["reason"] = reason
	}
	if reset {
		attrs["reset"] = "true"
	}
	eventAt := int64(0)
	if !at.IsZero() {
		eventAt = at.UnixMilli()
	}
	h.server.LifecycleTimeline().Record(timeline.Event{
		IssueRef: l.runKey(),
		Kind:     timeline.KindStageCompleted,
		Agent:    l.identity,
		At:       eventAt,
		Attrs:    attrs,
	})
	payload := hooks.Payload{
		Transition: hooks.TransitionStageCompleted,
		Run:        l.taskID,
		StageFrom:  from,
		StageTo:    to,
		Gen:        l.gen,
		Repo:       l.repo,
		Agent:      l.identity,
		Reason:     reason,
		Attrs:      attrs,
	}
	if h.server.deps != nil {
		if h.server.deps.HookFire != nil {
			h.server.deps.HookFire(context.Background(), payload)
		}
		if h.server.deps.CELTrigger != nil {
			h.server.deps.CELTrigger(context.Background(), celtrigger.NormalizedEvent{
				Kind:      celtrigger.KindStageCompleted,
				Repo:      l.repo,
				Number:    l.number,
				State:     "completed",
				Run:       l.taskID,
				StageFrom: from,
				StageTo:   to,
				Gen:       int64(l.gen),
			}, leaseStageTransitionSummary(from, to, reason, reset, l.taskID))
		}
	}
}

func (h *ContributeWSHub) completeImplementStage(identity, taskID string, now time.Time) bool {
	if h == nil || identity == "" || taskID == "" {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	h.leaseMu.Lock()
	l := h.leaseForLocked(identity, taskID)
	if l == nil || l.stage != StageImplement || l.expiresAt.IsZero() || now.After(l.expiresAt) {
		h.leaseMu.Unlock()
		return false
	}
	completed := *l
	delete(h.leases, leaseKey(identity, taskID))
	if err := h.saveLeasesLocked(); err != nil {
		h.logger.Warn("[contribute-ws] completed implement lease revoked in memory but not persisted",
			"identity", identity, "task", taskID, "error", err)
	}
	h.leaseMu.Unlock()

	h.recordLeaseStageAudit(agent.AuditLeaseStageAdvanced, taskID, StageImplement, "completed", "", completed.gen)
	h.emitLeaseStageTransitionAt(StageImplement, "completed", "", false, completed, now)
	if err := removeRunStageWorktree(completed.identity, leaseWorkKey(&completed), completed.stage, completed.gen); err != nil {
		h.logger.Warn("[contribute-ws] completed run-stage worktree cleanup failed",
			"identity", completed.identity, "task", taskID, "error", err)
	}
	return true
}

func (h *ContributeWSHub) completeWavefrontTask(task *WSTaskAssign, labels []string, startedAt time.Time) {
	if h == nil || h.server == nil || h.server.deps == nil || h.server.deps.WavefrontComplete == nil || task == nil {
		return
	}
	if task.Stage != StageImplement || task.SourceType != worksource.SourceTypeRun || strings.TrimSpace(task.ExternalID) == "" {
		return
	}
	revision, ok := wavefrontRevisionFromLabels(labels)
	if !ok {
		return
	}
	if err := h.server.deps.WavefrontComplete(context.Background(), task.identityKey(), task.ExternalID, revision, startedAt); err != nil {
		h.logger.Warn("[contribute-ws] wavefront completion failed",
			"task", task.TaskID, "key", task.identityKey(), "error", err)
	}
}

func wavefrontRevisionFromLabels(labels []string) (string, bool) {
	const prefix = "graph-rev/"
	for _, label := range labels {
		if rev, ok := strings.CutPrefix(label, prefix); ok && strings.TrimSpace(rev) != "" {
			return strings.TrimSpace(rev), true
		}
	}
	return "", false
}

// leaseStageTransitionSummary is the one-line CEL trigger description of a
// stage move; a reset names itself and carries the owner's reason.
func leaseStageTransitionSummary(from, to, reason string, reset bool, taskID string) string {
	if reset {
		return fmt.Sprintf("stage_reset %s→%s for %s: %s", from, to, taskID, reason)
	}
	return fmt.Sprintf("stage_completed %s→%s for %s", from, to, taskID)
}

// runLeaseHolder resolves a run key (taskLease.runKey) to a copy of the live,
// staged lease that holds it, so the owner-facing runs routes can address a run
// by the key the runs API exposes rather than by {identity, task}. Expired and
// unstaged leases are ignored, matching activeRunLeaseSnapshots; if more than
// one live lease names the same item the highest generation wins, because that
// is the one lookupLease would honor.
func (h *ContributeWSHub) runLeaseHolder(key string, now time.Time) (taskLease, bool) {
	if h == nil || key == "" {
		return taskLease{}, false
	}
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	var best *taskLease
	for _, l := range h.leases {
		if l == nil || l.stage == "" || l.expiresAt.IsZero() || now.After(l.expiresAt) {
			continue
		}
		leaseKey := leaseWorkKey(l)
		canonicalKey := ""
		if h.server != nil {
			canonicalKey = h.server.canonicalRunKey(l.repo, l.number, runKeyOfLease(leaseKey, l.repo), leaseKey)
		}
		if l.runKey() != key && leaseKey != key && canonicalKey != key {
			continue
		}
		if best == nil || l.gen > best.gen {
			best = l
		}
	}
	if best == nil {
		return taskLease{}, false
	}
	return *best, true
}

// renewLease extends an identity's server-issued lease window when the relay proves
// it is still working the task (kubestellar/hive#4260). It is the lease-registry half
// of the lastLeaseRenew stamp that reclaimExpiredLeases reads: both are driven by the
// same accepted task_progress, so "still alive" and "still re-adoptable" cannot drift
// apart and a long-running task does not lose the ability to survive a reconnect
// simply because it has been working for longer than leaseTTL.
//
// It grants NOTHING a caller did not already have. The lease is only touched when
// this identity holds one for this exact taskID, so a connection can neither renew
// another identity's lease nor extend a lease for a task it does not hold; a revoked
// lease is absent and stays absent. Only expiresAt moves — the {task, repo, number,
// tier, generation} tuple lookupLease matches on is never rewritten, so the C4
// exact-match contract and the #2568 generation fence are untouched.
//
// A persist failure is returned, not acted on (#8287): the extended window stays
// in memory regardless, because a failed renew persist must never revoke a live
// task — the relay is provably still working it. The caller logs; the next
// successful save (a later renew, any release) carries the window to disk.
func (h *ContributeWSHub) renewLease(identity, taskID string, now time.Time) error {
	if identity == "" || taskID == "" {
		return nil
	}
	h.leaseMu.Lock()
	var renewed *taskLease
	var saveErr error
	if l := h.leaseForLocked(identity, taskID); l != nil {
		l.expiresAt = now.Add(leaseTTL)
		renewed = l
		// #5681: persist the EXTENDED window. Without this a restart would restore
		// the window as it stood at assignment, so a task that had been progressing
		// for longer than leaseTTL — the exact case #4260 fixed in memory — would
		// come back already expired and could not be resumed.
		if err := h.saveLeasesLocked(); err != nil {
			saveErr = fmt.Errorf("persisting renewed lease for %s: %w", taskID, err)
		}
	}
	h.leaseMu.Unlock()
	// #8380: the worker claim travels with the lease — renew it too, outside
	// leaseMu, so a task that outlives the 30m claim TTL stays visibly held.
	if renewed != nil && renewed.number > 0 {
		h.renewClaimForLease(identity, renewed.repo, renewed.number)
	}
	return saveErr
}

// revokeLeaseForKey revokes whichever lease the identity holds on the given
// item key, if any — the takeover path for a relay whose socket is down and
// therefore has no connection to yank (#8380). Returns whether one was found.
func (h *ContributeWSHub) revokeLeaseForKey(identity, key string) bool {
	if identity == "" || key == "" {
		return false
	}
	h.leaseMu.Lock()
	taskID := ""
	for _, l := range h.leases {
		if l != nil && l.identity == identity && leaseClaimKey(l) == key {
			taskID = l.taskID
			break
		}
	}
	h.leaseMu.Unlock()
	if taskID == "" {
		return false
	}
	h.revokeLease(identity, taskID)
	return true
}

// revokeLease removes the server-authoritative lease for one task an identity holds,
// on any release path (hivecommons/hive C4): disconnect, ready-abandon,
// task_complete, task_failed, operator requeue, and lease-TTL expiry. Once revoked,
// a reconnecting relay's task_progress for that task no longer matches any lease and
// cannot re-adopt it — closing the window in which a released task could be
// resurrected from client fields. Only that task's entry goes; the identity's other
// leases are untouched (#7774). An empty taskID revokes every lease the identity
// holds — no production path passes one today, but the meaning is kept explicit.
func (h *ContributeWSHub) revokeLease(identity, taskID string) {
	if identity == "" {
		return
	}
	h.leaseMu.Lock()
	revoked := false
	// #8380: remember which items went so their worker claims go with them.
	var releasedKeys []string
	if taskID != "" {
		if l, ok := h.leases[leaseKey(identity, taskID)]; ok {
			if l != nil {
				releasedKeys = append(releasedKeys, leaseClaimKey(l))
			}
			delete(h.leases, leaseKey(identity, taskID))
			revoked = true
		}
	} else {
		for k, l := range h.leases {
			if l != nil && l.identity == identity {
				releasedKeys = append(releasedKeys, leaseClaimKey(l))
				delete(h.leases, k)
				revoked = true
			}
		}
	}
	if revoked {
		// #5681: a revoke that did not reach disk would be undone by the next
		// restart, resurrecting a released task. Persist it with the same urgency
		// as the in-memory delete. A persist failure is logged and the revoke
		// stands (#8287): revocation must never be blocked by disk state, because
		// leaving a stale grant live is the worse outcome — the in-memory delete
		// is what fences a released task right now, and the next successful
		// save carries it to disk.
		if err := h.saveLeasesLocked(); err != nil {
			h.logger.Warn("[contribute-ws] lease revoked in memory but not persisted",
				"identity", identity, "task", taskID, "error", err)
		}
	}
	h.leaseMu.Unlock()
	for _, key := range releasedKeys {
		h.releaseClaimForLease(identity, key, "lease revoked")
	}
}

// leaseClaimKey is the ledger key for the item a lease holds: the canonical
// worksource key when recorded (#5681), else the legacy repo#number spelling.
func leaseClaimKey(l *taskLease) string {
	if l.key != "" {
		return l.key
	}
	return fmt.Sprintf("%s#%d", l.repo, l.number)
}

// lookupLease returns the active, unexpired server-issued lease for an identity that
// EXACTLY matches the resume claim (hivecommons/hive C4): same task_id, same
// canonical repo, same number, and same assignment generation. Any mismatch — no
// lease, wrong task, wrong repo/number, wrong (or zero) generation, or an expired
// lease — returns nil, so a reconnecting relay may only re-adopt the precise task the
// hub assigned it, under the generation it was assigned, and only within the lease
// window. It never reconstructs ownership from the client's own fields.
//
// clientGen == 0 (an unversioned relay) is deliberately NOT honored here: re-adoption
// requires proving possession of the server-issued generation token, which an
// unversioned relay cannot present. Such a relay is asked to re-`ready` for fresh
// work instead of resurrecting a lease it cannot authenticate.
func (h *ContributeWSHub) lookupLease(identity, taskID, repo string, number int, clientGen uint64, now time.Time) *taskLease {
	if identity == "" || taskID == "" || clientGen == 0 {
		return nil
	}
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	// Keyed by {identity, task} (#7774): a lease for a DIFFERENT task the same
	// identity holds is simply not this one, rather than a mismatch that rejects
	// the resume — which is what revoked healthy work whenever an identity held
	// more than one task.
	l := h.leaseForLocked(identity, taskID)
	if l == nil {
		return nil
	}
	if now.After(l.expiresAt) {
		// Expired: drop it so it can never be re-adopted, and treat as no lease.
		// The in-memory drop is what refuses the resume; a persist failure only
		// means loadLeases must skip the expired record itself, which it does.
		delete(h.leases, leaseKey(identity, taskID))
		if err := h.saveLeasesLocked(); err != nil {
			h.logger.Warn("[contribute-ws] expired lease dropped in memory but not persisted",
				"identity", identity, "task", taskID, "error", err)
		}
		return nil
	}
	if l.gen != clientGen {
		return nil
	}
	if repo != "" && l.repo != repo {
		return nil
	}
	if number != 0 && l.number != number {
		return nil
	}
	return l
}

func (h *ContributeWSHub) leaseStageForDecision(identity, taskID string) string {
	if h == nil || identity == "" || taskID == "" {
		return ""
	}
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	if l := h.leaseForLocked(identity, taskID); l != nil {
		return l.stage
	}
	return ""
}

// persistedLease is the on-disk form of a taskLease (#5681).
//
// It carries the lease and nothing else. There is no credential in it: the scoped
// GitHub token is minted per assignment and delivered separately (#2537), never
// stored here. Restoring a lease therefore grants exactly one thing — the ability
// to RE-ADOPT a task the hub already issued to that identity — and never the
// ability to obtain a fresh credential without passing selectTask's gates.
type persistedLease struct {
	Identity        string    `json:"identity"`
	TaskID          string    `json:"task_id"`
	Repo            string    `json:"repo"`
	Number          int       `json:"number"`
	Key             string    `json:"key,omitempty"`
	Title           string    `json:"title,omitempty"`
	Tier            string    `json:"tier"`
	Stage           string    `json:"stage,omitempty"`
	Gen             uint64    `json:"gen"`
	TriageVerdict   string    `json:"triage_verdict,omitempty"`
	TriageRationale string    `json:"triage_rationale,omitempty"`
	ExpiresAt       time.Time `json:"expires_at"`
	// Claim fields (#8380); all omitempty so a registry written with claims
	// off is byte-for-byte what it was.
	ClaimedBy      string     `json:"claimed_by,omitempty"`
	ClaimExpiresAt *time.Time `json:"claim_expires_at,omitempty"`
	ClaimPosted    bool       `json:"claim_posted,omitempty"`
}

// setLeaseClaim records an issue claim on an existing lease (#8380). A lease
// that is not there (already released, or never persisted) is left alone —
// the claim has nothing to attach to. The persist failure policy matches
// renewLease: the in-memory record keeps the claim and the error is logged,
// because the claim's source of truth is the forge, not this file.
func (h *ContributeWSHub) setLeaseClaim(identity, taskID string, claim ghpkg.IssueClaimMark, posted bool) {
	if h == nil || identity == "" || taskID == "" {
		return
	}
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	l := h.leases[leaseKey(identity, taskID)]
	if l == nil {
		return
	}
	l.claimedBy = claim.Identity
	l.claimExpiresAt = claim.ExpiresAt
	l.claimPosted = posted
	if err := h.saveLeasesLocked(); err != nil {
		h.logger.Warn("[contribute-ws] lease claim not persisted", "task", taskID, "error", err)
	}
}

func (h *ContributeWSHub) taskLeasesPath() string {
	if h != nil && h.taskLeasesFile != "" {
		return h.taskLeasesFile
	}
	return taskLeasesFile
}

// saveLeasesLocked writes the server-issued lease registry to disk (#5681).
//
// THE CALLER MUST HOLD leaseMu. The snapshot and the write happen under the same
// lock deliberately: if the snapshot were taken under the lock and the rename done
// outside it, two concurrent mutations could land their renames in the opposite
// order and leave the file describing an OLDER registry than the one in memory —
// and the whole point of the file is that it is what the next process boots from.
// The cost is negligible: the file holds one record per held task (bounded by
// maxWSConnections times the tier's max_concurrent) and every mutation site is
// low-frequency —
// assignment, release, and one task_progress per relay per PROGRESS_REPORT_INTERVAL_MS.
//
// Leases already past their expiry are skipped rather than written: a lease that
// can no longer be re-adopted must not be able to come back from disk.
//
// Every failure — marshal, mkdir, temp-file create, chmod, write, fsync, close,
// rename, directory fsync — is logged AND returned (#8287). It used to be logged
// and swallowed, so a caller that had just handed out a lease had no way to know
// the grant was not durable and reported success for a record that would be gone
// on the next restart. Callers decide what a failure means for them: a grant is
// refused (recordLeaseForKey), a renew keeps its in-memory window (renewLease),
// and a revoke stands regardless (revokeLease). The file is left describing the
// last registry that was successfully committed; a failed attempt never leaves a
// partial file or a stray temp file behind.
func (h *ContributeWSHub) saveLeasesLocked() error {
	if h == nil || !h.persistTaskLedgers {
		return nil
	}
	now := time.Now()
	records := make([]persistedLease, 0, len(h.leases))
	for _, l := range h.leases {
		if l == nil || l.expiresAt.IsZero() || now.After(l.expiresAt) {
			continue
		}
		rec := persistedLease{
			Identity:        l.identity,
			TaskID:          l.taskID,
			Repo:            l.repo,
			Number:          l.number,
			Key:             l.key,
			Title:           l.title,
			Tier:            l.tier,
			Stage:           l.stage,
			Gen:             l.gen,
			TriageVerdict:   l.triageVerdict,
			TriageRationale: l.triageRationale,
			ExpiresAt:       l.expiresAt,
			ClaimedBy:       l.claimedBy,
			ClaimPosted:     l.claimPosted,
		}
		if !l.claimExpiresAt.IsZero() {
			exp := l.claimExpiresAt
			rec.ClaimExpiresAt = &exp
		}
		records = append(records, rec)
	}
	data, err := json.Marshal(records)
	if err != nil {
		h.logger.Warn("[contribute-ws] task leases marshal failed", "error", err)
		return fmt.Errorf("task leases marshal: %w", err)
	}
	path := h.taskLeasesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.logger.Warn("[contribute-ws] task leases directory creation failed", "error", err)
		return fmt.Errorf("task leases directory creation: %w", err)
	}
	// Crash-safe persist per the #5625 idiom: a UNIQUE temp name (a fixed name
	// lets a non-cooperating process clobber a commit in flight), fsync of the
	// bytes before the rename (the whole point of this file is that the next
	// process boots from it, so the record must be durable, not just renamed),
	// and an fsync of the directory so the rename itself survives a crash.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		h.logger.Warn("[contribute-ws] task leases temp creation failed", "error", err)
		return fmt.Errorf("task leases temp creation: %w", err)
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	// 0600, unlike the sibling ledgers: this file is the C4 authorization record
	// that lookupLease matches a resume against, so it is owner-only on both sides
	// — nothing else on the host has any business reading which contributor holds
	// which work item, and nothing else has any business writing it. CreateTemp
	// already makes 0600; the explicit chmod pins the invariant rather than
	// inheriting it.
	if err := tmp.Chmod(0o600); err != nil {
		h.logger.Warn("[contribute-ws] task leases chmod failed", "error", err)
		return fmt.Errorf("task leases chmod: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		h.logger.Warn("[contribute-ws] task leases write failed", "error", err)
		return fmt.Errorf("task leases write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		h.logger.Warn("[contribute-ws] task leases sync failed", "error", err)
		return fmt.Errorf("task leases sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		h.logger.Warn("[contribute-ws] task leases close failed", "error", err)
		return fmt.Errorf("task leases close: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		h.logger.Warn("[contribute-ws] task leases rename failed", "error", err)
		return fmt.Errorf("task leases rename: %w", err)
	}
	keep = true
	directory, err := os.Open(dir)
	if err != nil {
		h.logger.Warn("[contribute-ws] task leases directory open failed", "error", err)
		return fmt.Errorf("task leases directory open: %w", err)
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil {
		h.logger.Warn("[contribute-ws] task leases directory sync failed", "error", err)
		return fmt.Errorf("task leases directory sync: %w", err)
	}
	return nil
}

// loadLeases restores the server-issued lease registry at hub startup (#5681).
//
// Leases lived only in process memory. A hub restart — which self-upgrade rolls
// (#5391) make routine rather than rare — erased every record of what the hub had
// assigned, while the relays carried on working: they hold one task at a time and
// re-assert it on reconnect (#4260). With the registry empty, EVERY in-flight
// resume failed lookupLease, was answered "no active lease for this task", and had
// its agent interrupted mid-turn — then was handed the identical issue back seconds
// later. Ownership was never in question; only the record of it.
//
// This does not weaken C4. The restored record is still one the SERVER issued and
// wrote itself; a resume still has to match it exactly on
// {identity, task_id, repo, number, generation} and still has to be inside the
// window. Nothing is reconstructed from client-supplied fields, and a lease whose
// expiry has passed is dropped rather than loaded — so a stale file cannot
// resurrect a task that is no longer re-adoptable.
func (h *ContributeWSHub) loadLeases() {
	if h == nil || !h.persistTaskLedgers {
		return
	}
	data, err := os.ReadFile(h.taskLeasesPath())
	if err != nil {
		return
	}
	var records []persistedLease
	if json.Unmarshal(data, &records) != nil {
		h.logger.Warn("[contribute-ws] task leases file unreadable; starting with an empty registry")
		return
	}
	now := time.Now()
	var maxGen uint64
	restored := 0

	h.leaseMu.Lock()
	if h.leases == nil {
		h.leases = make(map[string]*taskLease)
	}
	for _, rec := range records {
		// gen == 0 could never be matched by lookupLease (it refuses clientGen 0),
		// so such a record is unusable; drop it rather than hold an issue hostage.
		if rec.Identity == "" || rec.TaskID == "" || rec.Gen == 0 {
			continue
		}
		if rec.ExpiresAt.IsZero() || now.After(rec.ExpiresAt) {
			continue
		}
		key := rec.Key
		if key == "" {
			key = worksource.Ref{Repo: rec.Repo, Number: rec.Number}.Key()
		}
		// One record per task (#7774). A file written before that held at most
		// one record per identity and loads unchanged; a file written after may
		// hold several for one identity, each of which must come back.
		l := &taskLease{
			identity:        rec.Identity,
			taskID:          rec.TaskID,
			repo:            rec.Repo,
			number:          rec.Number,
			key:             key,
			title:           rec.Title,
			tier:            rec.Tier,
			stage:           rec.Stage,
			gen:             rec.Gen,
			triageVerdict:   rec.TriageVerdict,
			triageRationale: rec.TriageRationale,
			restored:        true,
			expiresAt:       rec.ExpiresAt,
			claimedBy:       rec.ClaimedBy,
			claimPosted:     rec.ClaimPosted,
		}
		if rec.ClaimExpiresAt != nil {
			l.claimExpiresAt = *rec.ClaimExpiresAt
		}
		h.leases[leaseKey(rec.Identity, rec.TaskID)] = l
		if rec.Gen > maxGen {
			maxGen = rec.Gen
		}
		restored++
	}
	h.leaseMu.Unlock()

	// #2568: taskGen is an in-memory counter that restarts at zero, so without this
	// a post-restart assignment would mint generations that ALIAS the ones just
	// restored — and the Gate (generationAccepted) would then accept a pre-restart
	// straggler against a brand-new task that happened to draw the same number.
	// Advancing the counter past every restored generation keeps what the hub
	// issues strictly ahead of what it has already issued.
	for {
		cur := h.taskGen.Load()
		if cur >= maxGen || h.taskGen.CompareAndSwap(cur, maxGen) {
			break
		}
	}

	if restored > 0 {
		h.logger.Info("[contribute-ws] restored task leases across restart",
			"count", restored, "max_gen", maxGen)
	}
}

// pruneExpiredLeases drops leases that have aged out of their re-adoption window and
// rewrites the file when anything changed (#5681). lookupLease already drops an
// expired lease it happens to read, but a lease whose relay never comes back is
// never looked up: without this it would sit in the registry — and in the
// double-assignment guard below — until the process ended. Called from cleanupLoop
// alongside the other stale-state reaping. Returns how many were dropped.
func (h *ContributeWSHub) pruneExpiredLeases(now time.Time) int {
	dropped := 0
	var unknown []taskLease
	h.leaseMu.Lock()
	for k, l := range h.leases {
		if l == nil || l.expiresAt.IsZero() || now.After(l.expiresAt) {
			if l != nil && l.restored && l.stage == StageImplement {
				unknown = append(unknown, *l)
			}
			delete(h.leases, k)
			dropped++
		}
	}
	if dropped > 0 {
		// Log and continue (#8287): the in-memory drop is what matters, and
		// loadLeases skips expired records on its own if the file is stale.
		if err := h.saveLeasesLocked(); err != nil {
			h.logger.Warn("[contribute-ws] expired leases pruned in memory but not persisted",
				"dropped", dropped, "error", err)
		}
	}
	h.leaseMu.Unlock()
	for _, l := range unknown {
		h.recordWavefrontUnknown(l, now)
	}
	return dropped
}

func (h *ContributeWSHub) recordWavefrontUnknown(l taskLease, now time.Time) {
	if h == nil || h.server == nil || h.server.deps == nil || h.server.deps.WavefrontUnknown == nil {
		return
	}
	ref, ok := worksource.ParseKey(l.key)
	if !ok || ref.ExternalID == "" {
		return
	}
	externalID := ref.ExternalID
	if i := strings.LastIndex(externalID, ":"); i > 0 && externalID[i+1:] == StageImplement {
		externalID = externalID[:i]
	}
	started := l.expiresAt.Add(-leaseTTL)
	if started.IsZero() || started.After(now) {
		started = now
	}
	if err := h.server.deps.WavefrontUnknown(context.Background(), externalID, "stale in-flight lease after restart", started); err != nil {
		h.logger.Warn("[contribute-ws] wavefront unknown transition failed",
			"task", l.taskID, "key", l.key, "error", err)
	}
}

// leasedIssueKeys returns the canonical work-item keys that an unexpired lease is
// holding for some identity OTHER than exceptIdentity. selectTask adds them to its
// in-flight exclusions, so an item that is still RE-ADOPTABLE is never OFFERABLE
// (hivecommons/hive#7773).
//
// Those two windows used to be allowed to overlap. A dropped socket keeps its lease
// so the relay can resume (#4260), and the item was left merely cooling down under
// #2356's release hedge — ten minutes — while the lease stayed re-adoptable for
// leaseTTL, thirty. Nothing covered the gap: between ten and thirty minutes after a
// disconnected relay's last progress report the item was out of the live-connection
// scan, out of cooldown, and still resumable. A second contributor asking for work
// in that window was offered it; when the first relay came back — a laptop waking,
// a VPN reconnecting — lookupLease matched, resumeTaskToken minted it a fresh
// credential, and two contributors held the same issue with valid tokens. The
// design note for #5322 was right that the hedge "comfortably outlasts the
// reconnect backoff"; it did not outlast a medium-length outage.
//
// A lease that can still be resumed IS a hold, and is treated as one for exactly as
// long as it can be resumed: the moment it is released (task_complete, task_failed,
// ready-abandon, operator requeue, the wedged-task backstop) revokeLease removes
// it, and the moment it expires pruneExpiredLeases drops it. The cost #5681
// weighed — a park for the length of the lease on every disconnect — is real for a
// relay that never comes back, and it is the price of the invariant: the same
// item cannot be offerable to one contributor and resumable by another. #5681's
// two-minute post-restart grace was this rule applied to restored leases only; it
// is now the rule for every lease, so the restored flag no longer gates anything.
//
// A lease belonging to the REQUESTER is deliberately never an exclusion: asking for
// work is itself the statement that it is not holding that task any more. An
// identity that runs several connections and loses one mid-task can therefore be
// re-offered that task on its other connection while the first could still resume
// it — a duplicate within one account, in a configuration the docs discourage —
// which is narrower than the cross-contributor duplicate this closes.
func (h *ContributeWSHub) leasedIssueKeys(exceptIdentity string, now time.Time) map[string]bool {
	keys := make(map[string]bool)
	if h == nil {
		return keys
	}
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	for _, l := range h.leases {
		if l == nil || l.identity == exceptIdentity {
			continue
		}
		if l.expiresAt.IsZero() || now.After(l.expiresAt) {
			continue
		}
		if l.identity == runAdmissionIdentity {
			continue
		}
		if l.key != "" {
			keys[l.key] = true
		}
	}
	return keys
}

// taskLeasesFile is the durable home of the server-issued task-lease registry
// (#5681). It sits beside the other contributor ledgers, but is written 0600: it is
// the C4 authorization record a resume is matched against, not a report.
var taskLeasesFile = "/data/contributors/task-leases.json"

// reclaimExpiredLeases is the hub-owned LEASE-TTL backstop (kubestellar/hive#2568,
// option 4). A connection that is still HELD by its socket (heartbeat alive) but has
// not renewed its task lease within wsTaskTimeout is presumed wedged — connected but
// no longer progressing — and its task is auto-released. It reuses EXACTLY the manual
// requeue machinery: it books the same short failure cooldown (so the released issue
// is not instantly re-admissible and can't recreate the #2492 dup-assign race), bumps
// the assignment generation (the Gate — so the wedged worker, if it later wakes, is
// fenced), and pushes task_revoke with an auto-expiry reason so a still-listening
// relay stops cleanly. It is deliberately CONSERVATIVE: a task that keeps reporting
// task_progress renews lastLeaseRenew every report and is therefore NEVER reclaimed,
// so "working slowly but alive" is not confused with "wedged". `now` is injected so
// tests can drive expiry deterministically.
func (h *ContributeWSHub) reclaimExpiredLeases(now time.Time) int {
	type expiredTarget struct {
		conn *ContributorConnection
		task WSTaskAssign
	}
	var targets []expiredTarget
	h.mu.RLock()
	for _, c := range h.connections {
		c.mu.Lock()
		// Only a connection actively holding a task with a started lease clock can
		// expire; a zero lastLeaseRenew means no active lease (idle or just released).
		expired := c.currentTask != nil && !c.lastLeaseRenew.IsZero() &&
			now.Sub(c.lastLeaseRenew) > wsTaskTimeout
		if expired {
			released := *c.currentTask
			c.currentTask = nil
			c.currentPrompt = ""
			c.currentLabels = nil
			c.tokenMintedAt = time.Time{}
			// #2675: clear credential state so a stale pendingToken cannot leak to the
			// now-idle connection (mirrors RequeueContributorTask cleanup).
			c.pendingToken = ""
			c.credentialDelivered = false
			c.currentTaskGen = h.nextTaskGen()
			c.lastLeaseRenew = time.Time{}
			targets = append(targets, expiredTarget{conn: c, task: released})
		}
		c.mu.Unlock()
	}
	h.mu.RUnlock()

	for _, tgt := range targets {
		// C4: the lease-TTL backstop released this task — revoke its server-issued
		// lease so the wedged worker cannot re-adopt it via a later task_progress.
		h.revokeLease(identityOf(tgt.conn), tgt.task.TaskID)
		if tgt.task.Number > 0 {
			h.recordTaskFailureForTask(&tgt.task, false)
		}
		username := ""
		if tgt.conn.profile != nil {
			username = tgt.conn.profile.GitHubUsername
		}
		h.logger.Warn("[contribute-ws] task lease expired, auto-released",
			"username", username,
			"task", tgt.task.TaskID,
			"repo", tgt.task.Repo,
			"number", tgt.task.Number,
			"lease_ttl", wsTaskTimeout.String(),
		)
		h.recordTaskDecision(username, decisionLeaseExpired, &tgt.task,
			"lease went unrenewed for "+wsTaskTimeout.String()+"; task auto-released and revoked")
		h.addActivity(username, "lease expired: auto-released", tgt.conn.role, tgt.conn.cliBackend, tgt.conn.model, tgt.conn.reasoningEffort, tgt.task.TaskID)
		if tgt.conn.ws != nil {
			_ = tgt.conn.send(WSMessage{
				Type:   "task_revoke",
				Seq:    h.nextSeq(),
				TaskID: tgt.task.TaskID,
				Reason: leaseExpiredReason,
			})
		}
	}
	return len(targets)
}
