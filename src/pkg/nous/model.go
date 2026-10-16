// Package nous models background experiments on Hive itself. It does not own
// repo-scoped Experiment Lab experiments, delivery, scheduling, or evaluation.
package nous

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Subject is a change to Hive, never a user's repository or governor config.
type Subject struct {
	Kind      string `json:"kind"` // pr, flag, or channel
	Reference string `json:"reference"`
}

// Cohort records the opt-in population and matching dimensions at registration.
// A hive may belong to only one cohort in an experiment.
type Cohort struct {
	ID         string   `json:"id"`
	Role       string   `json:"role"` // control or treatment
	HiveIDs    []string `json:"hive_ids"`
	OptIn      bool     `json:"opt_in"`
	ACMMLevel  int      `json:"acmm_level"`
	RepoCount  int      `json:"repo_count"`
	AgentCount int      `json:"agent_count"`
	Backend    string   `json:"backend"`
	Model      string   `json:"model"`
}

// Window is pre-registered; baseline and experiment durations must be positive.
type Window struct {
	BaselineHours           int `json:"baseline_hours"`
	ExperimentDurationHours int `json:"experiment_duration_hours"`
	CooldownHours           int `json:"cooldown_hours"`
}

// MetricDefinition identifies a source, not an observed value. Unavailable
// metrics must not be treated as zero or inferred from unrelated counters.
type MetricDefinition struct {
	ID                string   `json:"id"`
	Unit              string   `json:"unit"`
	SourceFields      []string `json:"source_fields,omitempty"`
	Aggregation       string   `json:"aggregation,omitempty"`
	UnavailableReason string   `json:"unavailable_reason,omitempty"`
}

// MetricCatalog maps to the snapshot-series JSON fields in pkg/hivestate (#12354).
// Missing outcome instrumentation is explicit: snapshots do not contain PR
// outcomes, resolution times, or a link from kicks to accepted output.
func MetricCatalog() []MetricDefinition {
	return []MetricDefinition{
		{ID: "queue_depth_p90", Unit: "items", SourceFields: []string{"queue_issues", "queue_prs", "queue_hold"}, Aggregation: "nearest-rank p90 per queue (not the sum of p90s)"},
		{ID: "sla_violation_rate", Unit: "ratio", SourceFields: []string{"sla_violations"}, Aggregation: "samples with sla_violations > 0 / all samples"},
		{ID: "mttr", Unit: "hours", UnavailableReason: "snapshot series has no issue-open and fixing-PR-merge timestamps"},
		{ID: "kicks_without_output", Unit: "ratio", UnavailableReason: "agents_kicked has no linked output records"},
		{ID: "tokens_per_pr", Unit: "tokens/pr", UnavailableReason: "total_tokens has no PR attribution or accepted-PR denominator"},
		{ID: "merge_acceptance", Unit: "ratio", UnavailableReason: "snapshot series has no proposed/merged PR outcomes"},
	}
}

// OutcomeQuestion is the outcome-level catalog, including questions whose
// instrumentation is not yet supplied by the snapshot series.
type OutcomeQuestion struct {
	ID       string `json:"id"`
	Question string `json:"question"`
}

func OutcomeQuestions() []OutcomeQuestion {
	return []OutcomeQuestion{
		{"quality", "Is quality improving?"},
		{"security", "Is security improving?"},
		{"review_usefulness", "Are reviews useful and effective?"},
		{"auto_merge_timeliness", "Is auto-merge efficient and timely?"},
		{"outreach", "Is outreach effective?"},
		{"agent_effectiveness", "Is each agent effective?"},
		{"user_satisfaction", "Are users satisfied?"},
	}
}

// Metric pre-registers a directional absolute delta and/or absolute threshold
// in the definition's unit. Delta is treatment minus baseline, not a percent.
// Separate queue fields must be registered as separate metrics.
type Metric struct {
	ID           string   `json:"id"`
	QuestionID   string   `json:"question_id"`
	DefinitionID string   `json:"definition_id"`
	SourceField  string   `json:"source_field,omitempty"`
	Direction    string   `json:"direction"`               // increase or decrease
	MinimumDelta *float64 `json:"minimum_delta,omitempty"` // nonnegative magnitude
	Threshold    *float64 `json:"threshold,omitempty"`
}

// Guardrail is an absolute fast-fail bound, not a delivery/revert instruction.
type Guardrail struct {
	MetricID string   `json:"metric_id"`
	Lower    *float64 `json:"lower,omitempty"`
	Upper    *float64 `json:"upper,omitempty"`
}

// Measurement keeps missing evidence distinct from zero. Agent optionally
// scopes an effectiveness measurement; CohortID always identifies its population.
type Measurement struct {
	MetricID      string   `json:"metric_id"`
	CohortID      string   `json:"cohort_id"`
	Agent         string   `json:"agent,omitempty"`
	Baseline      *float64 `json:"baseline,omitempty"`
	Observed      *float64 `json:"observed,omitempty"`
	Samples       int      `json:"samples"`
	MissingReason string   `json:"missing_reason,omitempty"`
}

type Result struct {
	RecordedAt   time.Time     `json:"recorded_at"`
	Verdict      string        `json:"verdict"` // improved, regressed, inconclusive, fast_failed
	Measurements []Measurement `json:"measurements"`
	Summary      string        `json:"summary"`
}

type Experiment struct {
	ID         string      `json:"id"`
	Hypothesis string      `json:"hypothesis"`
	CreatedAt  time.Time   `json:"created_at"`
	Subject    Subject     `json:"subject"`
	Cohorts    []Cohort    `json:"cohorts"`
	Window     Window      `json:"window"`
	Metrics    []Metric    `json:"metrics"`
	Guardrails []Guardrail `json:"guardrails,omitempty"`
	Result     *Result     `json:"result,omitempty"`
}

func finite(v *float64) bool { return v == nil || (!math.IsNaN(*v) && !math.IsInf(*v, 0)) }
func nonempty(s string) bool { return strings.TrimSpace(s) != "" }

// Validate checks persisted records on both load and save. It does not judge
// outcomes or activate an experiment; unavailable metrics may be registered but
// cannot carry numerical results until their instrumentation is implemented.
func (e Experiment) Validate() error {
	if !nonempty(e.ID) || !nonempty(e.Hypothesis) || e.CreatedAt.IsZero() {
		return fmt.Errorf("id, hypothesis and created_at are required")
	}
	if (e.Subject.Kind != "pr" && e.Subject.Kind != "flag" && e.Subject.Kind != "channel") || !nonempty(e.Subject.Reference) {
		return fmt.Errorf("invalid Hive subject")
	}
	if e.Window.BaselineHours <= 0 || e.Window.ExperimentDurationHours <= 0 || e.Window.CooldownHours < 0 {
		return fmt.Errorf("invalid window")
	}
	cohorts, hives := map[string]bool{}, map[string]bool{}
	roles := map[string]bool{}
	for _, c := range e.Cohorts {
		if !nonempty(c.ID) || cohorts[c.ID] || !c.OptIn || len(c.HiveIDs) == 0 || (c.Role != "control" && c.Role != "treatment") || c.ACMMLevel < 0 || c.RepoCount < 0 || c.AgentCount < 0 || !nonempty(c.Backend) || !nonempty(c.Model) {
			return fmt.Errorf("invalid or duplicate cohort %q", c.ID)
		}
		cohorts[c.ID], roles[c.Role] = true, true
		for _, id := range c.HiveIDs {
			if !nonempty(id) || hives[id] {
				return fmt.Errorf("invalid or overlapping hive %q", id)
			}
			hives[id] = true
		}
	}
	if !roles["control"] || !roles["treatment"] {
		return fmt.Errorf("control and treatment cohorts are required")
	}
	definitions := map[string]MetricDefinition{}
	for _, d := range MetricCatalog() {
		definitions[d.ID] = d
	}
	questions := map[string]bool{}
	for _, q := range OutcomeQuestions() {
		questions[q.ID] = true
	}
	metrics := map[string]Metric{}
	for _, m := range e.Metrics {
		d, ok := definitions[m.DefinitionID]
		if !nonempty(m.ID) || metrics[m.ID].ID != "" || !ok || !questions[m.QuestionID] || (m.Direction != "increase" && m.Direction != "decrease") || (m.MinimumDelta == nil && m.Threshold == nil) || !finite(m.MinimumDelta) || !finite(m.Threshold) || (m.MinimumDelta != nil && *m.MinimumDelta < 0) {
			return fmt.Errorf("invalid metric %q", m.ID)
		}
		if len(d.SourceFields) > 0 {
			found := false
			for _, field := range d.SourceFields {
				if field == m.SourceField {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("metric %q requires a catalog source field", m.ID)
			}
		} else if m.SourceField != "" {
			return fmt.Errorf("metric %q has no snapshot source field", m.ID)
		}
		metrics[m.ID] = m
	}
	if len(metrics) == 0 {
		return fmt.Errorf("at least one pre-registered metric is required")
	}
	guardrails := map[string]bool{}
	for _, g := range e.Guardrails {
		if guardrails[g.MetricID] || metrics[g.MetricID].ID == "" || (g.Lower == nil && g.Upper == nil) || !finite(g.Lower) || !finite(g.Upper) || (g.Lower != nil && g.Upper != nil && *g.Lower > *g.Upper) {
			return fmt.Errorf("invalid guardrail for %q", g.MetricID)
		}
		guardrails[g.MetricID] = true
	}
	if e.Result == nil {
		return nil
	}
	r := e.Result
	if r.RecordedAt.Before(e.CreatedAt) || (r.Verdict != "improved" && r.Verdict != "regressed" && r.Verdict != "inconclusive" && r.Verdict != "fast_failed") || len(r.Measurements) == 0 {
		return fmt.Errorf("invalid result")
	}
	seen := map[[3]string]bool{}
	complete := false
	for _, v := range r.Measurements {
		m, ok := metrics[v.MetricID]
		key := [3]string{v.MetricID, v.CohortID, v.Agent}
		if !ok || !cohorts[v.CohortID] || seen[key] || v.Samples < 0 || !finite(v.Baseline) || !finite(v.Observed) {
			return fmt.Errorf("invalid measurement for %q", v.MetricID)
		}
		seen[key] = true
		missing := v.Baseline == nil || v.Observed == nil
		if missing && !nonempty(v.MissingReason) {
			return fmt.Errorf("missing evidence requires a reason")
		}
		if !missing && (v.Samples == 0 || nonempty(v.MissingReason)) {
			return fmt.Errorf("numerical evidence requires samples and no missing reason")
		}
		if !missing {
			complete = true
		}
		if definitions[m.DefinitionID].UnavailableReason != "" && (v.Baseline != nil || v.Observed != nil) {
			return fmt.Errorf("metric %q has no snapshot instrumentation", m.ID)
		}
	}
	if !complete && r.Verdict != "inconclusive" {
		return fmt.Errorf("a conclusive verdict requires numerical evidence")
	}
	return nil
}
