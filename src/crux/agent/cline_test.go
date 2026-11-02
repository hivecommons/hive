package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/cline"
	"github.com/hivecommons/hive/crux/config"
)

func TestClineLaunchAndObservation(t *testing.T) {
	cmd := backendLaunchCmd("cline", "openrouter/anthropic/test", "cline", false, "high")
	if !strings.Contains(cmd, cline.ReadyMarker) || !backendDefersStartupKick("cline") {
		t.Fatal(cmd)
	}
	if !paneShowsInputPrompt(cline.ReadyMarker+"> ") || !paneHasCLIMarker(cline.ReadyMarker) {
		t.Fatal("Cline ready not classified")
	}
	if !paneHasCLIMarker(strings.Repeat(cline.RunningMarker+" usage\n", 40)) {
		t.Fatal("long-running Cline lost its live marker when readiness scrolled away")
	}
	if !paneShowsAgentWorking(cline.ReadyMarker + "\n" + cline.RunningMarker) {
		t.Fatal("Cline turn not busy")
	}
	if paneShowsAgentWorking(cline.RunningMarker + "\n" + cline.ReadyMarker) {
		t.Fatal("completed Cline turn stays busy")
	}
	line := clineTurnCommand("/own binary", "openrouter/anthropic/test", "high", "/private prompt")
	for _, want := range []string{"cline-turn", "--model 'openrouter/anthropic/test'", "--thinking 'high'", "--prompt-file '/private prompt'"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %s in %s", want, line)
		}
	}
}

func TestClineRefusesUntranslatablePolicy(t *testing.T) {
	agent := &AgentProcess{Config: config.AgentConfig{Model: "openrouter/test", Tools: &config.ToolsConfig{Preset: "read-only"}}}
	if err := validateClineLaunch(context.Background(), "must-not-execute", agent); err == nil || !strings.Contains(err.Error(), "policy") {
		t.Fatal(err)
	}

	agent.Config.Tools = nil
	agent.Config.Connections = []config.ConnectionConfig{{Type: "mcp", URI: "https://must-not-use.invalid"}}
	if err := validateClineLaunch(context.Background(), "must-not-execute", agent); err == nil {
		t.Fatal("connections silently dropped")
	}
	agent.Config.Connections = nil
	agent.Config.Model = "auto"
	if err := validateClineLaunch(context.Background(), "must-not-execute", agent); err == nil || !strings.Contains(err.Error(), "provider/model") {
		t.Fatal("implicit provider accepted")
	}
	agent.Config.Model = "openrouter/test"
	agent.Config.LaunchCmd = "cline --yolo"
	if err := validateClineLaunch(context.Background(), "must-not-execute", agent); err == nil || !strings.Contains(err.Error(), "launch_cmd") {
		t.Fatal("override bypassed canonical runner")
	}
}

func TestClineSandboxRefusesMissingCredentialBoundary(t *testing.T) {
	for _, command := range []string{"", "cline --yolo"} {
		if _, err := sandboxCommand(configSnapshot{Backend: "cline", Model: "openrouter/test", LaunchCmd: command}, "prompt.txt"); err == nil || !strings.Contains(err.Error(), "provider-file") {
			t.Fatal("sandbox silently substituted a different engine or bypassed credential policy", err)
		}
	}
}
