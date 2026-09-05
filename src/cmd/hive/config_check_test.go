package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCheckConfig lays down a hive.yaml whose data.agents_dir points at a
// sibling overlay directory, and fills that directory with the given files.
// This is the #6024 shape: the contradiction lives in an overlay file, not in
// hive.yaml, so a validator that reads only hive.yaml would call it healthy.
func writeCheckConfig(t *testing.T, overlays map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "agent-configs")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir agents dir: %v", err)
	}
	for name, body := range overlays {
		if err := os.WriteFile(filepath.Join(agentsDir, name+".yaml"), []byte(body), 0o600); err != nil {
			t.Fatalf("writing overlay %s: %v", name, err)
		}
	}
	body := `
project:
  org: my-org
  repos:
    - repo-a
github:
  token: ghp_tok
data:
  agents_dir: ` + agentsDir + `
agents:
  worker:
    backend: claude
    enabled: true
`
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func TestRunConfigCheck_ValidConfigExitsZero(t *testing.T) {
	path := writeCheckConfig(t, nil)

	var stdout, stderr bytes.Buffer
	if code := runConfigCheck([]string{"-config", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("runConfigCheck() = %d, want 0\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "config OK") {
		t.Errorf("stdout does not report success: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "worker") {
		t.Errorf("stdout does not list the agents: %s", stdout.String())
	}
}

// A config that cannot load at all must exit non-zero and say why, rather than
// leaving an operator to discover it by watching a pod crash-loop.
func TestRunConfigCheck_InvalidConfigExitsNonZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	// No github credential and no agents: two hard validate() failures.
	if err := os.WriteFile(path, []byte("project:\n  org: my-org\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runConfigCheck([]string{"-config", path}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("runConfigCheck() = 0, want non-zero for an invalid config\nstdout: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "config INVALID") {
		t.Errorf("stderr does not report the failure: %s", stderr.String())
	}
}

func TestRunConfigCheck_MissingFileExitsNonZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runConfigCheck([]string{"-config", filepath.Join(t.TempDir(), "absent.yaml")}, &stdout, &stderr); code == 0 {
		t.Fatalf("runConfigCheck() = 0, want non-zero for a missing config file")
	}
}

// The whole point of #6024: the check must load the per-agent OVERLAY files
// too. It reports the provenance of overlay-sourced agents so an operator can
// see which entries came from a directory they may not have thought to open.
func TestRunConfigCheck_ReportsOverlayProvenance(t *testing.T) {
	path := writeCheckConfig(t, map[string]string{
		"worker": "backend: claude\nmodel: claude-sonnet-4-6\n",
	})

	var stdout, stderr bytes.Buffer
	if code := runConfigCheck([]string{"-config", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("runConfigCheck() = %d, want 0\nstderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "worker.yaml") {
		t.Errorf("stdout does not name the overlay file the agent came from: %s", stdout.String())
	}
}

// A contradictory overlay is now skipped rather than fatal, so the check
// reports OK - matching what a real boot would do. The skip itself is logged
// at ERROR by the loader; what matters here is that `hive validate` agrees
// with the boot it is standing in for instead of reporting a failure the hive
// would not actually suffer.
func TestRunConfigCheck_ContradictoryOverlayMatchesBootBehaviour(t *testing.T) {
	path := writeCheckConfig(t, map[string]string{
		"supervisor": "backend: copilot\nlaunch_cmd: bob --model auto\n",
	})

	var stdout, stderr bytes.Buffer
	if code := runConfigCheck([]string{"-config", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("runConfigCheck() = %d, want 0 (the bad overlay is skipped, not fatal)\nstderr: %s", code, stderr.String())
	}
	// The rejected overlay must not have contributed an agent.
	if strings.Contains(stdout.String(), "supervisor.yaml") {
		t.Errorf("a rejected overlay was reported as a live source: %s", stdout.String())
	}
}

func TestRunConfigCheck_BadFlagExitsNonZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runConfigCheck([]string{"-nope"}, &stdout, &stderr); code == 0 {
		t.Fatalf("runConfigCheck() = 0, want non-zero for an unknown flag")
	}
}
