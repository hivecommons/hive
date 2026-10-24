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

import (
	"time"

	"github.com/hivecommons/hive/crux"
)

// ConditionType identifies one axis of an agent's observed health. It is
// defined in the crux root so that crux.Process can carry watchdog
// conditions without the root importing this package.
type ConditionType = crux.ConditionType

// Condition axes; see crux.ConditionReady and friends.
const (
	ConditionReady         = crux.ConditionReady
	ConditionAuthenticated = crux.ConditionAuthenticated
	ConditionProducing     = crux.ConditionProducing
)

// ConditionStatus is the tri-state verdict of a condition; see
// crux.ConditionStatus.
type ConditionStatus = crux.ConditionStatus

// Condition statuses.
const (
	ConditionTrue    = crux.ConditionTrue
	ConditionFalse   = crux.ConditionFalse
	ConditionUnknown = crux.ConditionUnknown
)

// Condition is one observed-health axis for one agent; see crux.Condition.
type Condition = crux.Condition

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
