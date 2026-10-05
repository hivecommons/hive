package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// #10518: a missing npm fails the PATH probe before the integrity step.
// GitHub implicitly applies success() unless a status function is present,
// so the repair must explicitly run even after an earlier step fails.
func TestCIRunnerCanaryRepairsCacheAfterFailure(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "ci-runner-canary.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				If   string `yaml:"if"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(body, &wf); err != nil {
		t.Fatal(err)
	}
	for _, step := range wf.Jobs["canary"].Steps {
		if step.Name != "Tool cache integrity" {
			continue
		}
		if step.If != "${{ always() }}" {
			t.Fatalf("integrity step condition = %q; must run after failed setup/PATH checks", step.If)
		}
		cache := t.TempDir()
		dir := filepath.Join(cache, "node", "22.23.3", "x64")
		if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o700); err != nil {
			t.Fatal(err)
		}
		// Reproduce the live cache: node exists, but npm/npx are missing.
		if err := os.WriteFile(filepath.Join(dir, "bin", "node"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		marker := dir + ".complete"
		if err := os.WriteFile(marker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", "-e", "-c", step.Run)
		cmd.Env = append(os.Environ(), "RUNNER_TOOL_CACHE="+cache)
		if output, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("broken cache must keep the canary red: %s", output)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("broken cache marker was not invalidated: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "bin", "node")); err != nil {
			t.Fatalf("repair must not delete files used by other jobs: %v", err)
		}
		return
	}
	t.Fatal("Tool cache integrity step missing")
}
