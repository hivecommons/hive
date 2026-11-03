// Package nous models background experiments on Hive itself. It does not own
// repo-scoped Experiment Lab experiments, delivery, scheduling, or evaluation.
package nous

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Subject kinds: a Hive change is a PR, a feature flag, or a release channel.
const (
	SubjectPR      = "pr"
	SubjectFlag    = "flag"
	SubjectChannel = "channel"
)

// Cohort roles.
const (
	RoleControl   = "control"
	RoleTreatment = "treatment"
)

// Metric directions for the pre-registered delta.
const (
	DirectionIncrease = "increase"
	DirectionDecrease = "decrease"
)

// Result verdicts.
const (
	VerdictImproved     = "improved"
	VerdictRegressed    = "regressed"
	VerdictInconclusive = "inconclusive"
	VerdictFastFailed   = "fast_failed"
)

// Subject is a change to Hive, never a user's repository or governor config.
type Subject struct {
	Kind      string `json:"kind"` // SubjectPR, SubjectFlag, or SubjectChannel
	Reference string `json:"reference"`
}

// Cohort records the opt-in population and matching dimensions at registration.
// A hive may belong to only one cohort in an experiment.
type Cohort struct {
	ID         string   `json:"id"`
	Role       string   `json:"role"` // RoleControl or RoleTreatment
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
// SourceFields are the hivestate.Snapshot JSON tags spelled as literals because
// hivestate exports no name constants; TestCatalogSnapshotContract fails if a
// tag is renamed there, so update both together. Missing outcome
// instrumentation is explicit: snapshots do not contain PR outcomes,
// resolution times, or a link from kicks to accepted output.
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
	Direction    string   `json:"direction"`               // DirectionIncrease or DirectionDecrease
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
	Verdict      string        `json:"verdict"` // one of the Verdict* constants
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

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

// Validate checks persisted records on both load and save. It does not judge
// outcomes or activate an experiment; unavailable metrics may be registered but
// cannot carry numerical results until their instrumentation is implemented.
// Each condition reports its own reason so a rejected ledger can be repaired.
func (e Experiment) Validate() error {
	switch {
	case !nonempty(e.ID):
		return fmt.Errorf("id is required")
	case !nonempty(e.Hypothesis):
		return fmt.Errorf("hypothesis is required")
	case e.CreatedAt.IsZero():
		return fmt.Errorf("created_at is required")
	case !oneOf(e.Subject.Kind, SubjectPR, SubjectFlag, SubjectChannel):
		return fmt.Errorf("subject kind %q must be %s, %s or %s", e.Subject.Kind, SubjectPR, SubjectFlag, SubjectChannel)
	case !nonempty(e.Subject.Reference):
		return fmt.Errorf("subject reference is required")
	case e.Window.BaselineHours <= 0:
		return fmt.Errorf("window baseline_hours must be positive")
	case e.Window.ExperimentDurationHours <= 0:
		return fmt.Errorf("window experiment_duration_hours must be positive")
	case e.Window.CooldownHours < 0:
		return fmt.Errorf("window cooldown_hours must not be negative")
	}
	cohorts, hives := map[string]bool{}, map[string]bool{}
	roles := map[string]bool{}
	for _, c := range e.Cohorts {
		if err := c.validate(); err != nil {
			return fmt.Errorf("cohort %q: %w", c.ID, err)
		}
		if cohorts[c.ID] {
			return fmt.Errorf("cohort %q: duplicate id", c.ID)
		}
		cohorts[c.ID], roles[c.Role] = true, true
		for _, id := range c.HiveIDs {
			if !nonempty(id) {
				return fmt.Errorf("cohort %q: hive id is required", c.ID)
			}
			if hives[id] {
				return fmt.Errorf("cohort %q: hive %q belongs to another cohort", c.ID, id)
			}
			hives[id] = true
		}
	}
	if !roles[RoleControl] || !roles[RoleTreatment] {
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
		if err := m.validate(definitions, questions); err != nil {
			return fmt.Errorf("metric %q: %w", m.ID, err)
		}
		if _, dup := metrics[m.ID]; dup {
			return fmt.Errorf("metric %q: duplicate id", m.ID)
		}
		metrics[m.ID] = m
	}
	if len(metrics) == 0 {
		return fmt.Errorf("at least one pre-registered metric is required")
	}
	guardrails := map[string]bool{}
	for _, g := range e.Guardrails {
		if err := g.validate(metrics); err != nil {
			return fmt.Errorf("guardrail for %q: %w", g.MetricID, err)
		}
		if guardrails[g.MetricID] {
			return fmt.Errorf("guardrail for %q: duplicate metric", g.MetricID)
		}
		guardrails[g.MetricID] = true
	}
	if e.Result == nil {
		return nil
	}
	return e.Result.validate(e.CreatedAt, cohorts, metrics, definitions)
}

func (c Cohort) validate() error {
	switch {
	case !nonempty(c.ID):
		return fmt.Errorf("id is required")
	case !oneOf(c.Role, RoleControl, RoleTreatment):
		return fmt.Errorf("role %q must be %s or %s", c.Role, RoleControl, RoleTreatment)
	case !c.OptIn:
		return fmt.Errorf("population must opt in")
	case len(c.HiveIDs) == 0:
		return fmt.Errorf("at least one hive id is required")
	case c.ACMMLevel < 0:
		return fmt.Errorf("acmm_level must not be negative")
	case c.RepoCount < 0:
		return fmt.Errorf("repo_count must not be negative")
	case c.AgentCount < 0:
		return fmt.Errorf("agent_count must not be negative")
	case !nonempty(c.Backend):
		return fmt.Errorf("backend is required")
	case !nonempty(c.Model):
		return fmt.Errorf("model is required")
	}
	return nil
}

func (m Metric) validate(definitions map[string]MetricDefinition, questions map[string]bool) error {
	d, ok := definitions[m.DefinitionID]
	switch {
	case !nonempty(m.ID):
		return fmt.Errorf("id is required")
	case !ok:
		return fmt.Errorf("definition %q is not in the metric catalog", m.DefinitionID)
	case !questions[m.QuestionID]:
		return fmt.Errorf("question %q is not in the outcome catalog", m.QuestionID)
	case !oneOf(m.Direction, DirectionIncrease, DirectionDecrease):
		return fmt.Errorf("direction %q must be %s or %s", m.Direction, DirectionIncrease, DirectionDecrease)
	case m.MinimumDelta == nil && m.Threshold == nil:
		return fmt.Errorf("minimum_delta or threshold is required")
	case !finite(m.MinimumDelta):
		return fmt.Errorf("minimum_delta must be finite")
	case !finite(m.Threshold):
		return fmt.Errorf("threshold must be finite")
	case m.MinimumDelta != nil && *m.MinimumDelta < 0:
		return fmt.Errorf("minimum_delta must not be negative")
	}
	if len(d.SourceFields) > 0 {
		if !oneOf(m.SourceField, d.SourceFields...) {
			return fmt.Errorf("source_field %q must be one of %s", m.SourceField, strings.Join(d.SourceFields, ", "))
		}
	} else if m.SourceField != "" {
		return fmt.Errorf("definition %q has no snapshot source field", m.DefinitionID)
	}
	return nil
}

func (g Guardrail) validate(metrics map[string]Metric) error {
	switch {
	case metrics[g.MetricID].ID == "":
		return fmt.Errorf("metric is not pre-registered")
	case g.Lower == nil && g.Upper == nil:
		return fmt.Errorf("lower or upper bound is required")
	case !finite(g.Lower) || !finite(g.Upper):
		return fmt.Errorf("bounds must be finite")
	case g.Lower != nil && g.Upper != nil && *g.Lower > *g.Upper:
		return fmt.Errorf("lower bound exceeds upper bound")
	}
	return nil
}

func (r Result) validate(createdAt time.Time, cohorts map[string]bool, metrics map[string]Metric, definitions map[string]MetricDefinition) error {
	switch {
	case r.RecordedAt.Before(createdAt):
		return fmt.Errorf("result recorded_at precedes experiment created_at")
	case !oneOf(r.Verdict, VerdictImproved, VerdictRegressed, VerdictInconclusive, VerdictFastFailed):
		return fmt.Errorf("result verdict %q must be %s, %s, %s or %s", r.Verdict, VerdictImproved, VerdictRegressed, VerdictInconclusive, VerdictFastFailed)
	case len(r.Measurements) == 0:
		return fmt.Errorf("result requires at least one measurement")
	}
	seen := map[[3]string]bool{}
	complete := false
	for _, v := range r.Measurements {
		m, ok := metrics[v.MetricID]
		key := [3]string{v.MetricID, v.CohortID, v.Agent}
		switch {
		case !ok:
			return fmt.Errorf("measurement for %q: metric is not pre-registered", v.MetricID)
		case !cohorts[v.CohortID]:
			return fmt.Errorf("measurement for %q: cohort %q is not registered", v.MetricID, v.CohortID)
		case seen[key]:
			return fmt.Errorf("measurement for %q: duplicate cohort %q agent %q", v.MetricID, v.CohortID, v.Agent)
		case v.Samples < 0:
			return fmt.Errorf("measurement for %q: samples must not be negative", v.MetricID)
		case !finite(v.Baseline) || !finite(v.Observed):
			return fmt.Errorf("measurement for %q: baseline and observed must be finite", v.MetricID)
		}
		seen[key] = true
		missing := v.Baseline == nil || v.Observed == nil
		if missing && !nonempty(v.MissingReason) {
			return fmt.Errorf("measurement for %q: missing evidence requires a reason", v.MetricID)
		}
		if !missing && (v.Samples == 0 || nonempty(v.MissingReason)) {
			return fmt.Errorf("measurement for %q: numerical evidence requires samples and no missing reason", v.MetricID)
		}
		// A guardrail fast-fail is judged against absolute bounds, so an observed
		// treatment value is evidence even when the baseline window is empty.
		if !missing || (r.Verdict == VerdictFastFailed && v.Observed != nil && v.Samples > 0) {
			complete = true
		}
		if definitions[m.DefinitionID].UnavailableReason != "" && (v.Baseline != nil || v.Observed != nil) {
			return fmt.Errorf("measurement for %q: definition %q has no snapshot instrumentation", m.ID, m.DefinitionID)
		}
	}
	if !complete && r.Verdict != VerdictInconclusive {
		return fmt.Errorf("verdict %q requires numerical evidence", r.Verdict)
	}
	return nil
}
