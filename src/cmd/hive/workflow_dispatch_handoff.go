package main

import (
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// Whether a recorded hand-off started, eval-cycle wiring
// (hivecommons/hive#18749). scheduler.ReviewWorkflowHandoff decides; this
// file gathers what it decides on (the agent's mode and capabilities from
// the config, how the agent's kicked turn ended from the agent manager) and
// acts on the verdict: the record is rewritten, an issue claim the agent
// took for a hand-off it did not start is released, and the log says what
// happened and what comes next.

// workflowHandoffKickSlack is how far the agent manager's kick time may be
// from the recorded delivery time and still be the same kick: both stamp
// the same delivery, a moment apart.
const workflowHandoffKickSlack = 2 * time.Minute

// workflowHandoffLeftPasses is how many consecutive passes a pending
// hand-off's item must be missing from the dispatched items before the
// hand-off is settled as "the item left the state": one enumeration that
// came back short must not end the watch on a hand-off nobody started.
const workflowHandoffLeftPasses = 3

// workflowAgentTurn is what the agent manager knows about an agent's most
// recent kick.
type workflowAgentTurn struct {
	// LastKick is when that kick was delivered.
	LastKick time.Time
	// Refused and RefusalReason are the kick-refusal flag the pane watcher
	// raises when the model declines (the dashboard's agent_declined).
	Refused       bool
	RefusalReason string
	// Outcome is how the kicked turn ended, once it has.
	Outcome crux.KickOutcome
}

// workflowAgentTurnFrom reads an agent's turn from its process status.
func workflowAgentTurnFrom(p *crux.Process) (workflowAgentTurn, bool) {
	if p == nil || p.LastKick == nil {
		return workflowAgentTurn{}, false
	}
	return workflowAgentTurn{LastKick: *p.LastKick, Refused: p.KickRefused, RefusalReason: p.KickRefusalReason, Outcome: p.KickOutcome}, true
}

// workflowStartupDeferral is the kick-refusal reason of a kick the agent
// manager deferred while the agent was starting: no model declined anything.
const workflowStartupDeferral = "deferred until startup completes"

// workflowDispatchNeedFor is what the item's target asks of its agent.
func workflowDispatchNeedFor(d github.WorkflowDispatch) config.WorkflowDispatchNeed {
	return config.WorkflowDispatchNeed{Repo: d.Repo, Reviews: !d.Target.ProducesChange()}
}

// workflowHandoffEvidence gathers what this pass knows about the recorded
// hand-off e of item d, which is still in the state it was handed off in.
func workflowHandoffEvidence(cfg *config.Config, d github.WorkflowDispatch, e scheduler.WorkflowDispatchEntry, turn func(agent string) (workflowAgentTurn, bool), now time.Time) scheduler.WorkflowHandoffEvidence {
	need := workflowDispatchNeedFor(d)
	ev := scheduler.WorkflowHandoffEvidence{Now: now, Reviews: need.Reviews, ItemUpdatedAt: d.UpdatedAt}
	if ok, why := cfg.WorkflowDispatchAgentQualifies(e.Agent, need); !ok {
		ev.Unqualified = why
		return ev
	}
	if turn == nil {
		return ev
	}
	t, known := turn(e.Agent)
	if !known {
		return ev
	}
	switch {
	case t.LastKick.Before(e.At.Add(-workflowHandoffKickSlack)):
		// The manager's last kick predates the hand-off (a restart
		// restored older state): nothing is known about this turn.
	case t.LastKick.After(e.At.Add(workflowHandoffKickSlack)):
		// A later kick was delivered, and a kick is only delivered to an
		// agent back at its prompt: the hand-off's turn was over by then.
		ev.TurnEndedAt = t.LastKick
	default:
		// The agent's last kick is the hand-off.
		if t.Refused && !strings.Contains(t.RefusalReason, workflowStartupDeferral) {
			ev.Declined = strings.TrimSpace(t.RefusalReason)
			if ev.Declined == "" {
				ev.Declined = "the agent refused the kick"
			}
		}
		if t.Outcome.Settled(&t.LastKick) {
			ev.TurnEndedAt = t.Outcome.At
			switch t.Outcome.Kind {
			case crux.KickOutcomeStandDown, crux.KickOutcomeQuestion, crux.KickOutcomeNoOp:
				if ev.Declined == "" {
					ev.Declined = strings.TrimSpace(t.Outcome.Reason)
				}
				if ev.Declined == "" {
					ev.Declined = "the turn ended as " + t.Outcome.Kind
				}
			}
		}
	}
	return ev
}

// workflowHandoffSeenKey identifies an item in a dispatched state without
// its triggering reply, for "is the item still in that state?".
func workflowHandoffSeenKey(ref, profile, state string) string {
	return ref + "|" + profile + "|" + state
}

// reviewWorkflowHandoffs reviews every recorded, undecided hand-off whose
// item is still in the state it was handed off in, and notes which items
// are in a dispatched state this pass (for the dashboard's not-started
// list). A hand-off found not started frees the issue claim its agent took.
// complete says items is the whole set of dispatched items (the cycle
// enumerated the repositories): only then does an undecided hand-off whose
// item is absent count toward an item that left the state. absences counts
// those passes per key, so one short enumeration does not settle a hand-off
// (workflowHandoffLeftPasses); nil settles on the first.
func reviewWorkflowHandoffs(cfg *config.Config, dlog *scheduler.WorkflowDispatchLog, items []github.WorkflowDispatch, reply func(d github.WorkflowDispatch) (github.DispatchReply, bool, error), turn func(agent string) (workflowAgentTurn, bool), ledger *claims.Ledger, complete bool, absences map[string]int, now time.Time, logger *slog.Logger) map[string]bool {
	seen := map[string]bool{}
	for _, d := range items {
		seen[workflowHandoffSeenKey(d.Ref(), d.Target.Profile, d.State)] = true
	}
	undecided := map[string]bool{}
	var left []scheduler.WorkflowDispatchEntry
	for _, e := range dlog.Entries() {
		if e.Settled() {
			continue
		}
		if !seen[workflowHandoffSeenKey(e.Ref, e.Profile, e.State)] {
			if !complete {
				continue
			}
			if absences != nil {
				absences[e.Key]++
				if absences[e.Key] < workflowHandoffLeftPasses {
					continue
				}
				delete(absences, e.Key)
			}
			left = append(left, scheduler.WorkflowHandoffLeftState(e, now))
			continue
		}
		delete(absences, e.Key)
		undecided[e.Ref] = true
	}
	if err := dlog.RecordAll(left); err != nil && logger != nil {
		logger.Warn("workflow dispatch: recording finished hand-offs failed", "error", err)
	}
	reviewed := map[string]bool{}
	for _, d := range items {
		ref := d.Ref()
		sk := workflowHandoffSeenKey(ref, d.Target.Profile, d.State)
		if reviewed[sk] || !undecided[ref] {
			continue
		}
		reviewed[sk] = true
		var trigger *github.DispatchReply
		if d.Target.Reply != "" {
			if reply == nil {
				continue
			}
			r, found, err := reply(d)
			if err != nil || !found {
				continue
			}
			trigger = &r
		}
		e, ok := dlog.Get(scheduler.WorkflowDispatchKey(d, trigger))
		if !ok || e.Settled() {
			continue
		}
		next, changed := scheduler.ReviewWorkflowHandoff(e, workflowHandoffEvidence(cfg, d, e, turn, now))
		if !changed {
			continue
		}
		if err := dlog.Record(next); err != nil && logger != nil {
			logger.Warn("workflow dispatch: recording hand-off review failed", "item", ref, "error", err)
		}
		if next.Status != scheduler.WorkflowHandoffNotStarted {
			continue
		}
		releaseWorkflowDispatchClaim(cfg, ledger, d, e.Agent)
		if logger != nil {
			logger.Warn("workflow dispatch: hand-off not started", "item", ref, "profile", next.Profile, "state", next.State,
				"agent", next.Agent, "reason", next.Reason, "attempts", next.Attempts, "next", scheduler.WorkflowHandoffNext(next))
		}
	}
	return seen
}

// releaseWorkflowDispatchClaim frees the issue claim an agent took for a
// hand-off it did not start: while it stands, no other agent can be handed
// the issue. A claim someone else holds is left alone.
func releaseWorkflowDispatchClaim(cfg *config.Config, ledger *claims.Ledger, d github.WorkflowDispatch, agent string) {
	if d.Kind != workflowmodel.KindIssue || ledger == nil || agent == "" {
		return
	}
	_, _, _ = ledger.Release(qualifyClaimRepo(cfg.Project.Org, d.Repo), d.Number, agent, "", "workflow hand-off not started")
}

// noteSeen stores the items in a dispatched state as of the latest pass.
func (rt *workflowDispatchRuntime) noteSeen(seen map[string]bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.seen = seen
}

// notStarted lists the hand-offs that were not started and whose item was
// still in that state on the latest pass.
func (rt *workflowDispatchRuntime) notStarted(cfg *config.Config, logger *slog.Logger) []dashboard.WorkflowDispatchHandoff {
	out := []dashboard.WorkflowDispatchHandoff{}
	if cfg == nil {
		return out
	}
	entries := rt.dispatchLog(cfg, logger).NotStarted()
	rt.mu.Lock()
	seen := rt.seen
	rt.mu.Unlock()
	dlog := rt.dispatchLog(cfg, logger)
	now := time.Now()
	for _, e := range entries {
		if !seen[workflowHandoffSeenKey(e.Ref, e.Profile, e.State)] {
			continue
		}
		repo, number := splitWorkflowRef(e.Ref)
		h := dashboard.WorkflowDispatchHandoff{
			Ref: e.Ref, Repo: qualifyClaimRepo(cfg.Project.Org, repo), Number: number, Profile: e.Profile, State: e.State,
			Agent: e.Agent, Reason: e.Reason, At: e.At, Attempts: e.Attempts, Retrying: e.Retryable(), Next: scheduler.WorkflowHandoffNext(e),
		}
		workflowHandoffReasonOrigin(&h, e, dlog, now)
		out = append(out, h)
	}
	return out
}

// workflowDispatchHandoffs is the dashboard's WorkflowDispatchHandoffs.
func workflowDispatchHandoffs(cfg *config.Config, logger *slog.Logger) func() []dashboard.WorkflowDispatchHandoff {
	return func() []dashboard.WorkflowDispatchHandoff { return workflowDispatchRT.notStarted(cfg, logger) }
}

// splitWorkflowRef splits "owner/repo#N" (github.WorkflowDispatch.Ref).
func splitWorkflowRef(ref string) (string, int) {
	i := strings.LastIndex(ref, "#")
	if i < 0 {
		return ref, 0
	}
	n, err := strconv.Atoi(ref[i+1:])
	if err != nil {
		return ref[:i], 0
	}
	return ref[:i], n
}
