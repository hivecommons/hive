package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hivestate"
	"github.com/hivecommons/hive/pkg/nous"
)

func ptr(v float64) *float64 { return &v }

func experiment() nous.Experiment {
	cohort := func(id, role string, hives ...string) nous.Cohort {
		return nous.Cohort{ID: id, Role: role, HiveIDs: hives, OptIn: true, Backend: "copilot", Model: "m"}
	}
	return nous.Experiment{
		ID:         "exp-1",
		Hypothesis: "fewer held items",
		CreatedAt:  time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Subject:    nous.Subject{Kind: nous.SubjectFlag, Reference: "flag-a"},
		Cohorts:    []nous.Cohort{cohort("c", nous.RoleControl, "control-hive"), cohort("t", nous.RoleTreatment, "treat-hive")},
		Window:     nous.Window{BaselineHours: 1, ExperimentDurationHours: 2, CooldownHours: 1},
		Metrics: []nous.Metric{
			{ID: "hold", QuestionID: "quality", DefinitionID: "queue_depth_p90", SourceField: "queue_hold", Direction: nous.DirectionDecrease, MinimumDelta: ptr(1)},
			{ID: "sla", QuestionID: "quality", DefinitionID: "sla_violation_rate", SourceField: "sla_violations", Direction: nous.DirectionDecrease, Threshold: ptr(0.5)},
		},
	}
}

func bucket(at time.Time, hold, sla int, tokens int64) hivestate.Bucket {
	return hivestate.FromSnapshot(at, hivestate.Snapshot{QueueHold: hold, SLAViolations: sla, TotalTokens: tokens})
}

// series has baseline hold=5 and treatment hold=3 with no SLA violations.
func series(e nous.Experiment) []HiveSeries {
	s := e.CreatedAt
	return []HiveSeries{{HiveID: "treat-hive", Buckets: []hivestate.Bucket{
		bucket(s, 5, 0, 10), bucket(s.Add(time.Hour), 3, 0, 20), bucket(s.Add(2*time.Hour), 3, 0, 30),
	}}}
}

func TestPlanForMapsWindows(t *testing.T) {
	e := experiment()
	p, err := PlanFor(e)
	if err != nil {
		t.Fatal(err)
	}
	r := p.Registration
	if !r.Start.Equal(e.CreatedAt) || r.Baseline != time.Hour || r.Experiment != 2*time.Hour || r.Cooldown != time.Hour {
		t.Fatalf("windows: %+v", r)
	}
	e.Window.CooldownHours = 0
	if p, err = PlanFor(e); err != nil || p.Registration.Cooldown != 0 {
		t.Fatalf("zero cooldown: %+v %v", p.Registration, err)
	}
	e.Window.BaselineHours = 0
	if _, err := PlanFor(e); err == nil {
		t.Fatal("invalid window accepted")
	}
}

func TestPlanForMapsDirectionAndDelta(t *testing.T) {
	e := experiment()
	e.Metrics = append(e.Metrics, nous.Metric{ID: "issues", QuestionID: "quality", DefinitionID: "queue_depth_p90", SourceField: "queue_issues", Direction: nous.DirectionIncrease, MinimumDelta: ptr(2.5)})
	p, err := PlanFor(e)
	if err != nil {
		t.Fatal(err)
	}
	want := []Metric{
		{Name: "hold", LowerIsBetter: true, RequiredDelta: 1},
		{Name: "sla", LowerIsBetter: true, RequiredDelta: 0},
		{Name: "issues", LowerIsBetter: false, RequiredDelta: 2.5},
	}
	if len(p.Registration.Metrics) != len(want) {
		t.Fatalf("metrics: %+v", p.Registration.Metrics)
	}
	for i, m := range p.Registration.Metrics {
		if m.Name != want[i].Name || m.LowerIsBetter != want[i].LowerIsBetter || m.RequiredDelta != want[i].RequiredDelta || m.MaxIncrease != nil {
			t.Fatalf("metric %d: got %+v want %+v", i, m, want[i])
		}
	}
}

func TestSamplesForFiltersTreatmentCohort(t *testing.T) {
	e := experiment()
	at := e.CreatedAt
	got := SamplesFor(e, []HiveSeries{
		{HiveID: "control-hive", Buckets: []hivestate.Bucket{bucket(at, 99, 1, 0)}},
		{HiveID: "stranger", Buckets: []hivestate.Bucket{bucket(at, 99, 1, 0)}},
		{HiveID: "", Buckets: []hivestate.Bucket{bucket(at, 99, 1, 0)}},
		{HiveID: "treat-hive", Buckets: []hivestate.Bucket{bucket(at.Add(time.Minute), 4, 1, 0), {Start: at}}},
	})
	if len(got) != 1 || !got[0].At.Equal(at.Add(time.Minute)) {
		t.Fatalf("samples: %+v", got)
	}
	if got[0].Values["hold"] != 4 || got[0].Values["sla"] != 1 {
		t.Fatalf("values: %+v", got[0].Values)
	}
}

func TestSamplesForQueueP90PerBucket(t *testing.T) {
	e := experiment()
	b := hivestate.Bucket{Start: e.CreatedAt, Samples: 10, QueueHold: []int{10, 1, 2, 3, 4, 5, 6, 7, 8, 9}, SLAViolatedSamples: 3}
	got := SamplesFor(e, []HiveSeries{{HiveID: "treat-hive", Buckets: []hivestate.Bucket{b}}})
	if len(got) != 1 || got[0].Values["hold"] != 9 || got[0].Values["sla"] != 0.3 {
		t.Fatalf("samples: %+v", got)
	}
}

func TestUnavailableMetricIsMissingEvidence(t *testing.T) {
	e := experiment()
	e.Metrics = append(e.Metrics, nous.Metric{ID: "tokens", QuestionID: "quality", DefinitionID: "tokens_per_pr", Direction: nous.DirectionDecrease, MinimumDelta: ptr(1)})
	for _, s := range SamplesFor(e, series(e)) {
		if _, ok := s.Values["tokens"]; ok {
			t.Fatalf("unavailable metric got a value: %+v", s)
		}
	}
	d, err := EvaluateExperiment(true, "background", e.CreatedAt.Add(4*time.Hour), e, series(e))
	if err != nil || d.Phase != Complete || d.Judged || d.Passed || d.Reason != "missing evidence: tokens" {
		t.Fatalf("decision=%+v err=%v", d, err)
	}
}

func TestEvaluateExperimentJudges(t *testing.T) {
	e := experiment()
	d, err := EvaluateExperiment(true, "background", e.CreatedAt.Add(4*time.Hour), e, series(e))
	if err != nil || !d.Judged || !d.Passed || !d.Revert {
		t.Fatalf("decision=%+v err=%v", d, err)
	}
	e.Metrics[0].MinimumDelta = ptr(3)
	if d, err = EvaluateExperiment(true, "background", e.CreatedAt.Add(4*time.Hour), e, series(e)); err != nil || !d.Judged || d.Passed {
		t.Fatalf("delta not enforced: %+v %v", d, err)
	}
}

func TestThresholdJudgement(t *testing.T) {
	for _, tc := range []struct {
		direction string
		threshold float64
		passed    bool
	}{
		// Treatment hold mean is 3 and baseline 5; threshold-only has no delta requirement.
		{nous.DirectionDecrease, 3, true}, {nous.DirectionDecrease, 2.9, false},
		{nous.DirectionIncrease, 3, true}, {nous.DirectionIncrease, 3.1, false},
	} {
		e := experiment()
		e.Metrics[0].Direction, e.Metrics[0].MinimumDelta, e.Metrics[0].Threshold = tc.direction, nil, ptr(tc.threshold)
		d, err := EvaluateExperiment(true, "background", e.CreatedAt.Add(4*time.Hour), e, series(e))
		if err != nil || !d.Judged || d.Passed != tc.passed {
			t.Fatalf("%+v: decision=%+v err=%v", tc, d, err)
		}
	}
}

func TestGuardrailSLASpikeFastFails(t *testing.T) {
	e := experiment()
	e.Guardrails = []nous.Guardrail{{MetricID: "sla", Upper: ptr(0.5)}}
	s := series(e)
	// A spike during baseline is not a treatment effect.
	s[0].Buckets[0] = bucket(e.CreatedAt, 5, 3, 0)
	if d, err := EvaluateExperiment(true, "background", e.CreatedAt.Add(90*time.Minute), e, s); err != nil || d.FastFail {
		t.Fatalf("baseline spike: %+v %v", d, err)
	}
	s[0].Buckets = append(s[0].Buckets, bucket(e.CreatedAt.Add(80*time.Minute), 3, 2, 0))
	// Future samples are ignored.
	if d, err := EvaluateExperiment(true, "background", e.CreatedAt.Add(70*time.Minute), e, s); err != nil || d.FastFail {
		t.Fatalf("future spike: %+v %v", d, err)
	}
	for _, now := range []time.Duration{90 * time.Minute, 4 * time.Hour} {
		d, err := EvaluateExperiment(true, "background", e.CreatedAt.Add(now), e, s)
		if err != nil || !d.FastFail || !d.Revert || d.Judged || !strings.Contains(d.Reason, "guardrail sla") {
			t.Fatalf("at %v: decision=%+v err=%v", now, d, err)
		}
	}
}

func TestGuardrailLowerBound(t *testing.T) {
	e := experiment()
	e.Guardrails = []nous.Guardrail{{MetricID: "hold", Lower: ptr(3.5)}}
	d, err := EvaluateExperiment(true, "background", e.CreatedAt.Add(90*time.Minute), e, series(e))
	if err != nil || !d.FastFail || d.Phase != Experiment {
		t.Fatalf("decision=%+v err=%v", d, err)
	}
}

func TestGuardrailTokenBlowUp(t *testing.T) {
	e := experiment()
	e.Metrics = append(e.Metrics, nous.Metric{ID: "tokens", QuestionID: "quality", DefinitionID: "tokens_per_pr", Direction: nous.DirectionDecrease, MinimumDelta: ptr(1)})
	e.Guardrails = []nous.Guardrail{{MetricID: "tokens", Upper: ptr(1)}}
	s := series(e)
	s[0].Buckets = append(s[0].Buckets, bucket(e.CreatedAt.Add(90*time.Minute), 3, 0, 1<<40))
	// tokens_per_pr has no snapshot instrumentation: a blow-up in raw totals is
	// missing evidence, so it neither breaches the guardrail nor reads as zero.
	d, err := EvaluateExperiment(true, "background", e.CreatedAt.Add(2*time.Hour), e, s)
	if err != nil || d.FastFail {
		t.Fatalf("unavailable guardrail fired: %+v %v", d, err)
	}
	// An instrumented blow-up (queue hold) does fast-fail.
	e.Guardrails = []nous.Guardrail{{MetricID: "hold", Upper: ptr(10)}}
	s[0].Buckets = append(s[0].Buckets, bucket(e.CreatedAt.Add(100*time.Minute), 500, 0, 0))
	if d, err = EvaluateExperiment(true, "background", e.CreatedAt.Add(2*time.Hour), e, s); err != nil || !d.FastFail || !d.Revert {
		t.Fatalf("queue blow-up: %+v %v", d, err)
	}
}

func TestEvaluateExperimentDisabled(t *testing.T) {
	for _, mode := range []string{"", "repo"} {
		d, err := EvaluateExperiment(true, mode, time.Now(), nous.Experiment{}, nil)
		if err != nil || d.Phase != Disabled || d.Revert {
			t.Fatalf("%q: %+v %v", mode, d, err)
		}
	}
	if d, err := EvaluateExperiment(false, "background", time.Now(), experiment(), nil); err != nil || d.Phase != Disabled {
		t.Fatalf("disabled: %+v %v", d, err)
	}
	if _, err := EvaluateExperiment(true, "background", time.Now(), nous.Experiment{}, nil); err == nil {
		t.Fatal("invalid experiment accepted")
	}
}
