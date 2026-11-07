package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCLIMemoryFleetBlockDecodesAndStampsAgents(t *testing.T) {
	raw := `
agents:
  cli_memory:
    auto_memory: true
    clear_on_kick: true
  scanner:
    enabled: true
  quality:
    enabled: true
  disabled:
    enabled: false
`
	var cfg Config
	if err := yaml.NewDecoder(strings.NewReader(raw)).Decode(&cfg); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	want := CLIMemoryConfig{AutoMemory: true, ClearOnKick: true}
	if cfg.AgentsCLIMemory != want {
		t.Fatalf("fleet block = %+v, want %+v", cfg.AgentsCLIMemory, want)
	}
	if _, leaked := cfg.Agents["cli_memory"]; leaked {
		t.Fatal("cli_memory leaked into the agents map as an agent")
	}
	enabled := cfg.EnabledAgents()
	if len(enabled) != 2 {
		t.Fatalf("enabled agents = %d, want 2", len(enabled))
	}
	for name, ac := range enabled {
		if ac.CLIMemory != want {
			t.Fatalf("%s did not inherit the fleet block: %+v", name, ac.CLIMemory)
		}
	}
}

func TestCLIMemoryAbsentBlockIsDefaultOff(t *testing.T) {
	var cfg Config
	if err := yaml.NewDecoder(strings.NewReader("agents:\n  scanner:\n    enabled: true\n")).Decode(&cfg); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got := cfg.EnabledAgents()["scanner"].CLIMemory; got != (CLIMemoryConfig{}) {
		t.Fatalf("absent block resolved to %+v, want auto-memory off and no clear", got)
	}
}

func TestCLIMemoryInvalidBlockIsAnError(t *testing.T) {
	var cfg Config
	err := yaml.NewDecoder(strings.NewReader("agents:\n  cli_memory:\n    clear_on_kick: sometimes\n")).Decode(&cfg)
	if err == nil || !strings.Contains(err.Error(), "agents.cli_memory") {
		t.Fatalf("Decode err = %v, want an agents.cli_memory error", err)
	}
}

func TestCLIMemoryRoundTripsThroughMarshal(t *testing.T) {
	cfg := Config{AgentRuntimeConfig: AgentRuntimeConfig{
		Agents:          map[string]AgentConfig{"scanner": {Enabled: true}},
		AgentsCLIMemory: CLIMemoryConfig{ClearOnKick: true},
	}}
	cfg.Project.Org = "org"
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(out), "cli_memory:") || !strings.Contains(string(out), "clear_on_kick: true") {
		t.Fatalf("fleet block not encoded:\n%s", out)
	}
	var back Config
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.AgentsCLIMemory != (CLIMemoryConfig{ClearOnKick: true}) {
		t.Fatalf("round trip = %+v", back.AgentsCLIMemory)
	}
	if _, leaked := back.Agents["cli_memory"]; leaked {
		t.Fatal("cli_memory decoded as an agent after round trip")
	}

	// The default block encodes nothing under agents.
	cfg.AgentsCLIMemory = CLIMemoryConfig{}
	out, err = yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), "cli_memory") {
		t.Fatalf("default block encoded:\n%s", out)
	}
}
