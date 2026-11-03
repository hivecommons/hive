package crux

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
