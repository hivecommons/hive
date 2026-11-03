package config

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func experimentLabTestConfig() *Config {
	return &Config{
		Project: ProjectConfig{Org: "o", Repos: []string{"r"}},
		GitHub:  GitHubConfig{Token: "t"},
		AgentRuntimeConfig: AgentRuntimeConfig{
			Agents: map[string]AgentConfig{"a": {Backend: "claude"}},
		},
	}
}

// TestDeprecatedStrategyLabAliasEnablesExperimentLab covers the one-release
// alias window from the #11585 decision: dashboard.strategy_lab: true keeps
// the panel on, is folded into dashboard.experiment_lab, and logs one line.
func TestDeprecatedStrategyLabAliasEnablesExperimentLab(t *testing.T) {
	deprecatedStrategyLabWarned = false
	t.Cleanup(func() { deprecatedStrategyLabWarned = false })

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	cfg := experimentLabTestConfig()
	if err := yaml.Unmarshal([]byte("dashboard:\n  strategy_lab: true\n"), cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cfg.applyDefaults()

	if !cfg.Dashboard.ExperimentLab {
		t.Fatal("dashboard.strategy_lab: true did not enable dashboard.experiment_lab")
	}
	if cfg.Dashboard.StrategyLab {
		t.Fatal("deprecated dashboard.strategy_lab survived migration; it must not be re-saved")
	}
	if !strings.Contains(buf.String(), "dashboard.strategy_lab is deprecated") {
		t.Fatalf("expected a deprecation line in the log, got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "dashboard.experiment_lab") {
		t.Fatalf("deprecation line must name the replacement key, got %q", buf.String())
	}

	// A second applyDefaults (config reload) must not repeat the warning.
	buf.Reset()
	cfg.Dashboard.StrategyLab = true
	cfg.applyDefaults()
	if buf.Len() != 0 {
		t.Fatalf("deprecation warning repeated on reload: %q", buf.String())
	}

	out, err := yaml.Marshal(cfg.Dashboard)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "strategy_lab") {
		t.Fatalf("re-saved dashboard config still carries strategy_lab: %s", out)
	}
	if !strings.Contains(string(out), "experiment_lab: true") {
		t.Fatalf("re-saved dashboard config lacks experiment_lab: true: %s", out)
	}
}

func TestExperimentLabKeyWinsWithoutWarning(t *testing.T) {
	deprecatedStrategyLabWarned = false
	t.Cleanup(func() { deprecatedStrategyLabWarned = false })

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	cfg := experimentLabTestConfig()
	if err := yaml.Unmarshal([]byte("dashboard:\n  experiment_lab: true\n"), cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cfg.applyDefaults()

	if !cfg.Dashboard.ExperimentLab {
		t.Fatal("dashboard.experiment_lab: true was not honoured")
	}
	if strings.Contains(buf.String(), "deprecated") {
		t.Fatalf("documented key must not log a deprecation, got %q", buf.String())
	}
}

// The alias can only ever turn the panel on: an explicit experiment_lab: false
// alongside strategy_lab: true still enables it, matching the health
// migration's "copy an explicit opt-in forward" rule rather than inventing a
// precedence between a deprecated key and its replacement.
func TestDeprecatedStrategyLabAliasNeverDisables(t *testing.T) {
	deprecatedStrategyLabWarned = true
	t.Cleanup(func() { deprecatedStrategyLabWarned = false })

	cfg := experimentLabTestConfig()
	if err := yaml.Unmarshal([]byte("dashboard:\n  experiment_lab: false\n  strategy_lab: true\n"), cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cfg.applyDefaults()
	if !cfg.Dashboard.ExperimentLab {
		t.Fatal("strategy_lab: true should enable the panel even with experiment_lab: false")
	}
}
