package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// hivecommons/hive#6488: the ACMM pack filter in buildAgents exists to hide
// GHOSTS — pack-materialized agents left over from another level (1360b72f).
// It also swallowed every operator-defined CUSTOM agent (a config name no pack
// ever materializes), so a running, token-consuming agent rendered no card in
// the dashboard's Agents section. These tests pin the corrected boundary:
// custom config agents always show; other-level pack names stay hidden;
// current-level pack agents are unaffected.

func packFilterProc(name string) *agent.AgentProcess {
	return &agent.AgentProcess{
		Name:         name,
		Config:       config.AgentConfig{Backend: "claude", Enabled: true},
		State:        agent.StateRunning,
		OutputBuffer: agent.NewRingBuffer(10),
	}
}

func buildAgentNames(t *testing.T, cfg *config.Config, procs ...*agent.AgentProcess) map[string]bool {
	t.Helper()
	statuses := make(map[string]*agent.AgentProcess, len(procs))
	for _, p := range procs {
		statuses[p.Name] = p
	}
	out := make(map[string]bool)
	for _, a := range buildAgents(statuses, cfg, governor.State{Mode: governor.ModeIdle}) {
		out[a.Name] = true
	}
	return out
}

func TestBuildAgents_CustomConfigAgentShowsAtAnyPackLevel(t *testing.T) {
	level := 1
	custom := packFilterProc("mybot")
	cfg := &config.Config{
		ACMMLevel: &level,
		Agents:    map[string]config.AgentConfig{custom.Name: custom.Config},
	}
	names := buildAgentNames(t, cfg, custom)
	if !names["mybot"] {
		t.Fatalf("custom config agent %q missing from buildAgents output: %v", "mybot", names)
	}
}

func TestBuildAgents_OtherLevelPackGhostStaysHidden(t *testing.T) {
	// "supervisor" is a pack agent from level 2+; at level 1 it is a ghost of
	// another level even when a config entry lingers, and must stay hidden.
	level := 1
	ghost := packFilterProc("supervisor")
	cfg := &config.Config{
		ACMMLevel: &level,
		Agents:    map[string]config.AgentConfig{ghost.Name: ghost.Config},
	}
	names := buildAgentNames(t, cfg, ghost)
	if names["supervisor"] {
		t.Fatalf("other-level pack agent %q should be filtered at level 1: %v", "supervisor", names)
	}
}

func TestBuildAgents_NonConfigNonPackAgentStaysHidden(t *testing.T) {
	// A runtime-roster name that is neither in config nor in the current pack
	// has no operator intent behind it — keep it out of the payload.
	level := 1
	stray := packFilterProc("stray")
	cfg := &config.Config{
		ACMMLevel: &level,
		Agents:    map[string]config.AgentConfig{},
	}
	names := buildAgentNames(t, cfg, stray)
	if names["stray"] {
		t.Fatalf("non-config non-pack agent %q should be filtered: %v", "stray", names)
	}
}

func TestBuildAgents_CurrentPackAgentUnaffected(t *testing.T) {
	level := 2
	sup := packFilterProc("supervisor")
	custom := packFilterProc("mybot")
	cfg := &config.Config{
		ACMMLevel: &level,
		Agents: map[string]config.AgentConfig{
			sup.Name:    sup.Config,
			custom.Name: custom.Config,
		},
	}
	names := buildAgentNames(t, cfg, sup, custom)
	if !names["supervisor"] || !names["mybot"] {
		t.Fatalf("want both supervisor and mybot visible at level 2, got: %v", names)
	}
}

func TestPackAgentNames_CoversAllLevels(t *testing.T) {
	names := config.PackAgentNames()
	for _, want := range []string{"supervisor", "guide", "scanner"} {
		if !names[want] {
			t.Fatalf("PackAgentNames missing %q: %v", want, names)
		}
	}
	if names["mybot"] {
		t.Fatal("PackAgentNames should not contain custom names")
	}
}
