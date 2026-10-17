// Package scheduler implements background-only Hive-self experiment decisions.
// It deliberately has no dependency on the dashboard's repo-scoped Nous state.
// Store, snapshot, heartbeat and relay adapters must persist and execute these
// decisions; this package neither changes governor configuration nor starts jobs.
package scheduler

import (
	"fmt"
	"math"
	"time"
)

type Phase string

const (
	Disabled   Phase = "disabled"
	Baseline   Phase = "baseline"
	Experiment Phase = "experiment"
	Cooldown   Phase = "cooldown"
	Complete   Phase = "complete"
)

// Registration is immutable once baseline collection begins. Durations are
// supplied by the persisted registration, never inferred from observed results.
type Registration struct {
	Start                          time.Time
	Baseline, Experiment, Cooldown time.Duration
	// Metrics must be pre-registered; positive deltas mean an increase relative
	// to baseline. LowerIsBetter reverses the success comparison only.
	Metrics []Metric
}

type Metric struct {
	Name          string
	LowerIsBetter bool
	RequiredDelta float64
	// MaxIncrease is an optional absolute increase over baseline that triggers
	// fast-fail. It is independent of the desired direction of improvement.
	MaxIncrease *float64
}

// Sample is an aggregate snapshot measurement for the already-matched cohort.
// Cohort assignment and opt-in validation belong to the adapter; observations
// from unassigned hives must not be supplied here. Missing metrics stay missing.
type Sample struct {
	At     time.Time
	Values map[string]float64
}

type Decision struct {
	Phase Phase
	// Revert asks the flag/channel adapter to restore the baseline assignment.
	// This is also required at the end of a successful experiment, before cooldown.
	Revert   bool
	FastFail bool
	// Judged is false until cooldown has ended and every metric has evidence.
	Judged bool
	Passed bool
	Reason string
}

// Evaluate is deterministic and side-effect free. The caller must durably apply
// a Revert before reporting a result; retries must not re-enable the treatment.
// Fast-fail decisions must be latched in the experiment store by the caller.
func Evaluate(enabled bool, mode string, now time.Time, r Registration, samples []Sample) (Decision, error) {
	if !enabled || mode != "background" {
		return Decision{Phase: Disabled}, nil
	}
	if err := validate(r); err != nil {
		return Decision{}, err
	}
	baselineEnd := r.Start.Add(r.Baseline)
	experimentEnd := baselineEnd.Add(r.Experiment)
	cooldownEnd := experimentEnd.Add(r.Cooldown)
	phase := Baseline
	switch {
	case !now.Before(cooldownEnd):
		phase = Complete
	case !now.Before(experimentEnd):
		phase = Cooldown
	case !now.Before(baselineEnd):
		phase = Experiment
	}
	d := Decision{Phase: phase, Revert: phase == Cooldown || phase == Complete}
	if now.Before(baselineEnd) {
		return d, nil
	}
	// Use the same fixed half-open baseline window throughout the experiment. Current
	// observations cannot contaminate the baseline and future snapshots are ignored.
	baseline := means(samples, r.Start, baselineEnd, now)
	treatment := means(samples, baselineEnd, experimentEnd, now)
	for _, m := range r.Metrics {
		b, ok := baseline[m.Name]
		if !ok || m.MaxIncrease == nil {
			continue
		}
		for _, s := range samples {
			if s.At.Before(baselineEnd) || !s.At.Before(experimentEnd) || s.At.After(now) {
				continue
			}
			v, ok := s.Values[m.Name]
			if ok && finite(v) && v-b > *m.MaxIncrease {
				return Decision{Phase: phase, Revert: true, FastFail: true, Reason: "fast-fail: " + m.Name}, nil
			}
		}
	}
	if phase != Complete {
		return d, nil
	}
	d.Passed = true
	for _, m := range r.Metrics {
		b, bok := baseline[m.Name]
		t, tok := treatment[m.Name]
		if !bok || !tok {
			d.Passed = false
			d.Reason = "missing evidence: " + m.Name
			return d, nil
		}
		delta := t - b
		if !finite(delta) {
			d.Passed = false
			d.Reason = "invalid delta: " + m.Name
			return d, nil
		}
		if m.LowerIsBetter {
			delta = -delta
		}
		if delta < m.RequiredDelta {
			d.Passed = false
		}
	}
	d.Judged = true
	return d, nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func validate(r Registration) error {
	if r.Start.IsZero() || r.Baseline <= 0 || r.Experiment <= 0 || r.Cooldown < 0 || len(r.Metrics) == 0 {
		return fmt.Errorf("invalid experiment windows or empty metrics")
	}
	seen := make(map[string]bool)
	for _, m := range r.Metrics {
		if m.Name == "" || seen[m.Name] || !finite(m.RequiredDelta) || m.RequiredDelta < 0 || (m.MaxIncrease != nil && (!finite(*m.MaxIncrease) || *m.MaxIncrease < 0)) {
			return fmt.Errorf("invalid or duplicate metric %q", m.Name)
		}
		seen[m.Name] = true
	}
	return nil
}

func means(samples []Sample, start, end, now time.Time) map[string]float64 {
	sums := make(map[string]float64)
	counts := make(map[string]int)
	for _, s := range samples {
		if s.At.Before(start) || !s.At.Before(end) || s.At.After(now) {
			continue
		}
		for name, v := range s.Values {
			if !finite(v) {
				continue
			}
			sums[name] += v
			counts[name]++
		}
	}
	for name, sum := range sums {
		mean := sum / float64(counts[name])
		if !finite(mean) {
			delete(sums, name)
		} else {
			sums[name] = mean
		}
	}
	return sums
}
