package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// #12973: all runner probes passed, but one jobs API 502 made the watchdog
// report an unhealthy pool. Check the action inputs that enable its built-in
// request retries; removing them must restore the failing zero-retry policy.
func TestCIRunnerCanaryRetriesTransientAPIFailures(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "ci-runner-canary.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string `yaml:"uses"`
				With struct {
					Retries     int    `yaml:"retries"`
					ExemptCodes string `yaml:"retry-exempt-status-codes"`
				} `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(body, &wf); err != nil {
		t.Fatal(err)
	}
	for _, step := range wf.Jobs["watchdog"].Steps {
		if !strings.HasPrefix(step.Uses, "actions/github-script@") {
			continue
		}
		if step.With.Retries < 1 || step.With.Retries > 3 {
			t.Fatalf("watchdog retries = %d; need a bounded retry budget of 1–3", step.With.Retries)
		}
		exempt := make(map[string]bool)
		for _, code := range strings.Split(step.With.ExemptCodes, ",") {
			exempt[strings.TrimSpace(code)] = true
		}
		for _, code := range []string{"500", "502", "503", "504"} {
			if exempt[code] {
				t.Errorf("transient HTTP %s must be eligible for retry", code)
			}
		}
		for _, code := range []string{"400", "401", "403", "404", "422"} {
			if !exempt[code] {
				t.Errorf("non-retryable HTTP %s must remain exempt", code)
			}
		}
		return
	}
	t.Fatal("watchdog GitHub API step missing")
}

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
