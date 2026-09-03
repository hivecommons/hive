package governor

import (
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// NoCadenceAgents (#5577) names the silent-idle class: enabled, kickable
// agents with no cadence in ANY mode and no kick ever. Everything an operator
// deliberately configured — a cadence anywhere (including "off"), on-demand,
// event-only channels, disabled — must NOT be flagged.
func TestNoCadenceAgents(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	// telemetry: enabled, kickable, in NO mode's cadence map, never kicked —
	// the flagged shape (the live case behind the RFC).
	agents["telemetry"] = config.AgentConfig{Enabled: true}
	// disabled agent with no cadences: operator switched it off, not flagged.
	agents["mothballed"] = config.AgentConfig{Enabled: false}
	// on-demand agent: kicked manually by design, not flagged.
	agents["helper"] = config.AgentConfig{Enabled: true, OnDemand: true}
	// event-driven agent (non-kick channels): governor timer is not its
	// trigger, not flagged.
	agents["watcher"] = config.AgentConfig{Enabled: true, Channels: []config.ChannelConfig{{Type: "advisory"}}}
	// explicit "off" cadence: configured choice, not omission — not flagged.
	agents["parked"] = config.AgentConfig{Enabled: true}
	cfg.Modes["idle"].Cadences["parked"] = "off"

	g := New(cfg, agents, testLogger())

	got := g.NoCadenceAgents()
	if want := []string{"telemetry"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("NoCadenceAgents = %v, want %v", got, want)
	}
}

// A kick from ANY path (manual, CEL, resume) clears the agent: "never kicked
// ever" is half the definition, so RecordKick must remove it.
func TestNoCadenceAgents_ClearedByAnyKick(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	agents["telemetry"] = config.AgentConfig{Enabled: true}
	g := New(cfg, agents, testLogger())

	g.RecordKick("telemetry")

	if got := g.NoCadenceAgents(); len(got) != 0 {
		t.Fatalf("kicked agent still flagged: %v", got)
	}
}

// The result is always a measurement: non-nil even when empty, so the
// heartbeat serializes [] (clearing the hub's carry-forward) rather than null
// ("not measured").
func TestNoCadenceAgents_EmptyIsNonNil(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	g := New(cfg, agents, testLogger())

	got := g.NoCadenceAgents()
	if got == nil {
		t.Fatal("NoCadenceAgents returned nil; must be a non-nil measurement")
	}
	if len(got) != 0 {
		t.Fatalf("scheduled agent flagged: %v", got)
	}
}

// A replica inherits its base agent's cadence (resolveCadence's fallback), so
// a replica whose BASE is scheduled must not be flagged.
func TestNoCadenceAgents_ReplicaUsesBaseCadence(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	agents["scanner-2"] = config.AgentConfig{Enabled: true, ReplicaOf: "scanner", ReplicaIndex: 2, ReplicaCount: 2}
	g := New(cfg, agents, testLogger())

	if got := g.NoCadenceAgents(); len(got) != 0 {
		t.Fatalf("replica of a scheduled base flagged: %v", got)
	}
}
