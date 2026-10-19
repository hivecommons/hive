package stageengine

import (
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels/shadowdiff"
)

// StatusExampleLimit bounds Status.LastMismatchExamples.
const StatusExampleLimit = 5

// Status is the stageEngine block of GET /api/status and the activity
// tracker's status strip (ADR-0025 §Rollout). Counts are process totals
// from the Counters; examples come from the shadow recorder's ring.
type Status struct {
	// Mode is the reconciler's current mode (off, shadow, enforce).
	Mode string `json:"mode"`
	// Derivations is stage_derivations_total summed over its labels.
	Derivations int `json:"derivations"`
	// Mismatches is stage_shadow_mismatch_total summed: shadow-mode
	// derivations where legacy and engine disagreed on the stage. The
	// shadow→enforce gate is 0 over 48 h.
	Mismatches int `json:"mismatches"`
	// Anomalies is stage_transition_anomaly_total summed: observed
	// from→to pairs the transition table does not expect.
	Anomalies int `json:"anomalies"`
	// Healed is stage_duplicate_pills_healed_total summed: writes that
	// found more than one stage/* label on an item.
	Healed int `json:"healed"`
	// LastMismatchExamples are the newest shadow mismatches, newest first,
	// at most StatusExampleLimit.
	LastMismatchExamples []StatusExample `json:"lastMismatchExamples"`
}

// StatusExample is one recorded shadow mismatch.
type StatusExample struct {
	Ref    string    `json:"ref"`
	Kind   string    `json:"kind"` // shadowdiff.KindStage or KindReason
	Legacy string    `json:"legacy"`
	Engine string    `json:"engine"`
	At     time.Time `json:"at"`
}

// StatusOf assembles a Status. r may be nil (mode reported as ""), c nil
// (DefaultCounters) and rec nil (no examples).
func StatusOf(r Reconciler, c *Counters, rec *shadowdiff.Recorder) Status {
	if c == nil {
		c = DefaultCounters()
	}
	s := Status{
		Derivations:          c.Total(MetricDerivations),
		Mismatches:           c.Total(MetricShadowMismatch),
		Anomalies:            c.Total(MetricAnomaly),
		Healed:               c.Total(MetricDuplicateHealed),
		LastMismatchExamples: []StatusExample{},
	}
	if sw, ok := r.(ModeSwitch); ok && sw != nil {
		s.Mode = sw.Mode().String()
	}
	if rec == nil {
		return s
	}
	snap := rec.Snapshot()
	for i := len(snap.Examples) - 1; i >= 0 && len(s.LastMismatchExamples) < StatusExampleLimit; i-- {
		ex := snap.Examples[i]
		legacy, engine := ex.Legacy.Stage.String(), ex.Engine.Stage.String()
		if ex.Kind == shadowdiff.KindReason {
			legacy, engine = ex.Legacy.Reason, ex.Engine.Reason
		}
		s.LastMismatchExamples = append(s.LastMismatchExamples, StatusExample{Ref: ex.Key, Kind: ex.Kind, Legacy: legacy, Engine: engine, At: ex.At})
	}
	return s
}
