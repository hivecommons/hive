package config

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// BootSafetyWarningsEnv carries the pre-snapshot comparison across the
// entrypoint's privilege-drop re-exec. It contains only gate diagnostics,
// never config contents or credentials, and lasts for this process's boot.
const BootSafetyWarningsEnv = "HIVE_BOOT_SAFETY_WARNINGS"

// ConfigLoadPolicy controls startup diagnostics; it does not change gate enforcement.
type ConfigLoadPolicy struct {
	RefuseGateRegression bool `yaml:"refuse_gate_regression,omitempty" json:"refuse_gate_regression,omitempty"`
}

// BootSafetyWarnings returns warnings captured before the entrypoint replaced
// the runtime snapshot. Reading them here keeps reloads from erasing boot evidence.
func BootSafetyWarnings() []string {
	var warnings []string
	_ = json.Unmarshal([]byte(os.Getenv(BootSafetyWarningsEnv)), &warnings)
	return warnings
}

// gateSnapshot deliberately reads only safety settings. Recovery diagnostics
// must work even when an unrelated agent field makes a full config invalid.
type gateSnapshot struct {
	Config ConfigLoadPolicy `yaml:"config"`
	Review struct {
		RequireLabels []string `yaml:"require_labels"`
	} `yaml:"review"`
	Project struct {
		Org         string `yaml:"org"`
		PausedRepos []struct {
			Repo string `yaml:"repo"`
		} `yaml:"paused_repos"`
		IssueFilter struct {
			RequireLabels []string `yaml:"require_labels"`
		} `yaml:"issue_filter"`
	} `yaml:"project"`
	Writeback struct {
		PerRepo map[string]struct {
			Mode string `yaml:"mode"`
		} `yaml:"per_repo"`
	} `yaml:"writeback"`
}

func readGateSnapshot(path string) (gateSnapshot, error) {
	var cfg gateSnapshot
	data, err := os.ReadFile(path)
	if err == nil {
		err = yaml.Unmarshal([]byte(expandEnvVars(string(data))), &cfg)
	}
	return cfg, err
}

// SafetyGateDiff compares an effective boot config against all surviving prior
// layers, before any of them are overwritten. Missing layers mean first boot;
// unreadable or malformed layers are explicit diagnostics rather than silence.
// The bool is the effective config's strict-mode setting.
func SafetyGateDiff(path string, previous []string) ([]string, bool, error) {
	effective, err := readGateSnapshot(path)
	if err != nil {
		return nil, false, fmt.Errorf("reading effective safety gates: %w", err)
	}
	warnings := []string{}
	for _, priorPath := range previous {
		prior, err := readGateSnapshot(priorPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			// Do not print parser errors: they can contain unrelated secret values.
			warnings = append(warnings, fmt.Sprintf("safety gate comparison unavailable: %s (unreadable or invalid YAML)", priorPath))
			continue
		}
		dropped := func(key string, was any) {
			value, _ := json.Marshal(was)
			warnings = append(warnings, fmt.Sprintf("safety gate dropped: %s (was %s) — effective config has none or a weaker restriction (previous: %s)", key, value, priorPath))
		}
		if len(nonemptyLabels(prior.Review.RequireLabels)) > 0 && len(nonemptyLabels(effective.Review.RequireLabels)) == 0 {
			dropped("review.require_labels", prior.Review.RequireLabels)
		}
		if len(nonemptyLabels(prior.Project.IssueFilter.RequireLabels)) > 0 && len(nonemptyLabels(effective.Project.IssueFilter.RequireLabels)) == 0 {
			dropped("project.issue_filter.require_labels", prior.Project.IssueFilter.RequireLabels)
		}
		paused := map[string]bool{}
		for _, p := range effective.Project.PausedRepos {
			paused[repoPauseKey(effective.Project.Org, p.Repo)] = true
		}
		for _, p := range prior.Project.PausedRepos {
			if p.Repo != "" && !paused[repoPauseKey(prior.Project.Org, p.Repo)] {
				dropped("project.paused_repos", p.Repo)
			}
		}
		modes := map[string]string{}
		for repo, override := range effective.Writeback.PerRepo {
			modes[strings.ToLower(strings.TrimSpace(repo))] = override.Mode
		}
		repos := make([]string, 0, len(prior.Writeback.PerRepo))
		for repo := range prior.Writeback.PerRepo {
			repos = append(repos, repo)
		}
		sort.Strings(repos)
		for _, repo := range repos {
			mode := prior.Writeback.PerRepo[repo].Mode
			current := modes[strings.ToLower(strings.TrimSpace(repo))]
			// off is strictly stronger than either partial-write mode. The partial
			// modes allow different kinds, so switching between them is a delta too.
			if (mode == "off" || mode == "issues-only" || mode == "labels-only") && current != mode && current != "off" {
				dropped("writeback.per_repo."+repo+".mode", mode)
			}
		}
	}
	return warnings, effective.Config.RefuseGateRegression, nil
}

func nonemptyLabels(labels []string) []string {
	var out []string
	for _, label := range labels {
		if strings.TrimSpace(label) != "" {
			out = append(out, label)
		}
	}
	return out
}
