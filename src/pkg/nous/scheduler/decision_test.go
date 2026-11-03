package scheduler

import (
	"math"
	"strings"
	"testing"
	"time"
)

func registration() Registration {
	return Registration{
		Start:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Baseline: time.Hour, Experiment: 2 * time.Hour, Cooldown: time.Hour,
		Metrics: []Metric{{Name: "tokens_per_pr", LowerIsBetter: true, RequiredDelta: 10}},
	}
}

func observations(r Registration) []Sample {
	return []Sample{
		{At: r.Start, Values: map[string]float64{"tokens_per_pr": 100}},
		{At: r.Start.Add(time.Hour), Values: map[string]float64{"tokens_per_pr": 90}},
	}
}

func TestDisabledOutsideBackground(t *testing.T) {
	for _, tc := range []struct {
		enabled bool
		mode    string
	}{
		{false, "background"}, {true, ""}, {true, "repo"}, {true, "interactive"},
	} {
		// Even an invalid registration cannot accidentally activate another mode.
		d, err := Evaluate(tc.enabled, tc.mode, time.Now(), Registration{}, nil)
		if err != nil || d.Phase != Disabled || d.Revert || d.Judged {
			t.Fatalf("%+v: decision=%+v err=%v", tc, d, err)
		}
	}
}

func TestWindowBoundaries(t *testing.T) {
	r := registration()
	for _, tc := range []struct {
		elapsed        time.Duration
		phase          Phase
		revert, judged bool
	}{
		{0, Baseline, false, false},
		{time.Hour - time.Nanosecond, Baseline, false, false},
		{time.Hour, Experiment, false, false},
		{3*time.Hour - time.Nanosecond, Experiment, false, false},
		{3 * time.Hour, Cooldown, true, false},
		{4*time.Hour - time.Nanosecond, Cooldown, true, false},
		{4 * time.Hour, Complete, true, true},
	} {
		d, err := Evaluate(true, "background", r.Start.Add(tc.elapsed), r, observations(r))
		if err != nil || d.Phase != tc.phase || d.Revert != tc.revert || d.Judged != tc.judged {
			t.Fatalf("at %v: %+v err=%v", tc.elapsed, d, err)
		}
	}
	r.Cooldown = 0
	d, err := Evaluate(true, "background", r.Start.Add(3*time.Hour), r, observations(r))
	if err != nil || d.Phase != Complete || !d.Judged {
		t.Fatalf("zero cooldown: %+v %v", d, err)
	}
}

func TestJudgementUsesRegisteredDirectionAndThreshold(t *testing.T) {
	for _, tc := range []struct {
		lower  bool
		value  float64
		passed bool
	}{
		{true, 90, true}, {true, 91, false}, {false, 110, true}, {false, 109, false},
	} {
		r := registration()
		r.Metrics[0].LowerIsBetter = tc.lower
		samples := observations(r)
		samples[1].Values["tokens_per_pr"] = tc.value
		// Cooldown observations must not improve or poison the treatment average.
		samples = append(samples, Sample{At: r.Start.Add(3 * time.Hour), Values: map[string]float64{"tokens_per_pr": -1000}})
		d, err := Evaluate(true, "background", r.Start.Add(4*time.Hour), r, samples)
		if err != nil || !d.Judged || d.Passed != tc.passed {
			t.Fatalf("%+v: %+v %v", tc, d, err)
		}
	}
}

func TestFastFailIsPerSnapshotAndRequiresRevert(t *testing.T) {
	r := registration()
	bound := 50.0
	r.Metrics[0].MaxIncrease = &bound
	samples := observations(r)
	samples = append(samples, Sample{At: r.Start.Add(2 * time.Hour), Values: map[string]float64{"tokens_per_pr": 151}})
	for _, elapsed := range []time.Duration{2 * time.Hour, 3 * time.Hour, 4 * time.Hour} {
		d, err := Evaluate(true, "background", r.Start.Add(elapsed), r, samples)
		if err != nil || !d.FastFail || !d.Revert || d.Passed || d.Reason != "fast-fail: tokens_per_pr" {
			t.Fatalf("at %v: %+v %v", elapsed, d, err)
		}
	}
	// Future snapshots cannot trip a bound before they were observed.
	d, err := Evaluate(true, "background", r.Start.Add(time.Hour), r, samples)
	if err != nil || d.FastFail || d.Revert {
		t.Fatalf("future: %+v %v", d, err)
	}
	samples[2].Values["tokens_per_pr"] = 150
	d, err = Evaluate(true, "background", r.Start.Add(2*time.Hour), r, samples)
	if err != nil || d.FastFail {
		t.Fatalf("bound equality: %+v %v", d, err)
	}
}

func TestMissingEvidenceNeverPasses(t *testing.T) {
	for _, samples := range [][]Sample{
		nil,
		{{At: registration().Start, Values: map[string]float64{"tokens_per_pr": 100}}},
		{{At: registration().Start.Add(time.Hour), Values: map[string]float64{"tokens_per_pr": 0}}},
		{{At: registration().Start, Values: map[string]float64{"tokens_per_pr": math.NaN()}}, {At: registration().Start.Add(time.Hour), Values: map[string]float64{"tokens_per_pr": math.Inf(1)}}},
	} {
		r := registration()
		d, err := Evaluate(true, "background", r.Start.Add(4*time.Hour), r, samples)
		if err != nil || d.Judged || d.Passed || !d.Revert || !strings.HasPrefix(d.Reason, "missing evidence:") {
			t.Fatalf("decision=%+v err=%v", d, err)
		}
	}
}

func TestEveryRegisteredMetricRequiresEvidence(t *testing.T) {
	r := registration()
	r.Metrics = append(r.Metrics, Metric{Name: "sla_violation_rate", LowerIsBetter: true, RequiredDelta: 0.01})
	samples := observations(r)
	now := r.Start.Add(4 * time.Hour)
	d, err := Evaluate(true, "background", now, r, samples)
	if err != nil || d.Judged || d.Passed {
		t.Fatalf("missing SLA: %+v %v", d, err)
	}
	samples[0].Values["sla_violation_rate"] = 0.1
	samples[1].Values["sla_violation_rate"] = 0.2
	d, err = Evaluate(true, "background", now, r, samples)
	if err != nil || !d.Judged || d.Passed {
		t.Fatalf("regressed SLA: %+v %v", d, err)
	}
	bound := 0.05
	r.Metrics[1].MaxIncrease = &bound
	d, err = Evaluate(true, "background", now, r, samples)
	if err != nil || !d.FastFail || !d.Revert || d.Reason != "fast-fail: sla_violation_rate" {
		t.Fatalf("SLA spike: %+v %v", d, err)
	}
}

func TestInvalidRegistrations(t *testing.T) {
	for _, mutate := range []func(*Registration){
		func(r *Registration) { r.Start = time.Time{} },
		func(r *Registration) { r.Baseline = 0 },
		func(r *Registration) { r.Experiment = -time.Hour },
		func(r *Registration) { r.Cooldown = -time.Hour },
		func(r *Registration) { r.Metrics = nil },
		func(r *Registration) { r.Metrics[0].Name = "" },
		func(r *Registration) { r.Metrics = append(r.Metrics, r.Metrics[0]) },
		func(r *Registration) { r.Metrics[0].RequiredDelta = math.NaN() },
		func(r *Registration) { r.Metrics[0].RequiredDelta = -1 },
		func(r *Registration) { v := math.Inf(1); r.Metrics[0].MaxIncrease = &v },
		func(r *Registration) { v := -1.0; r.Metrics[0].MaxIncrease = &v },
	} {
		r := registration()
		mutate(&r)
		if _, err := Evaluate(true, "background", r.Start, r, nil); err == nil {
			t.Fatalf("accepted %+v", r)
		}
	}
}
