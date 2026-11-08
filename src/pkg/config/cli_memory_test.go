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
    clear_on_kick: true
  scanner:
    enabled: true
  archivist:
    enabled: true
    cli_memory:
      auto_memory: true
  disabled:
    enabled: false
`
	var cfg Config
	if err := yaml.NewDecoder(strings.NewReader(raw)).Decode(&cfg); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !cfg.AgentsCLIMemory.ClearOnKick || cfg.AgentsCLIMemory.AutoMemory != nil {
		t.Fatalf("fleet block = %+v", cfg.AgentsCLIMemory)
	}
	if _, leaked := cfg.Agents["cli_memory"]; leaked {
		t.Fatal("cli_memory leaked into the agents map as an agent")
	}
	enabled := cfg.EnabledAgents()
	scanner := enabled["scanner"].CLIMemory
	if scanner == nil || !scanner.ClearMemoryOnKick() || scanner.AutoMemoryEnabled() {
		t.Fatalf("scanner did not inherit the fleet block: %+v", scanner)
	}
	archivist := enabled["archivist"].CLIMemory
	if archivist == nil || !archivist.AutoMemoryEnabled() || archivist.ClearMemoryOnKick() {
		t.Fatalf("archivist override lost: %+v", archivist)
	}
	scanner.ClearOnKick = false
	if !cfg.AgentsCLIMemory.ClearOnKick {
		t.Fatal("stamped pointer aliases the fleet value")
	}
}

func TestCLIMemoryDefaultsOffWhenAbsent(t *testing.T) {
	var cfg Config
	if err := yaml.NewDecoder(strings.NewReader("agents:\n  scanner:\n    enabled: true\n")).Decode(&cfg); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got := cfg.EnabledAgents()["scanner"].CLIMemory
	if got != nil {
		t.Fatalf("absent fleet block stamped %+v", got)
	}
	if got.AutoMemoryEnabled() || got.ClearMemoryOnKick() {
		t.Fatal("nil policy must read as auto-memory off, no wipe")
	}
}

func TestCLIMemoryRoundTripsThroughMarshal(t *testing.T) {
	on := true
	cfg := Config{AgentRuntimeConfig: AgentRuntimeConfig{
		Agents:          map[string]AgentConfig{"scanner": {Enabled: true}},
		AgentsCLIMemory: CLIMemoryConfig{AutoMemory: &on, ClearOnKick: true},
	}}
	cfg.Project.Org = "org"
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(out), "cli_memory:") || !strings.Contains(string(out), "auto_memory: true") || !strings.Contains(string(out), "clear_on_kick: true") {
		t.Fatalf("fleet block not encoded:\n%s", out)
	}
	var back Config
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !back.AgentsCLIMemory.AutoMemoryEnabled() || !back.AgentsCLIMemory.ClearOnKick {
		t.Fatalf("round trip = %+v", back.AgentsCLIMemory)
	}
	if _, leaked := back.Agents["cli_memory"]; leaked {
		t.Fatal("cli_memory decoded as an agent after round trip")
	}
	cfg.AgentsCLIMemory = CLIMemoryConfig{}
	out, err = yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), "cli_memory") {
		t.Fatalf("zero fleet block encoded:\n%s", out)
	}
}

func TestCLIMemoryDrainOnWriteNeedsDrainToKnowledge(t *testing.T) {
	raw := `
agents:
  cli_memory:
    drain_to_knowledge: true
    drain_on_write: true
  scanner:
    enabled: true
  builder:
    enabled: true
    cli_memory:
      drain_on_write: true
`
	var cfg Config
	if err := yaml.NewDecoder(strings.NewReader(raw)).Decode(&cfg); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !cfg.AgentsCLIMemory.DrainOnWrite || cfg.AgentsCLIMemory.IsZero() {
		t.Fatalf("fleet block = %+v", cfg.AgentsCLIMemory)
	}
	enabled := cfg.EnabledAgents()
	if !enabled["scanner"].CLIMemory.DrainMemoryOnWrite() {
		t.Fatal("scanner must inherit the fleet drain_on_write")
	}
	if enabled["builder"].CLIMemory.DrainMemoryOnWrite() {
		t.Fatal("drain_on_write without drain_to_knowledge must stay off")
	}
	var none *CLIMemoryConfig
	if none.DrainMemoryOnWrite() {
		t.Fatal("nil policy must read as off")
	}
}
