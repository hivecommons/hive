package stageengine

// Status is the stageEngine block of GET /api/status and the activity
// tracker's status strip (ADR-0025 §Rollout). Counts are process totals
// from the Counters.
type Status struct {
	// Mode is the reconciler's current mode (shadow, enforce).
	Mode string `json:"mode"`
	// Derivations is stage_derivations_total summed over its labels.
	Derivations int `json:"derivations"`
	// Anomalies is stage_transition_anomaly_total summed: observed
	// from→to pairs the transition table does not expect.
	Anomalies int `json:"anomalies"`
	// Healed is stage_duplicate_pills_healed_total summed: writes that
	// found more than one stage/* label on an item.
	Healed int `json:"healed"`
}

// StatusOf assembles a Status. r may be nil (mode reported as "") and c
// nil (DefaultCounters).
func StatusOf(r Reconciler, c *Counters) Status {
	if c == nil {
		c = DefaultCounters()
	}
	s := Status{
		Derivations: c.Total(MetricDerivations),
		Anomalies:   c.Total(MetricAnomaly),
		Healed:      c.Total(MetricDuplicateHealed),
	}
	if sw, ok := r.(ModeSwitch); ok && sw != nil {
		s.Mode = sw.Mode().String()
	}
	return s
}
