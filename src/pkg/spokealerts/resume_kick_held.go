package spokealerts

import (
	"fmt"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/governor"
)

// ResumeKickHeldAlertIDPrefix prefixes the per-agent alert raised when a
// crash-restarted agent was refused its resume kick by the governor gate
// (#2573) and now idles at a fresh prompt until its next scheduled slot.
const ResumeKickHeldAlertIDPrefix = "agent-resume-kick-held:"

// ResumeKickHeldMaxAgeIntervals bounds how long a banner may live, as a
// multiple of the agent's cadence interval (#9612). An interval-throttled
// agent's scheduled kick lands within ONE interval, so a banner still up after
// two has lost its clearing event and is dropped as a backstop.
const ResumeKickHeldMaxAgeIntervals = 2

// ResumeKickHeldFallbackMaxAge is the banner backstop when the agent's cadence
// interval is unknown (no interval cadence in the current mode).
const ResumeKickHeldFallbackMaxAge = 6 * time.Hour

// ResumeKickHeldAlertID returns the alert id for agent.
func ResumeKickHeldAlertID(agent string) string {
	return ResumeKickHeldAlertIDPrefix + agent
}

// ResumeKickHeldAgent is one restarted agent the resume-kick gate refused,
// with the gate's reason.
type ResumeKickHeldAgent struct {
	Agent  string
	Reason governor.ResumeRefusal
}

// ResumeKickHeldRaisesBanner reports whether a refusal for reason warrants the
// per-agent banner (#9612). Only an interval throttle does: the agent is
// expected to work and its next scheduled kick is coming, so the operator may
// want to kick it early. Every other reason means the agent is idle by
// configuration (paused or unscheduled in this mode, on-demand, not interval
// scheduled) or by budget, which the budget-exhausted banner already covers
// fleet-wide (BudgetExhaustedAlertID).
func ResumeKickHeldRaisesBanner(reason governor.ResumeRefusal) bool {
	return reason == governor.ResumeRefusalIntervalThrottle
}

// ResumeKickHeldFacts are the per-cycle observations Reconcile clears
// against. Each is a seam so the clear rules are unit-testable; a nil func is
// treated as "no signal".
type ResumeKickHeldFacts struct {
	// LastKick returns the agent's most recent kick time and whether one is
	// known.
	LastKick func(agent string) (time.Time, bool)
	// Inactive reports that the agent is operator-paused, disabled, or no
	// longer in the roster: no kick is coming and none is wanted.
	Inactive func(agent string) bool
	// WorkedSince reports that the agent's session has been busy or working
	// at some point after since.
	WorkedSince func(agent string, since time.Time) bool
	// Gate returns the resume-kick gate's CURRENT verdict for the agent
	// (without recording a grant) and its cadence interval (0 when unknown).
	Gate func(agent string) (governor.ResumeRefusal, time.Duration)
}

// ResumeKickHeld remembers which agents currently carry a resume-kick-held
// alert and when it was raised, so the alert can be cleared once it no longer
// describes a problem: any kick reached the agent, the agent is no longer
// expected to run, it is working again, or the banner outlived its max age
// (#9612). Safe for concurrent use.
type ResumeKickHeld struct {
	mu     sync.Mutex
	heldAt map[string]time.Time
	now    func() time.Time
}

// NewResumeKickHeld returns an empty tracker.
func NewResumeKickHeld() *ResumeKickHeld {
	return &ResumeKickHeld{heldAt: map[string]time.Time{}, now: time.Now}
}

// Apply raises one alert per held agent whose refusal reason warrants it
// (ResumeKickHeldRaisesBanner). A held agent refused for any other reason gets
// no banner, and an existing banner for it is cleared: the agent is now idle
// by configuration or budget, not waiting on a slot. oomSuspect, when non-nil,
// reports whether the agent's last crash coincided with a cgroup OOM kill,
// which adds the "raise the memory limit" remedy.
func (r *ResumeKickHeld) Apply(alerts AlertSink, held []ResumeKickHeldAgent, oomSuspect func(string) bool) {
	if len(held) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range held {
		if !ResumeKickHeldRaisesBanner(h.Reason) {
			r.clearLocked(alerts, h.Agent)
			continue
		}
		oom := oomSuspect != nil && oomSuspect(h.Agent)
		alerts.AddSystemAlert(ResumeKickHeldAlertID(h.Agent), "warning", ResumeKickHeldMessage(h.Agent, h.Reason, oom))
		if _, ok := r.heldAt[h.Agent]; !ok {
			r.heldAt[h.Agent] = r.now()
		}
	}
}

// Reconcile runs every eval cycle and drops the alert for each tracked agent
// when ANY of these holds (#9612):
//
//   - a kick (scheduled, or manual from the dashboard) reached it after the
//     alert was raised;
//   - it is operator-paused, disabled, or no longer in the roster;
//   - its session was busy or working after the alert was raised;
//   - the gate would now refuse it for a reason other than the interval
//     throttle (mode changed to one that pauses or does not schedule it, it
//     became on-demand, or the budget ran out), so no kick is coming;
//   - the alert is older than ResumeKickHeldMaxAgeIntervals cadence
//     intervals (ResumeKickHeldFallbackMaxAge when the interval is unknown),
//     so no banner can outlive a lost clearing event.
func (r *ResumeKickHeld) Reconcile(alerts AlertSink, facts ResumeKickHeldFacts) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for agent, since := range r.heldAt {
		if resumeKickHeldShouldClear(agent, since, now, facts) {
			r.clearLocked(alerts, agent)
		}
	}
}

// ClearKicked drops the alert for every tracked agent whose last kick is
// later than the moment the alert was raised. It is Reconcile with only the
// kick fact; kept for callers that have nothing else to offer.
func (r *ResumeKickHeld) ClearKicked(alerts AlertSink, lastKick func(string) (time.Time, bool)) {
	r.Reconcile(alerts, ResumeKickHeldFacts{LastKick: lastKick})
}

// clearLocked drops agent's alert if one is tracked. Caller holds r.mu.
func (r *ResumeKickHeld) clearLocked(alerts AlertSink, agent string) {
	if _, ok := r.heldAt[agent]; !ok {
		return
	}
	alerts.ClearSystemAlert(ResumeKickHeldAlertID(agent))
	delete(r.heldAt, agent)
}

func resumeKickHeldShouldClear(agent string, since, now time.Time, facts ResumeKickHeldFacts) bool {
	if facts.LastKick != nil {
		if t, ok := facts.LastKick(agent); ok && t.After(since) {
			return true
		}
	}
	if facts.Inactive != nil && facts.Inactive(agent) {
		return true
	}
	if facts.WorkedSince != nil && facts.WorkedSince(agent, since) {
		return true
	}
	var interval time.Duration
	if facts.Gate != nil {
		var reason governor.ResumeRefusal
		reason, interval = facts.Gate(agent)
		if reason != governor.ResumeAllowed && !ResumeKickHeldRaisesBanner(reason) {
			return true
		}
	}
	return now.Sub(since) >= ResumeKickHeldMaxAge(interval)
}

// ResumeKickHeldMaxAge is the banner backstop for an agent whose cadence
// interval is interval: ResumeKickHeldMaxAgeIntervals intervals, or
// ResumeKickHeldFallbackMaxAge when the interval is unknown (<= 0).
func ResumeKickHeldMaxAge(interval time.Duration) time.Duration {
	if interval <= 0 {
		return ResumeKickHeldFallbackMaxAge
	}
	return ResumeKickHeldMaxAgeIntervals * interval
}

// ResumeKickHeldMessage renders the banner: what happened, why the agent is
// idle (the gate's actual refusal reason), and the fix. Only an interval
// throttle has a "next scheduled slot" to wait for.
func ResumeKickHeldMessage(agent string, reason governor.ResumeRefusal, oom bool) string {
	cause := "its CLI crashed and was restarted"
	if oom {
		cause = "its CLI was OOM-killed (container hit its memory limit) and restarted"
	}
	return fmt.Sprintf("agent %s is idle at a fresh prompt: %s, but %s%s",
		agent, cause, resumeRefusalExplanation(reason), oomRemedy(oom))
}

// resumeRefusalExplanation names the gate's reason and the matching remedy.
func resumeRefusalExplanation(reason governor.ResumeRefusal) string {
	switch reason {
	case governor.ResumeRefusalIntervalThrottle:
		return "the resume-kick gate refused an early kick (one resume kick per cadence interval) - kick it from the agent card now, or wait for its next scheduled slot"
	case governor.ResumeRefusalPausedInMode:
		return "its cadence is paused in the current mode, so it is idle by configuration - kick it from the agent card if you want it to work now"
	case governor.ResumeRefusalUnscheduled:
		return "it has no cadence in the current mode, so the governor will not kick it - set a cadence on the agent card or kick it now"
	case governor.ResumeRefusalNotInterval:
		return "its cadence is not interval-based, so it gets no early resume kick - it runs at its next scheduled time, or kick it from the agent card now"
	case governor.ResumeRefusalOnDemand:
		return "it is on-demand and is never timer-kicked - kick it from the agent card when there is work for it"
	case governor.ResumeRefusalBudgetExhausted:
		return "the weekly token budget is exhausted, so kicks are suspended - raise the budget or exempt the agent, or kick it from the agent card"
	default:
		return fmt.Sprintf("the resume-kick gate refused an early kick (%s) - kick it from the agent card", reason)
	}
}

func oomRemedy(oom bool) string {
	if !oom {
		return ""
	}
	return "; to stop the crashes raise the pod memory limit or reduce concurrent sub-agents"
}
