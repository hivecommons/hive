// Package watchdog implements the per-agent self-healing reconciler of RFC
// #4665: a Kubernetes-style liveness/readiness loop that observes the truth
// (the tmux pane, the credential source, production evidence) instead of
// trusting the `state=running` config echo, restarts dead agents with
// exponential backoff, escalates crash loops, and publishes k8s-style status
// conditions the dashboard can render.
//
// The package deliberately owns NO tmux/exec machinery of its own. Everything
// it observes and every action it takes goes through the Fleet interface,
// implemented by the agent manager — the same pane capture, restart, and
// pause paths the rest of the hive already uses (see pkg/agent's watchdog
// fleet adapter). This is what keeps the reconciler from becoming a second,
// uncoordinated control loop.
package watchdog

import "time"

// ConditionType identifies one axis of an agent's observed health, mirroring
// Kubernetes' status.conditions vocabulary.
type ConditionType string

const (
	// ConditionReady reports the liveness verdict: the agent's CLI is at its
	// ready signature and able to accept work.
	ConditionReady ConditionType = "Ready"
	// ConditionAuthenticated reports the credential verdict: the provider
	// credential behind the agent's backend is alive, independent of whether
	// the CLI currently looks healthy (a dead refresh token can hide behind a
	// live-looking pane for hours — failure mode 1 of RFC #4665).
	ConditionAuthenticated ConditionType = "Authenticated"
	// ConditionProducing reports the readiness verdict: the agent shows
	// evidence of recent output (state-file activity), catching the
	// idle-but-alive class invisible to liveness alone.
	ConditionProducing ConditionType = "Producing"
)

// ConditionStatus is the tri-state verdict of a condition. Unknown is a
// first-class value, never collapsed into True: a probe that could not
// determine the answer says so honestly (unknown ≠ healthy).
type ConditionStatus string

const (
	ConditionTrue    ConditionStatus = "True"
	ConditionFalse   ConditionStatus = "False"
	ConditionUnknown ConditionStatus = "Unknown"
)

// Condition is one observed-health axis for one agent, k8s-shaped so the
// dashboard (and any operator tooling) can consume it without translation.
type Condition struct {
	Type               ConditionType   `json:"type"`
	Status             ConditionStatus `json:"status"`
	Reason             string          `json:"reason,omitempty"`
	Message            string          `json:"message,omitempty"`
	LastTransitionTime time.Time       `json:"lastTransitionTime"`
}

// setCondition updates (or inserts) the condition of the given type in conds,
// preserving LastTransitionTime when the status did not change — the k8s
// semantics that make "Ready=False since 14:02" a meaningful statement.
// Reason/Message always track the latest observation.
func setCondition(conds []Condition, c Condition, now time.Time) []Condition {
	for i := range conds {
		if conds[i].Type != c.Type {
			continue
		}
		if conds[i].Status == c.Status {
			c.LastTransitionTime = conds[i].LastTransitionTime
		} else {
			c.LastTransitionTime = now
		}
		conds[i] = c
		return conds
	}
	c.LastTransitionTime = now
	return append(conds, c)
}

// FindCondition returns the condition of the given type, or a zero Condition
// with ok=false when the agent has never been probed on that axis.
func FindCondition(conds []Condition, t ConditionType) (Condition, bool) {
	for _, c := range conds {
		if c.Type == t {
			return c, true
		}
	}
	return Condition{}, false
}
