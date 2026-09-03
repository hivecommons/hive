package planning

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

// This file implements Phase 3 bounded replanning and the eval-loop lane that
// drives it. The pure stall detector lives in stall.go; here we turn a detected
// stall into a bounded, reversible action:
//
//   - under the cap: re-kick the architect to revise the stalled plan, record
//     the replan (replan_count++, last_replan_at=now), and audit/alert/notify;
//   - at the cap: stop replanning and raise a "needs human" escalation.
//
// SAFETY (launch-path mutex): this lane runs on the governor eval tick, exactly
// like the existing kick machinery. It NEVER touches agent.Manager's internal
// mutex — it drives the architect only through the Kicker interface (backed by
// the manager's already-lock-safe SendKick, the same call runEvalCycle uses).
// Run takes a context.Context; the caller gates it with Due() so it is a plain
// synchronous call on the tick, adding NO new goroutine.

// architectAgent is the agent name the planner/replanner drives. It matches the
// architect lane used by Phase 1 decomposition.
const architectAgent = "architect"

// Kicker drives an agent out-of-band. The agent manager satisfies it via
// SendKick, which takes the manager lock ITSELF and is called from the governor
// tick — never from the agent-launch path — so no re-entrant lock is possible.
// Keeping this an interface also keeps pkg/planning free of an import cycle and
// unit-testable with a fake.
type Kicker interface {
	// Kick delivers message to the named agent. It must be safe to call from
	// the governor goroutine.
	Kick(agent, message string) error
}

// KickRecorder records that an agent was kicked (for governor kick history /
// cadence accounting). The governor satisfies it via RecordKick.
type KickRecorder interface {
	RecordKick(agent string)
}

// Sink records the replan lane's decisions through the same channels the
// trajectory/budget alerts use (audit log, system alert, notification). The
// dashboard implements it. All methods must be safe to call from the governor
// goroutine, and any may be a no-op if the underlying sink is absent.
type Sink interface {
	Audit(actor, action, detail, agent string)
	Alert(id, severity, message string)
	Notify(title, message string)
}

// ReplanLaneConfig configures the replan lane.
type ReplanLaneConfig struct {
	// IntervalS is the minimum seconds between lane runs. The governor tick
	// gates on Due(), so this floors the cadence at (but never below) the tick.
	// Zero means DefaultReplanIntervalS.
	IntervalS int
	// Stall tunes the stall detector (threshold + replan cap).
	Stall StallConfig
}

// DefaultReplanIntervalS is the default replan-lane cadence: 30 minutes. Stall
// detection is expensive-ish (it re-kicks agents) and stalls evolve slowly, so
// it runs far less often than the governor tick.
const DefaultReplanIntervalS = 30 * 60

// replanAlertPrefix namespaces the lane's dashboard system alerts by epic.
const replanAlertPrefix = "plan-stall-"

// ReplanLane runs periodic stall detection + bounded replanning over every bead
// store. It mirrors trajectory.Lane's shape: a Due()-gated, synchronous Run the
// governor tick calls; no goroutine of its own.
type ReplanLane struct {
	stores   map[string]*beads.Store
	kicker   Kicker
	recorder KickRecorder
	sink     Sink
	logger   *slog.Logger
	interval time.Duration
	stallCfg StallConfig
	lastRun  time.Time
}

// NewReplanLane builds the lane. stores is the same per-agent store map the
// dashboard/governor use. kicker and recorder drive/record the architect
// re-kick; either may be nil (the lane then only detects + accounts + alerts,
// without kicking — useful in tests and safe by construction). sink may be nil
// (decisions are then only logged).
func NewReplanLane(stores map[string]*beads.Store, kicker Kicker, recorder KickRecorder, sink Sink, cfg ReplanLaneConfig, logger *slog.Logger) *ReplanLane {
	intervalS := cfg.IntervalS
	if intervalS <= 0 {
		intervalS = DefaultReplanIntervalS
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ReplanLane{
		stores:   stores,
		kicker:   kicker,
		recorder: recorder,
		sink:     sink,
		logger:   logger,
		interval: time.Duration(intervalS) * time.Second,
		stallCfg: cfg.Stall,
	}
}

// Due reports whether enough time has elapsed since the last run for the lane to
// run again at time now. The governor tick gates on this so the lane's cadence
// is independent of, but floored by, the governor interval — exactly like
// trajectory.Lane.Due.
func (l *ReplanLane) Due(now time.Time) bool {
	return l.lastRun.IsZero() || now.Sub(l.lastRun) >= l.interval
}

// Run detects stalled plans across every store once and acts on each, bounded by
// the replan cap. It is synchronous and adds no goroutine; the caller runs it
// from the governor tick after Due() returns true. Returns the number of replans
// performed (for logging/tests).
func (l *ReplanLane) Run(ctx context.Context) int {
	l.lastRun = time.Now()
	now := l.lastRun
	replans := 0
	for storeName, store := range l.stores {
		snap := SnapshotFromStore(store)
		stalled := DetectStalledPlans(snap, now, l.stallCfg)
		for _, sp := range stalled {
			if ctx.Err() != nil {
				return replans // shutting down; stop cleanly
			}
			if l.handleStall(store, storeName, sp, now) {
				replans++
			}
		}
	}
	return replans
}

// handleStall applies the bounded-replan decision to one stalled plan. Returns
// true when a replan was performed (as opposed to a cap escalation or a
// bookkeeping failure).
func (l *ReplanLane) handleStall(store *beads.Store, storeName string, sp StalledPlan, now time.Time) bool {
	alertID := replanAlertPrefix + sp.EpicID

	if sp.CapReached {
		// Cap exhausted — stop replanning, escalate to a human. Reversible: a
		// human can edit the plan and RejectPlan/ApprovePlan to reset the loop;
		// the cap does not touch bead state.
		msg := fmt.Sprintf("plan %q stalled for %s and hit the replan cap (%d) — needs human review",
			sp.EpicTitle, roundDur(sp.StalledFor), l.stallCfg.maxReplans())
		l.emit(alertID, "error", "replan-cap", msg, sp.EpicID)
		l.logger.Warn("audit: plan replan cap reached",
			"store", storeName, "epic", sp.EpicID, "replan_count", sp.ReplanCount, "stalled_for", sp.StalledFor.String())
		return false
	}

	// Under the cap — record the replan first (increment count + stamp time) so
	// the accounting is durable even if the kick fails. This is the minimal,
	// reversible mutation: two metadata writes on the epic.
	next := sp.ReplanCount + 1
	if err := store.SetMetadata(sp.EpicID, MetaReplanCount, strconv.Itoa(next)); err != nil {
		l.logger.Warn("replan: bumping replan_count failed", "epic", sp.EpicID, "err", err.Error())
		return false
	}
	if err := store.SetMetadata(sp.EpicID, MetaLastReplanAt, now.UTC().Format(time.RFC3339)); err != nil {
		l.logger.Warn("replan: stamping last_replan_at failed", "epic", sp.EpicID, "err", err.Error())
		// Non-fatal: the count is already bumped; continue to kick + audit.
	}

	// Re-kick the architect to revise the stalled plan. This is the safe,
	// minimal first cut: a stateless kick with a "this plan stalled, revise the
	// approach" prompt naming the unfinished children, reusing the existing kick
	// machinery. It does NOT synchronously drive tmux and never touches the
	// manager lock from the launch path — SendKick (behind Kicker) takes its own
	// lock from this governor-tick call, exactly like every other kick.
	if l.kicker != nil {
		if err := l.kicker.Kick(architectAgent, buildReplanPrompt(sp)); err != nil {
			l.logger.Warn("replan: architect kick failed", "epic", sp.EpicID, "err", err.Error())
			// Count is already bumped (avoids thrashing a failing kick); surface
			// the failure but still treat this cycle as a replan attempt.
		} else if l.recorder != nil {
			l.recorder.RecordKick(architectAgent)
		}
	}

	msg := fmt.Sprintf("plan %q stalled for %s with %d unfinished task(s) — re-kicked architect to revise (replan %d/%d)",
		sp.EpicTitle, roundDur(sp.StalledFor), len(sp.Remaining), next, l.stallCfg.maxReplans())
	l.emit(alertID, "warning", "replan", msg, sp.EpicID)
	l.logger.Info("audit: plan replanned",
		"store", storeName, "epic", sp.EpicID, "replan_count", next, "remaining", len(sp.Remaining))
	return true
}

// emit fans a decision out to audit log, system alert, and notification through
// the sink (if configured).
func (l *ReplanLane) emit(alertID, severity, action, message, epicID string) {
	if l.sink == nil {
		return
	}
	l.sink.Audit("planning", action, message, architectAgent)
	l.sink.Alert(alertID, severity, message)
	l.sink.Notify("Plan "+action, message)
	_ = epicID
}

// buildReplanPrompt renders the "revise the stalled approach" prompt for the
// architect, naming the unfinished children so it can re-plan the remaining
// work rather than starting over.
func buildReplanPrompt(sp StalledPlan) string {
	msg := "[agent:architect]\n" +
		"A plan you decomposed has STALLED — its sub-tasks have not progressed for " +
		roundDur(sp.StalledFor).String() + ".\n\n" +
		"EPIC: " + sp.EpicTitle + " (" + sp.EpicID + ")\n" +
		"UNFINISHED SUB-TASKS:\n"
	for _, c := range sp.Remaining {
		msg += "  - [" + string(c.Status) + "] " + c.Title + "\n"
	}
	msg += "\nRevise the approach for the UNFINISHED work only: identify the blocker, " +
		"and either break the stuck task(s) down further or propose an alternative path. " +
		"Do NOT re-plan the completed sub-tasks. Output only the revised plan.\n"
	return msg
}

// roundDur rounds a duration to the minute for human-readable messages.
func roundDur(d time.Duration) time.Duration {
	if d < time.Minute {
		return d.Round(time.Second)
	}
	return d.Round(time.Minute)
}
