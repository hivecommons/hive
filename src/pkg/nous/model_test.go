package nous

import (
	"math"
	"strings"
	"testing"
	"time"
)

func number(v float64) *float64 { return &v }

func exampleExperiment() Experiment {
	return Experiment{
		ID: "flag-review", Hypothesis: "review flag lowers queue depth", CreatedAt: time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC),
		Subject: Subject{Kind: SubjectFlag, Reference: "review-v2"},
		Cohorts: []Cohort{
			{ID: "baseline", Role: RoleControl, HiveIDs: []string{"hive-a"}, OptIn: true, ACMMLevel: 2, RepoCount: 1, AgentCount: 3, Backend: "pi", Model: "model"},
			{ID: "candidate", Role: RoleTreatment, HiveIDs: []string{"hive-b"}, OptIn: true, ACMMLevel: 2, RepoCount: 1, AgentCount: 3, Backend: "pi", Model: "model"},
		},
		Window:     Window{BaselineHours: 24, ExperimentDurationHours: 48, CooldownHours: 12},
		Metrics:    []Metric{{ID: "queue", QuestionID: "auto_merge_timeliness", DefinitionID: "queue_depth_p90", SourceField: "queue_prs", Direction: DirectionDecrease, MinimumDelta: number(2), Threshold: number(10)}},
		Guardrails: []Guardrail{{MetricID: "queue", Upper: number(50)}},
	}
}

func addResult(e *Experiment) {
	e.Result = &Result{RecordedAt: e.CreatedAt.Add(72 * time.Hour), Verdict: VerdictImproved, Measurements: []Measurement{
		{MetricID: "queue", CohortID: "candidate", Baseline: number(20), Observed: number(10), Samples: 100},
	}}
}

func TestExperimentValidate(t *testing.T) {
	for _, kind := range []string{SubjectFlag, SubjectPR, SubjectChannel} {
		e := exampleExperiment()
		e.Subject.Kind = kind
		addResult(&e)
		if err := e.Validate(); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	tests := []struct {
		name   string
		change func(*Experiment)
	}{
		{"blank id", func(e *Experiment) { e.ID = " " }},
		{"missing hypothesis", func(e *Experiment) { e.Hypothesis = "" }},
		{"missing timestamp", func(e *Experiment) { e.CreatedAt = time.Time{} }},
		{"repo subject", func(e *Experiment) { e.Subject.Kind = "repo" }},
		{"missing subject", func(e *Experiment) { e.Subject.Reference = "" }},
		{"zero baseline", func(e *Experiment) { e.Window.BaselineHours = 0 }},
		{"negative duration", func(e *Experiment) { e.Window.ExperimentDurationHours = -1 }},
		{"negative cooldown", func(e *Experiment) { e.Window.CooldownHours = -1 }},
		{"no cohorts", func(e *Experiment) { e.Cohorts = nil }},
		{"duplicate cohorts", func(e *Experiment) { e.Cohorts[1].ID = e.Cohorts[0].ID }},
		{"not opted in", func(e *Experiment) { e.Cohorts[0].OptIn = false }},
		{"overlapping hives", func(e *Experiment) { e.Cohorts[1].HiveIDs = e.Cohorts[0].HiveIDs }},
		{"empty population", func(e *Experiment) { e.Cohorts[0].HiveIDs = nil }},
		{"empty hive", func(e *Experiment) { e.Cohorts[0].HiveIDs[0] = "" }},
		{"negative dimension", func(e *Experiment) { e.Cohorts[0].RepoCount = -1 }},
		{"missing model", func(e *Experiment) { e.Cohorts[0].Model = "" }},
		{"missing treatment", func(e *Experiment) { e.Cohorts[1].Role = RoleControl }},
		{"no metrics", func(e *Experiment) { e.Metrics = nil }},
		{"duplicate metric", func(e *Experiment) { e.Metrics = append(e.Metrics, e.Metrics[0]) }},
		{"unknown question", func(e *Experiment) { e.Metrics[0].QuestionID = "unknown" }},
		{"unknown definition", func(e *Experiment) { e.Metrics[0].DefinitionID = "unknown" }},
		{"invalid field", func(e *Experiment) { e.Metrics[0].SourceField = "queue_total" }},
		{"missing direction", func(e *Experiment) { e.Metrics[0].Direction = "" }},
		{"no target", func(e *Experiment) { e.Metrics[0].MinimumDelta = nil; e.Metrics[0].Threshold = nil }},
		{"negative delta", func(e *Experiment) { e.Metrics[0].MinimumDelta = number(-1) }},
		{"nan delta", func(e *Experiment) { e.Metrics[0].MinimumDelta = number(math.NaN()) }},
		{"infinite threshold", func(e *Experiment) { e.Metrics[0].Threshold = number(math.Inf(1)) }},
		{"unknown guardrail", func(e *Experiment) { e.Guardrails[0].MetricID = "unknown" }},
		{"duplicate guardrail", func(e *Experiment) { e.Guardrails = append(e.Guardrails, e.Guardrails[0]) }},
		{"conflicting guardrail", func(e *Experiment) {
			e.Guardrails = append(e.Guardrails, Guardrail{MetricID: "queue", Lower: number(100)})
		}},
		{"no bound", func(e *Experiment) { e.Guardrails[0].Upper = nil }},
		{"inverted bound", func(e *Experiment) { e.Guardrails[0].Lower = number(100) }},
		{"nan bound", func(e *Experiment) { e.Guardrails[0].Upper = number(math.NaN()) }},
		{"early result", func(e *Experiment) { e.Result.RecordedAt = e.CreatedAt.Add(-time.Hour) }},
		{"unknown verdict", func(e *Experiment) { e.Result.Verdict = "success" }},
		{"no evidence", func(e *Experiment) { e.Result.Measurements = nil }},
		{"unknown measurement", func(e *Experiment) { e.Result.Measurements[0].MetricID = "unknown" }},
		{"unknown population", func(e *Experiment) { e.Result.Measurements[0].CohortID = "unknown" }},
		{"duplicate measurement", func(e *Experiment) { e.Result.Measurements = append(e.Result.Measurements, e.Result.Measurements[0]) }},
		{"negative samples", func(e *Experiment) { e.Result.Measurements[0].Samples = -1 }},
		{"no samples", func(e *Experiment) { e.Result.Measurements[0].Samples = 0 }},
		{"infinite observation", func(e *Experiment) { e.Result.Measurements[0].Observed = number(math.Inf(-1)) }},
		{"unexplained missing evidence", func(e *Experiment) { e.Result.Measurements[0].Observed = nil }},
		{"contradictory missing evidence", func(e *Experiment) { e.Result.Measurements[0].MissingReason = "missing" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := exampleExperiment()
			addResult(&e)
			tt.change(&e)
			if err := e.Validate(); err == nil {
				t.Fatal("accepted invalid experiment")
			}
		})
	}
}

// Each rejected condition names itself so a fail-closed ledger load can be repaired.
func TestValidateReportsReason(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Experiment)
		want   string
	}{
		{"blank id", func(e *Experiment) { e.ID = " " }, "id is required"},
		{"repo subject", func(e *Experiment) { e.Subject.Kind = "repo" }, `subject kind "repo"`},
		{"zero baseline", func(e *Experiment) { e.Window.BaselineHours = 0 }, "baseline_hours"},
		{"duplicate cohorts", func(e *Experiment) { e.Cohorts[1].ID = e.Cohorts[0].ID }, `cohort "baseline": duplicate id`},
		{"overlapping hives", func(e *Experiment) { e.Cohorts[1].HiveIDs = e.Cohorts[0].HiveIDs }, `hive "hive-a" belongs to another cohort`},
		{"missing model", func(e *Experiment) { e.Cohorts[0].Model = "" }, `cohort "baseline": model is required`},
		{"unknown question", func(e *Experiment) { e.Metrics[0].QuestionID = "unknown" }, `question "unknown"`},
		{"unknown definition", func(e *Experiment) { e.Metrics[0].DefinitionID = "unknown" }, `definition "unknown"`},
		{"invalid field", func(e *Experiment) { e.Metrics[0].SourceField = "queue_total" }, `source_field "queue_total" must be one of queue_issues, queue_prs, queue_hold`},
		{"negative delta", func(e *Experiment) { e.Metrics[0].MinimumDelta = number(-1) }, `metric "queue": minimum_delta must not be negative`},
		{"duplicate metric", func(e *Experiment) { e.Metrics = append(e.Metrics, e.Metrics[0]) }, `metric "queue": duplicate id`},
		{"duplicate guardrail", func(e *Experiment) { e.Guardrails = append(e.Guardrails, e.Guardrails[0]) }, `guardrail for "queue": duplicate metric`},
		{"inverted bound", func(e *Experiment) { e.Guardrails[0].Lower = number(100) }, "lower bound exceeds upper bound"},
		{"unknown verdict", func(e *Experiment) { e.Result.Verdict = "success" }, `verdict "success"`},
		{"unknown population", func(e *Experiment) { e.Result.Measurements[0].CohortID = "x" }, `cohort "x" is not registered`},
		{"no samples", func(e *Experiment) { e.Result.Measurements[0].Samples = 0 }, "numerical evidence requires samples"},
		{"unexplained missing evidence", func(e *Experiment) { e.Result.Measurements[0].Observed = nil }, "missing evidence requires a reason"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := exampleExperiment()
			addResult(&e)
			tt.change(&e)
			err := e.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want reason containing %q", err, tt.want)
			}
		})
	}
}

func TestUnavailableMetricsRemainMissing(t *testing.T) {
	for _, d := range MetricCatalog() {
		if d.UnavailableReason == "" {
			continue
		}
		t.Run(d.ID, func(t *testing.T) {
			e := exampleExperiment()
			e.Metrics[0].DefinitionID = d.ID
			e.Metrics[0].SourceField = ""
			addResult(&e)
			if err := e.Validate(); err == nil {
				t.Fatal("fabricated numerical evidence for unavailable metric")
			}
			v := &e.Result.Measurements[0]
			v.Baseline = nil
			v.Observed = nil
			v.Samples = 0
			v.MissingReason = d.UnavailableReason
			e.Result.Verdict = VerdictInconclusive
			if err := e.Validate(); err != nil {
				t.Fatal(err)
			}
			e.Result.Verdict = VerdictImproved
			if err := e.Validate(); err == nil {
				t.Fatal("conclusive verdict without numerical evidence")
			}
			e.Result.Verdict = VerdictInconclusive
			e.Metrics[0].SourceField = "total_tokens"
			if err := e.Validate(); err == nil {
				t.Fatal("accepted fabricated source field")
			}
		})
	}
}

func TestCatalog(t *testing.T) {
	definitions := map[string]MetricDefinition{}
	for _, d := range MetricCatalog() {
		definitions[d.ID] = d
	}
	for _, id := range []string{"queue_depth_p90", "sla_violation_rate", "mttr", "kicks_without_output", "tokens_per_pr", "merge_acceptance"} {
		d, ok := definitions[id]
		if !ok || d.Unit == "" || (len(d.SourceFields) == 0 && d.UnavailableReason == "") {
			t.Fatalf("incomplete metric %s", id)
		}
	}
	if definitions["sla_violation_rate"].SourceFields[0] != "sla_violations" {
		t.Fatal("wrong SLA field")
	}
	questions := map[string]bool{}
	for _, q := range OutcomeQuestions() {
		if questions[q.ID] || q.Question == "" {
			t.Fatal("invalid question catalog")
		}
		questions[q.ID] = true
	}
	for _, id := range []string{"quality", "security", "review_usefulness", "auto_merge_timeliness", "outreach", "agent_effectiveness", "user_satisfaction"} {
		if !questions[id] {
			t.Fatalf("missing question %s", id)
		}
	}
	// Callers cannot mutate global catalog state.
	catalog := MetricCatalog()
	catalog[0].SourceFields[0] = "changed"
	if MetricCatalog()[0].SourceFields[0] != "queue_issues" {
		t.Fatal("shared catalog mutation")
	}
}

func TestFastFailedAcceptsObservedWithoutBaseline(t *testing.T) {
	e := exampleExperiment()
	addResult(&e)
	v := &e.Result.Measurements[0]
	v.Baseline = nil
	v.MissingReason = "no snapshot evidence in the baseline window"
	v.Samples = 1
	e.Result.Verdict = VerdictFastFailed
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	e.Result.Verdict = VerdictImproved
	if err := e.Validate(); err == nil {
		t.Fatal("conclusive verdict without baseline evidence")
	}
	e.Result.Verdict = VerdictFastFailed
	v.Observed = nil
	v.Samples = 0
	if err := e.Validate(); err == nil {
		t.Fatal("fast_failed without any observed evidence")
	}
}
