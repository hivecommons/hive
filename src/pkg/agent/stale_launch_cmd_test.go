package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// A launch_cmd pinned to a backend binary path that does not exist (the
// dashboard's old "/usr/bin/<cli>" rewrite, #10509) must fall back to the
// built-in launcher; every other launch_cmd is the operator's and is kept.
func TestStaleLaunchCmdBinary(t *testing.T) {
	dir := t.TempDir()
	missingBob := filepath.Join(dir, "missing", "bob")
	presentBob := filepath.Join(dir, "bob")
	if err := os.WriteFile(presentBob, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	missingClaude := filepath.Join(dir, "missing", "claude")
	missingWrapper := filepath.Join(dir, "missing", "my-launcher.sh")

	cases := []struct {
		name      string
		launchCmd string
		backend   string
		wantPath  string
		wantStale bool
	}{
		{"issue 10509: missing bob path", missingBob + " --allow-all --model claude-sonnet-4-6", "bob", missingBob, true},
		{"present bob path", presentBob + " chat", "bob", "", false},
		{"bare binary resolves via PATH", "bob chat", "bob", "", false},
		{"wrapper form", "agent-launch.sh --backend bob", "bob", "", false},
		{"missing custom wrapper is not a backend binary", missingWrapper, "bob", "", false},
		{"other backend's binary", missingBob, "copilot", "", false},
		{"inference backend execs claude", missingClaude + " --model x", "vllm", missingClaude, true},
		{"unknown backend", missingBob, "nonesuch", "", false},
		{"empty", "", "bob", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, stale := staleLaunchCmdBinary(tc.launchCmd, tc.backend)
			if stale != tc.wantStale || got != tc.wantPath {
				t.Errorf("staleLaunchCmdBinary(%q, %q) = (%q, %v), want (%q, %v)",
					tc.launchCmd, tc.backend, got, stale, tc.wantPath, tc.wantStale)
			}
		})
	}
}

// The kick and resume paths classify an agy agent from the same effective
// launch_cmd the launch path uses, so a stale saved path stays headless.
func TestAgentUsesAgyHeadlessStaleLaunchCmd(t *testing.T) {
	t.Setenv(agyLaunchModeEnv, "")
	missingAgy := filepath.Join(t.TempDir(), "missing", "agy")
	cases := []struct {
		name      string
		launchCmd string
		backend   string
		want      bool
	}{
		{"no launch_cmd", "", "agy", true},
		{"stale absolute agy path", missingAgy + " --model x", "agy", true},
		{"operator wrapper", "agent-launch.sh --backend agy", "agy", false},
		{"missing custom wrapper", filepath.Join(filepath.Dir(missingAgy), "my-launcher.sh"), "agy", false},
		{"other backend", "", "claude", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := &AgentProcess{Config: config.AgentConfig{LaunchCmd: tc.launchCmd}}
			if got := agentUsesAgyHeadless(tc.backend, agent); got != tc.want {
				t.Errorf("agentUsesAgyHeadless(%q, launch_cmd=%q) = %v, want %v", tc.backend, tc.launchCmd, got, tc.want)
			}
		})
	}
}
