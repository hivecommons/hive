package main

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/planning"
)

// agentKicker is the adapter handed to the Phase 3 stall-replan lane
// (planning.Kicker). These tests pin its two contracts: it satisfies the
// interface, and Kick is a pure pass-through to Manager.SendKick — errors
// (including "agent not found") surface to the replan lane unchanged rather
// than being swallowed.

var _ planning.Kicker = agentKicker{}

func TestAgentKickerUnknownAgentError(t *testing.T) {
	mgr := agent.NewManager(map[string]config.AgentConfig{}, restoreTestLogger(), agent.ProjectContext{})
	k := agentKicker{mgr: mgr}

	err := k.Kick("ghost", "replan kick")
	if err == nil {
		t.Fatal("Kick on unknown agent: want error, got nil")
	}
	if !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Kick error should name the missing agent: %v", err)
	}
}

func TestAgentKickerNotRunningAgentError(t *testing.T) {
	mgr := agent.NewManager(map[string]config.AgentConfig{
		"idle-agent": {Enabled: true},
	}, restoreTestLogger(), agent.ProjectContext{})
	k := agentKicker{mgr: mgr}

	err := k.Kick("idle-agent", "replan kick")
	if err == nil {
		t.Fatal("Kick on non-running agent: want error, got nil")
	}
	if !strings.Contains(err.Error(), "idle-agent") {
		t.Fatalf("Kick error should name the agent: %v", err)
	}
}
