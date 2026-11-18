package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestAnalysisFactCheckDefaults(t *testing.T) {
	var c AnalysisFactCheckConfig
	if c.Enabled {
		t.Fatal("the pass is off by default")
	}
	if got := c.EffectiveIndependence(); got != FactCheckIndependenceModel {
		t.Errorf("independence default = %q", got)
	}
	if got := c.EffectiveReadyPolicy(); got != FactCheckReadyHuman {
		t.Errorf("ready policy default = %q", got)
	}
	if got := c.EffectiveMaxRounds(); got != DefaultFactCheckMaxRounds {
		t.Errorf("max rounds default = %d", got)
	}
	if got := c.EffectiveRevisionWait(); got != DefaultFactCheckRevisionWait {
		t.Errorf("revision wait default = %s", got)
	}
	if got := c.EffectiveAuthoritativeFiles(); len(got) != len(DefaultFactCheckAuthoritativeFiles) {
		t.Errorf("authoritative files default = %v", got)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("a disabled empty block is valid: %v", err)
	}
}

func TestAnalysisFactCheckEffectiveValues(t *testing.T) {
	tests := []struct {
		name string
		cfg  AnalysisFactCheckConfig
		ind  string
		rdy  string
		max  int
		wait time.Duration
	}{
		{"explicit family and hive", AnalysisFactCheckConfig{Independence: " Family ", ReadyPolicy: "HIVE", MaxRounds: 5, RevisionWait: "90m"},
			FactCheckIndependenceFamily, FactCheckReadyHive, 5, 90 * time.Minute},
		{"agent level", AnalysisFactCheckConfig{Independence: "agent"}, FactCheckIndependenceAgent, FactCheckReadyHuman, DefaultFactCheckMaxRounds, DefaultFactCheckRevisionWait},
		{"unknown values fall back", AnalysisFactCheckConfig{Independence: "bogus", ReadyPolicy: "bogus", MaxRounds: -1, RevisionWait: "soon"},
			FactCheckIndependenceModel, FactCheckReadyHuman, DefaultFactCheckMaxRounds, DefaultFactCheckRevisionWait},
		{"non-positive wait falls back", AnalysisFactCheckConfig{RevisionWait: "-1h"}, FactCheckIndependenceModel, FactCheckReadyHuman, DefaultFactCheckMaxRounds, DefaultFactCheckRevisionWait},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.EffectiveIndependence(); got != tt.ind {
				t.Errorf("independence = %q want %q", got, tt.ind)
			}
			if got := tt.cfg.EffectiveReadyPolicy(); got != tt.rdy {
				t.Errorf("ready = %q want %q", got, tt.rdy)
			}
			if got := tt.cfg.EffectiveMaxRounds(); got != tt.max {
				t.Errorf("max = %d want %d", got, tt.max)
			}
			if got := tt.cfg.EffectiveRevisionWait(); got != tt.wait {
				t.Errorf("wait = %s want %s", got, tt.wait)
			}
		})
	}
}

func TestAnalysisFactCheckValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     AnalysisFactCheckConfig
		wantErr string
	}{
		{"enabled with an agent", AnalysisFactCheckConfig{Enabled: true, AuthorAgents: []string{"quality"}}, ""},
		{"enabled with a label", AnalysisFactCheckConfig{Enabled: true, Labels: []string{"analysis"}}, ""},
		{"enabled with nothing that qualifies", AnalysisFactCheckConfig{Enabled: true}, "no PR would qualify"},
		{"enabled with blank qualifiers", AnalysisFactCheckConfig{Enabled: true, AuthorAgents: []string{" "}, Labels: []string{""}}, "no PR would qualify"},
		{"disabled staging is valid", AnalysisFactCheckConfig{AuthorAgents: []string{"quality"}}, ""},
		{"bad independence", AnalysisFactCheckConfig{Independence: "sameish"}, "independence"},
		{"bad ready policy", AnalysisFactCheckConfig{ReadyPolicy: "robot"}, "ready_policy"},
		{"rounds too many", AnalysisFactCheckConfig{MaxRounds: MaxFactCheckMaxRounds + 1}, "max_rounds"},
		{"rounds negative", AnalysisFactCheckConfig{MaxRounds: -1}, "max_rounds"},
		{"rounds at the cap", AnalysisFactCheckConfig{MaxRounds: MaxFactCheckMaxRounds}, ""},
		{"bad wait", AnalysisFactCheckConfig{RevisionWait: "tomorrow"}, "revision_wait"},
		{"rubric too long", AnalysisFactCheckConfig{Rubric: strings.Repeat("x", MaxFactCheckRubricLen+1)}, "rubric"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("error = %v, want one mentioning %q", err, tt.wantErr)
			}
		})
	}
}

func TestAnalysisFactCheckYAMLKeys(t *testing.T) {
	const doc = `
review:
  analysis_factcheck:
    enabled: true
    author_agents: [quality]
    labels: [analysis]
    reviewer_agents: [reviewer]
    reviewer_model: gpt-6.1-sol
    independence: family
    max_rounds: 4
    ready_policy: hive
    revision_wait: 3h
    authoritative_files: [MAINTAINERS]
    rubric: use the rubric
`
	var cfg struct {
		Review ReviewConfig `yaml:"review"`
	}
	if err := yaml.Unmarshal([]byte(doc), &cfg); err != nil {
		t.Fatal(err)
	}
	fc := cfg.Review.AnalysisFactCheck
	if !fc.Enabled || fc.AuthorAgents[0] != "quality" || fc.Labels[0] != "analysis" || fc.ReviewerAgents[0] != "reviewer" ||
		fc.ReviewerModel != "gpt-6.1-sol" || fc.EffectiveIndependence() != FactCheckIndependenceFamily ||
		fc.EffectiveMaxRounds() != 4 || fc.EffectiveReadyPolicy() != FactCheckReadyHive ||
		fc.EffectiveRevisionWait() != 3*time.Hour || fc.EffectiveAuthoritativeFiles()[0] != "MAINTAINERS" || fc.Rubric != "use the rubric" {
		t.Fatalf("yaml keys did not bind: %+v", fc)
	}
	if err := fc.Validate(); err != nil {
		t.Fatal(err)
	}
}
