package config

import (
	"fmt"
	"strings"
	"time"
)

// Analysis fact-check (hivecommons/hive#19074).
//
// A PR an agent opens as an analysis or report (a docs assessment, an audit
// write-up) is only as true as the authoring agent's reading of its sources,
// and the review swarm's code-shaped perspectives do not test that. This block
// turns on an independent pass that re-verifies the claims before a person
// reviews. Everything here is opt-in and generic: which PRs qualify, who
// reviews, how independent that reviewer must be and how many rounds the
// author and reviewer may trade are the operator's call.

const (
	// FactCheckIndependenceAgent only requires the reviewer to be a different
	// agent than the author.
	FactCheckIndependenceAgent = "agent"
	// FactCheckIndependenceModel also requires a different model id. Default.
	FactCheckIndependenceModel = "model"
	// FactCheckIndependenceFamily also requires a different model family.
	FactCheckIndependenceFamily = "family"

	// FactCheckReadyHuman leaves the draft-to-ready call to a person. Default.
	FactCheckReadyHuman = "human"
	// FactCheckReadyHive marks the PR ready for review after a passing round.
	FactCheckReadyHive = "hive"

	// DefaultFactCheckMaxRounds bounds the author/reviewer rounds per PR.
	DefaultFactCheckMaxRounds = 3
	// MaxFactCheckMaxRounds is the highest round limit accepted.
	MaxFactCheckMaxRounds = 10
	// DefaultFactCheckRevisionWait is how long the author has to push a
	// revision after findings before the PR is handed to a person.
	DefaultFactCheckRevisionWait = 6 * time.Hour
	// MaxFactCheckRubricLen bounds review.analysis_factcheck.rubric.
	MaxFactCheckRubricLen = 4000
)

// DefaultFactCheckAuthoritativeFiles are the files an analysis' statements
// about people, roles and approvals are checked against when
// authoritative_files is unset.
var DefaultFactCheckAuthoritativeFiles = []string{
	"MAINTAINERS", "MAINTAINERS.md", "OWNERS", "CODEOWNERS", ".github/CODEOWNERS",
	".project/maintainers.yaml", "GOVERNANCE.md",
}

// AnalysisFactCheckConfig is review.analysis_factcheck.
type AnalysisFactCheckConfig struct {
	// Enabled turns the pass on. Off by default: nothing changes for a hive
	// that does not set it.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// AuthorAgents names the agents whose PRs qualify (for example the agent
	// that produces docs analyses). A PR also qualifies by Labels.
	AuthorAgents []string `yaml:"author_agents,omitempty" json:"author_agents,omitempty"`
	// Labels names PR labels that qualify a PR an agent opened.
	Labels []string `yaml:"labels,omitempty" json:"labels,omitempty"`
	// ReviewerAgents names the agents that may run the pass, in preference
	// order. Unset means review.reviewer_agents, else the agents with the
	// reviewer role. The author is never eligible, whatever this lists.
	ReviewerAgents []string `yaml:"reviewer_agents,omitempty" json:"reviewer_agents,omitempty"`
	// ReviewerModel optionally pins the model the review runs with (the
	// reviewer launches a sub-agent with it). Independence is judged against
	// it when set, else against the reviewer agent's own model.
	ReviewerModel string `yaml:"reviewer_model,omitempty" json:"reviewer_model,omitempty"`
	// Independence is how different the reviewer must be from the author:
	// agent, model (default) or family. A reviewer that cannot be shown to
	// meet it is not used; the PR is handed to a person instead.
	Independence string `yaml:"independence,omitempty" json:"independence,omitempty"`
	// MaxRounds caps the reviewer rounds per PR. Zero means the default (3).
	MaxRounds int `yaml:"max_rounds,omitempty" json:"max_rounds,omitempty"`
	// ReadyPolicy is who moves the PR out of draft after a passing round:
	// human (default) or hive.
	ReadyPolicy string `yaml:"ready_policy,omitempty" json:"ready_policy,omitempty"`
	// RevisionWait is a Go duration: how long the author has to push a
	// revision after findings. Unset means 6h.
	RevisionWait string `yaml:"revision_wait,omitempty" json:"revision_wait,omitempty"`
	// AuthoritativeFiles lists the repo files that are the source of truth
	// for people, roles and approvals. Unset means the built-in list.
	AuthoritativeFiles []string `yaml:"authoritative_files,omitempty" json:"authoritative_files,omitempty"`
	// Rubric is extra domain guidance appended to the reviewer's brief (for
	// example where the scoring rubric lives). Treated as the operator's own
	// text, bounded by MaxFactCheckRubricLen.
	Rubric string `yaml:"rubric,omitempty" json:"rubric,omitempty"`
}

// EffectiveIndependence resolves independence, defaulting to model.
func (c AnalysisFactCheckConfig) EffectiveIndependence() string {
	switch strings.ToLower(strings.TrimSpace(c.Independence)) {
	case FactCheckIndependenceAgent:
		return FactCheckIndependenceAgent
	case FactCheckIndependenceFamily:
		return FactCheckIndependenceFamily
	default:
		return FactCheckIndependenceModel
	}
}

// EffectiveReadyPolicy resolves ready_policy, defaulting to human.
func (c AnalysisFactCheckConfig) EffectiveReadyPolicy() string {
	if strings.EqualFold(strings.TrimSpace(c.ReadyPolicy), FactCheckReadyHive) {
		return FactCheckReadyHive
	}
	return FactCheckReadyHuman
}

// EffectiveMaxRounds resolves max_rounds.
func (c AnalysisFactCheckConfig) EffectiveMaxRounds() int {
	if c.MaxRounds <= 0 {
		return DefaultFactCheckMaxRounds
	}
	return c.MaxRounds
}

// EffectiveRevisionWait resolves revision_wait; an unset, zero or unparsable
// value means the default.
func (c AnalysisFactCheckConfig) EffectiveRevisionWait() time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(c.RevisionWait))
	if err != nil || d <= 0 {
		return DefaultFactCheckRevisionWait
	}
	return d
}

// EffectiveAuthoritativeFiles resolves authoritative_files.
func (c AnalysisFactCheckConfig) EffectiveAuthoritativeFiles() []string {
	if files := trimmedNonEmpty(c.AuthoritativeFiles); len(files) > 0 {
		return files
	}
	return append([]string(nil), DefaultFactCheckAuthoritativeFiles...)
}

// Qualifiers returns the trimmed author agents and labels.
func (c AnalysisFactCheckConfig) Qualifiers() (agents, labels []string) {
	return trimmedNonEmpty(c.AuthorAgents), trimmedNonEmpty(c.Labels)
}

// Validate rejects a block that would enable the pass with nothing that can
// qualify a PR, or with a value outside its vocabulary. A disabled block is
// always valid so an operator can stage the settings.
func (c AnalysisFactCheckConfig) Validate() error {
	if v := strings.ToLower(strings.TrimSpace(c.Independence)); v != "" &&
		v != FactCheckIndependenceAgent && v != FactCheckIndependenceModel && v != FactCheckIndependenceFamily {
		return fmt.Errorf("review.analysis_factcheck: independence %q must be agent, model or family", c.Independence)
	}
	if v := strings.ToLower(strings.TrimSpace(c.ReadyPolicy)); v != "" &&
		v != FactCheckReadyHuman && v != FactCheckReadyHive {
		return fmt.Errorf("review.analysis_factcheck: ready_policy %q must be human or hive", c.ReadyPolicy)
	}
	if c.MaxRounds < 0 || c.MaxRounds > MaxFactCheckMaxRounds {
		return fmt.Errorf("review.analysis_factcheck: max_rounds %d out of range (0-%d; 0 means the default %d)",
			c.MaxRounds, MaxFactCheckMaxRounds, DefaultFactCheckMaxRounds)
	}
	if raw := strings.TrimSpace(c.RevisionWait); raw != "" {
		if _, err := time.ParseDuration(raw); err != nil {
			return fmt.Errorf("review.analysis_factcheck: revision_wait %q is not a duration (e.g. 6h)", c.RevisionWait)
		}
	}
	if len(c.Rubric) > MaxFactCheckRubricLen {
		return fmt.Errorf("review.analysis_factcheck: rubric is %d bytes, over the limit of %d", len(c.Rubric), MaxFactCheckRubricLen)
	}
	if !c.Enabled {
		return nil
	}
	agents, labels := c.Qualifiers()
	if len(agents) == 0 && len(labels) == 0 {
		return fmt.Errorf("review.analysis_factcheck: enabled but neither author_agents nor labels is set, so no PR would qualify")
	}
	return nil
}
