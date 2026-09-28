package spokealerts

import (
	"fmt"
	"sync"
	"time"
)

// ResumeKickHeldAlertIDPrefix prefixes the per-agent alert raised when a
// crash-restarted agent was refused its resume kick by the governor gate
// (#2573) and now idles at a fresh prompt until its next scheduled slot.
const ResumeKickHeldAlertIDPrefix = "agent-resume-kick-held:"

// ResumeKickHeldAlertID returns the alert id for agent.
func ResumeKickHeldAlertID(agent string) string {
	return ResumeKickHeldAlertIDPrefix + agent
}

// ResumeKickHeld remembers which agents currently carry a resume-kick-held
// alert and when it was raised, so the alert can be cleared once ANY kick —
// scheduled, or manual from the dashboard — reaches the agent after that
// point. Safe for concurrent use.
type ResumeKickHeld struct {
	mu     sync.Mutex
	heldAt map[string]time.Time
	now    func() time.Time
}

// NewResumeKickHeld returns an empty tracker.
func NewResumeKickHeld() *ResumeKickHeld {
	return &ResumeKickHeld{heldAt: map[string]time.Time{}, now: time.Now}
}

// Apply raises one alert per held agent. oomSuspect, when non-nil, reports
// whether the agent's last crash coincided with a cgroup OOM kill, which
// changes the remedy from "wait or kick" to "raise the memory limit".
func (r *ResumeKickHeld) Apply(alerts AlertSink, held []string, oomSuspect func(string) bool) {
	if len(held) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, agent := range held {
		oom := oomSuspect != nil && oomSuspect(agent)
		alerts.AddSystemAlert(ResumeKickHeldAlertID(agent), "warning", ResumeKickHeldMessage(agent, oom))
		if _, ok := r.heldAt[agent]; !ok {
			r.heldAt[agent] = r.now()
		}
	}
}

// ClearKicked drops the alert for every tracked agent whose last kick is
// later than the moment the alert was raised. lastKick returns the agent's
// most recent kick time and whether one is known.
func (r *ResumeKickHeld) ClearKicked(alerts AlertSink, lastKick func(string) (time.Time, bool)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for agent, since := range r.heldAt {
		t, ok := lastKick(agent)
		if !ok || !t.After(since) {
			continue
		}
		alerts.ClearSystemAlert(ResumeKickHeldAlertID(agent))
		delete(r.heldAt, agent)
	}
}

// ResumeKickHeldMessage renders the banner: what happened, why the agent is
// idle, and the fix.
func ResumeKickHeldMessage(agent string, oom bool) string {
	cause := "its CLI crashed and was restarted"
	if oom {
		cause = "its CLI was OOM-killed (container hit its memory limit) and restarted"
	}
	return fmt.Sprintf("agent %s is idle at a fresh prompt: %s, but the resume-kick gate refused an early kick (one resume kick per cadence interval) — kick it from the agent card now, or wait for its next scheduled slot%s",
		agent, cause, oomRemedy(oom))
}

func oomRemedy(oom bool) string {
	if !oom {
		return ""
	}
	return "; to stop the crashes raise the pod memory limit or reduce concurrent sub-agents"
}
