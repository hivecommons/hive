package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeGateConfig(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSafetyGateDiffRecovery(t *testing.T) {
	dir := t.TempDir()
	seed := writeGateConfig(t, dir, "seed", "project: {org: example}\n")
	priorBody := `review: {require_labels: [hive-review]}
project:
  org: example
  paused_repos: [{repo: held, by: operator, reason: private-reason}]
  issue_filter: {require_labels: [accepted]}
writeback:
  per_repo:
    example/held: {mode: off}
github: {token: private-token}
`
	for _, name := range []string{"hive.yaml.runtime", "hive.yaml.runtime.bak", "hive.yaml.bak", "hive.yaml.dashboard"} {
		t.Run(name, func(t *testing.T) {
			prior := writeGateConfig(t, dir, name, priorBody)
			warnings, strict, err := SafetyGateDiff(seed, []string{prior})
			if err != nil || strict || len(warnings) != 4 {
				t.Fatalf("warnings=%v strict=%v err=%v", warnings, strict, err)
			}
			joined := strings.Join(warnings, "\n")
			for _, key := range []string{"review.require_labels", "project.paused_repos", "project.issue_filter.require_labels", "writeback.per_repo.example/held.mode", "hive-review", prior} {
				if !strings.Contains(joined, key) {
					t.Errorf("missing %q in %s", key, joined)
				}
			}
			if strings.Contains(joined, "private-") {
				t.Fatal("diagnostics exposed unrelated config or pause metadata")
			}
			// Comparison is read-only even when recovery discovers a regression.
			got, err := os.ReadFile(prior)
			if err != nil || string(got) != priorBody {
				t.Fatal("prior config was modified")
			}
		})
	}
}

func TestSafetyGateDiffPreservedAndStronger(t *testing.T) {
	dir := t.TempDir()
	prior := writeGateConfig(t, dir, "prior", `review: {require_labels: [hive-review]}
project:
  org: Example
  paused_repos: [{repo: Held}]
  issue_filter: {require_labels: [accepted]}
writeback: {per_repo: {example/held: {mode: issues-only}}}
`)
	effective := writeGateConfig(t, dir, "effective", `review: {require_labels: [hive-review]}
project:
  org: example
  paused_repos: [{repo: example/held, reason: changed}, {repo: another}]
  issue_filter: {require_labels: [accepted]}
writeback: {per_repo: {Example/Held: {mode: off}}}
`)
	warnings, _, err := SafetyGateDiff(effective, []string{prior})
	if err != nil || len(warnings) != 0 {
		t.Fatalf("%v, %v", warnings, err)
	}
}

func TestSafetyGateDiffPartialLossAndFirstBoot(t *testing.T) {
	dir := t.TempDir()
	effective := writeGateConfig(t, dir, "effective", "project: {org: org, paused_repos: [{repo: kept}]}\nwriteback: {per_repo: {org/kept: {mode: off}}}\n")
	warnings, _, err := SafetyGateDiff(effective, []string{filepath.Join(dir, "missing")})
	if err != nil || len(warnings) != 0 {
		t.Fatalf("first boot: %v, %v", warnings, err)
	}
	prior := writeGateConfig(t, dir, "prior", "project: {org: org, paused_repos: [{repo: kept}, {repo: lost}]}\nwriteback: {per_repo: {org/kept: {mode: off}, org/lost: {mode: labels-only}}}\n")
	warnings, _, err = SafetyGateDiff(effective, []string{prior})
	if err != nil || len(warnings) != 2 {
		t.Fatalf("partial loss: %v, %v", warnings, err)
	}
}

func TestSafetyGateDiffInvalidBaselineAndStrictPersistence(t *testing.T) {
	dir := t.TempDir()
	effective := writeGateConfig(t, dir, "effective", "config: {refuse_gate_regression: true}\n")
	prior := writeGateConfig(t, dir, "prior", "review: [broken\nprivate-token")
	warnings, strict, err := SafetyGateDiff(effective, []string{prior})
	if err != nil || !strict || len(warnings) != 1 || strings.Contains(warnings[0], "private-token") {
		t.Fatalf("%v, %v, %v", warnings, strict, err)
	}
	var cfg Config
	if err := yaml.Unmarshal([]byte("config: {refuse_gate_regression: true}"), &cfg); err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil || !strings.Contains(string(data), "refuse_gate_regression: true") {
		t.Fatalf("strict mode lost on save: %v", err)
	}
	t.Setenv(BootSafetyWarningsEnv, `["safety gate dropped: review.require_labels"]`)
	if got := BootSafetyWarnings(); len(got) != 1 {
		t.Fatalf("boot evidence lost: %v", got)
	}
}
