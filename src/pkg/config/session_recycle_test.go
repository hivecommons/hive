package config

import (
	"strings"
	"testing"
	"time"

	cruxconfig "github.com/hivecommons/hive/crux/config"
	"gopkg.in/yaml.v3"
)

func TestSessionRecycleFleetBlockDecodesAndStampsAgents(t *testing.T) {
	raw := `
agents:
  session_recycle:
    max_rss_mb: 2048
    max_age: 4h
  scanner:
    enabled: true
  quality:
    enabled: true
    session_recycle:
      enabled: false
  disabled:
    enabled: false
`
	var cfg Config
	if err := yaml.NewDecoder(strings.NewReader(raw)).Decode(&cfg); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if cfg.AgentsSessionRecycle.MaxRSSMB != 2048 || cfg.AgentsSessionRecycle.MaxAge != 4*time.Hour {
		t.Fatalf("fleet block = %+v", cfg.AgentsSessionRecycle)
	}
	if _, leaked := cfg.Agents["session_recycle"]; leaked {
		t.Fatal("session_recycle leaked into the agents map as an agent")
	}
	enabled := cfg.EnabledAgents()
	if len(enabled) != 2 {
		t.Fatalf("EnabledAgents = %d agents, want 2", len(enabled))
	}
	scanner := enabled["scanner"].SessionRecycle
	if scanner == nil || scanner.MaxRSSMB != 2048 || scanner.MaxAge != 4*time.Hour {
		t.Fatalf("scanner did not inherit the fleet block: %+v", scanner)
	}
	if eff := scanner.Effective(); eff.MinIdle != cruxconfig.DefaultSessionRecycleMinIdle || !scanner.IsEnabled() {
		t.Fatalf("scanner effective = %+v", eff)
	}
	quality := enabled["quality"].SessionRecycle
	if quality == nil || quality.IsEnabled() {
		t.Fatalf("quality override lost: %+v", quality)
	}
	// Stamping is a copy, not a shared pointer.
	scanner.MaxRSSMB = 1
	if cfg.AgentsSessionRecycle.MaxRSSMB != 2048 {
		t.Fatal("stamped pointer aliases the fleet value")
	}
}

func TestSessionRecycleAbsentBlockLeavesAgentsNil(t *testing.T) {
	var cfg Config
	if err := yaml.NewDecoder(strings.NewReader("agents:\n  scanner:\n    enabled: true\n")).Decode(&cfg); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got := cfg.EnabledAgents()["scanner"].SessionRecycle; got != nil {
		t.Fatalf("absent fleet block stamped %+v", got)
	}
	if !cfg.EnabledAgents()["scanner"].SessionRecycle.IsEnabled() {
		t.Fatal("nil policy must read as enabled with defaults")
	}
}

func TestSessionRecycleRoundTripsThroughMarshal(t *testing.T) {
	cfg := Config{AgentRuntimeConfig: AgentRuntimeConfig{
		Agents:               map[string]AgentConfig{"scanner": {Enabled: true}},
		AgentsSessionRecycle: SessionRecycleConfig{MaxRSSMB: 900, MinIdle: 5 * time.Minute},
	}}
	cfg.Project.Org = "org"
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(out), "session_recycle:") || !strings.Contains(string(out), "max_rss_mb: 900") {
		t.Fatalf("fleet block not encoded:\n%s", out)
	}
	var back Config
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.AgentsSessionRecycle.MaxRSSMB != 900 || back.AgentsSessionRecycle.MinIdle != 5*time.Minute {
		t.Fatalf("round trip = %+v", back.AgentsSessionRecycle)
	}
	if _, leaked := back.Agents["session_recycle"]; leaked {
		t.Fatal("session_recycle decoded as an agent after round trip")
	}

	// A zero fleet block encodes nothing under agents.
	cfg.AgentsSessionRecycle = SessionRecycleConfig{}
	out, err = yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), "session_recycle") {
		t.Fatalf("zero fleet block encoded:\n%s", out)
	}
}
