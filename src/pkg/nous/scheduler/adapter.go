package scheduler

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/hivecommons/hive/pkg/hivestate"
	"github.com/hivecommons/hive/pkg/nous"
)

// HiveSeries is one hive's snapshot series, e.g. hivestate.Load(SnapshotDir, since).
// Snapshots carry no hive identity, so the caller attributes each series.
type HiveSeries struct {
	HiveID  string
	Buckets []hivestate.Bucket
}

// Bound is an absolute fast-fail guardrail on one registered metric.
type Bound struct {
	Metric       string
	Lower, Upper *float64
}

// Plan is the scheduler view of a persisted nous.Experiment.
type Plan struct {
	Registration Registration
	// Guardrails keep the model's absolute bounds; they are not converted to
	// Metric.MaxIncrease, whose semantics remain a delta over baseline.
	Guardrails []Bound
	metrics    []nous.Metric
}

// PlanFor maps a validated experiment to a Registration. Windows start at
// CreatedAt and use the pre-registered hours. Each nous.Metric becomes a
// Metric named by its ID; DirectionDecrease sets LowerIsBetter and
// MinimumDelta (or 0) becomes RequiredDelta. Metrics whose definition is
// unavailable are still registered: SamplesFor never supplies a value for
// them, so they end as missing evidence, never zero.
func PlanFor(e nous.Experiment) (Plan, error) {
	if err := e.Validate(); err != nil {
		return Plan{}, fmt.Errorf("experiment %q: %w", e.ID, err)
	}
	p := Plan{
		Registration: Registration{
			Start:      e.CreatedAt,
			Baseline:   time.Duration(e.Window.BaselineHours) * time.Hour,
			Experiment: time.Duration(e.Window.ExperimentDurationHours) * time.Hour,
			Cooldown:   time.Duration(e.Window.CooldownHours) * time.Hour,
		},
		metrics: append([]nous.Metric(nil), e.Metrics...),
	}
	for _, m := range e.Metrics {
		sm := Metric{Name: m.ID, LowerIsBetter: m.Direction == nous.DirectionDecrease}
		if m.MinimumDelta != nil {
			sm.RequiredDelta = *m.MinimumDelta
		}
		p.Registration.Metrics = append(p.Registration.Metrics, sm)
	}
	for _, g := range e.Guardrails {
		p.Guardrails = append(p.Guardrails, Bound{Metric: g.MetricID, Lower: g.Lower, Upper: g.Upper})
	}
	return p, nil
}

// SamplesFor builds samples from the snapshot series of opted-in treatment
// cohort hives only; control, unassigned and unattributed hives are dropped.
// Each bucket yields one sample at its Start. queue_depth_p90 is the
// nearest-rank p90 of the bucket's registered queue field and
// sla_violation_rate is violated samples / samples; Evaluate then averages the
// per-bucket values. Unavailable definitions never produce a value.
func SamplesFor(e nous.Experiment, series []HiveSeries) []Sample {
	treatment := map[string]bool{}
	for _, c := range e.Cohorts {
		if c.Role != nous.RoleTreatment || !c.OptIn {
			continue
		}
		for _, id := range c.HiveIDs {
			treatment[id] = true
		}
	}
	var out []Sample
	for _, s := range series {
		if !treatment[s.HiveID] {
			continue
		}
		for _, b := range s.Buckets {
			if b.Samples <= 0 {
				continue
			}
			values := map[string]float64{}
			for _, m := range e.Metrics {
				if v, ok := bucketValue(m, b); ok {
					values[m.ID] = v
				}
			}
			out = append(out, Sample{At: b.Start, Values: values})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

func bucketValue(m nous.Metric, b hivestate.Bucket) (float64, bool) {
	switch m.DefinitionID {
	case "queue_depth_p90":
		var vals []int
		switch m.SourceField {
		case "queue_issues":
			vals = b.QueueIssues
		case "queue_prs":
			vals = b.QueuePRs
		case "queue_hold":
			vals = b.QueueHold
		}
		if len(vals) == 0 {
			return 0, false
		}
		return float64(p90(vals)), true
	case "sla_violation_rate":
		if m.SourceField != "sla_violations" {
			return 0, false
		}
		return float64(b.SLAViolatedSamples) / float64(b.Samples), true
	}
	return 0, false
}

func p90(vals []int) int {
	s := append([]int(nil), vals...)
	sort.Ints(s)
	i := int(math.Ceil(0.9*float64(len(s)))) - 1
	if i < 0 {
		i = 0
	}
	return s[i]
}

// EvaluateExperiment maps e and the hive snapshot series and evaluates them.
//
// Guardrail semantics: Evaluate's MaxIncrease (a delta over baseline) is left
// unchanged. The model's Guardrail Lower/Upper are absolute bounds and are
// enforced here: any treatment-window sample of a guarded metric strictly
// outside its bounds latches a fast-fail with Revert, overriding judgement just
// as Evaluate's own fast-fail does. Missing values never breach a bound.
//
// Judgement: a metric with MinimumDelta must improve by at least that amount
// in its direction; a metric with Threshold must have a treatment mean at or
// beyond it (>= for increase, <= for decrease). A threshold-only metric has no
// delta requirement. Missing evidence still leaves the decision unjudged.
func EvaluateExperiment(enabled bool, mode string, now time.Time, e nous.Experiment, series []HiveSeries) (Decision, error) {
	if !enabled || mode != "background" {
		return Decision{Phase: Disabled}, nil
	}
	p, err := PlanFor(e)
	if err != nil {
		return Decision{}, err
	}
	samples := SamplesFor(e, series)
	d, err := Evaluate(enabled, mode, now, p.Registration, samples)
	if err != nil || d.FastFail {
		return d, err
	}
	r := p.Registration
	baselineEnd := r.Start.Add(r.Baseline)
	experimentEnd := baselineEnd.Add(r.Experiment)
	if now.Before(baselineEnd) {
		return d, nil
	}
	if name, breached := breach(p.Guardrails, samples, baselineEnd, experimentEnd, now); breached {
		return Decision{Phase: d.Phase, Revert: true, FastFail: true, Reason: "fast-fail: guardrail " + name}, nil
	}
	if !d.Judged {
		return d, nil
	}
	baseline := means(samples, r.Start, baselineEnd, now)
	treatment := means(samples, baselineEnd, experimentEnd, now)
	d.Passed = true
	for _, m := range p.metrics {
		t := treatment[m.ID]
		delta := t - baseline[m.ID]
		if m.Direction == nous.DirectionDecrease {
			delta = -delta
		}
		if m.MinimumDelta != nil && delta < *m.MinimumDelta {
			d.Passed = false
		}
		if m.Threshold != nil {
			if (m.Direction == nous.DirectionDecrease && t > *m.Threshold) || (m.Direction != nous.DirectionDecrease && t < *m.Threshold) {
				d.Passed = false
			}
		}
	}
	return d, nil
}

func breach(bounds []Bound, samples []Sample, start, end, now time.Time) (string, bool) {
	for _, g := range bounds {
		for _, s := range samples {
			if s.At.Before(start) || !s.At.Before(end) || s.At.After(now) {
				continue
			}
			v, ok := s.Values[g.Metric]
			if !ok || !finite(v) {
				continue
			}
			if (g.Lower != nil && v < *g.Lower) || (g.Upper != nil && v > *g.Upper) {
				return g.Metric, true
			}
		}
	}
	return "", false
}
