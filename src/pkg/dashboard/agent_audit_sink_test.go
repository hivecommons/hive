package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"gopkg.in/natefinch/lumberjack.v2"
)

// TestAgentAuditSinkSatisfiesInterface pins the dependency-inversion contract:
// the dashboard adapter must remain assignable to agent.AuditSink, which is
// what lets cmd/hive inject it without pkg/agent importing pkg/dashboard.
func TestAgentAuditSinkSatisfiesInterface(t *testing.T) {
	var _ agent.AuditSink = &agentAuditSink{}

	s := &Server{audit: &AuditLog{}}
	if s.AgentAuditSink() == nil {
		t.Fatal("AgentAuditSink returned nil")
	}
}

func TestAgentAuditSinkNilSafe(t *testing.T) {
	var s *agentAuditSink
	s.Record("system", "agent_started", "a1", nil) // must not panic

	(&agentAuditSink{}).Record("system", "agent_started", "a1", nil)
}

// TestAgentAuditSinkRendersLifecycleEntry checks the entry lands in the store
// in the shape the Audit Log UI renders: action in Action, agent name in
// Agent (so it is filterable per agent), and the structured fields flattened
// into Detail.
func TestAgentAuditSinkRendersLifecycleEntry(t *testing.T) {
	log := &AuditLog{}
	sink := &agentAuditSink{log: log}

	sink.Record("system", agent.AuditAgentStartFailed, "ci-maintainer", map[string]any{
		"outcome": "failure",
		"backend": "watsonx",
		"model":   "granite-3",
		"error":   "unknown backend: watsonx",
	})

	entries := log.Recent(10)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Action != agent.AuditAgentStartFailed {
		t.Errorf("Action = %q, want %q", e.Action, agent.AuditAgentStartFailed)
	}
	if e.Agent != "ci-maintainer" {
		t.Errorf("Agent = %q, want ci-maintainer", e.Agent)
	}
	if e.User != "system" {
		t.Errorf("User = %q, want system", e.User)
	}
	for _, want := range []string{"outcome=failure", "backend=watsonx", "model=granite-3", "unknown backend: watsonx"} {
		if !strings.Contains(e.Detail, want) {
			t.Errorf("Detail %q missing %q", e.Detail, want)
		}
	}
}

// TestAgentAuditSinkSurvivesPersistReload is the durability half of the ask:
// a lifecycle event must still be there after the hive restarts, since the
// watsonx agent failed for a day across many launch attempts.
func TestAgentAuditSinkSurvivesPersistReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	log := &AuditLog{writer: &lumberjack.Logger{Filename: path}}
	sink := &agentAuditSink{log: log}
	sink.Record("system", agent.AuditAgentStartFailed, "ci-maintainer", map[string]any{
		"outcome": "failure",
		"backend": "watsonx",
		"model":   "granite-3",
		"error":   "unknown backend: watsonx",
	})
	if err := log.writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted audit log: %v", err)
	}
	line := strings.TrimSpace(string(data))
	if line == "" {
		t.Fatal("nothing persisted to disk")
	}

	// Reload the way loadFromDisk does, and confirm the event round-trips
	// intact — including the backend/model/error that make it actionable.
	var reloaded AuditEntry
	if err := json.Unmarshal([]byte(line), &reloaded); err != nil {
		t.Fatalf("unmarshal persisted entry: %v", err)
	}
	if reloaded.Action != agent.AuditAgentStartFailed {
		t.Errorf("Action = %q, want %q", reloaded.Action, agent.AuditAgentStartFailed)
	}
	if reloaded.Agent != "ci-maintainer" {
		t.Errorf("Agent = %q, want ci-maintainer", reloaded.Agent)
	}
	if !strings.Contains(reloaded.Detail, "backend=watsonx") ||
		!strings.Contains(reloaded.Detail, "model=granite-3") ||
		!strings.Contains(reloaded.Detail, "unknown backend: watsonx") {
		t.Errorf("Detail lost context across the round trip: %q", reloaded.Detail)
	}
	if reloaded.Timestamp == "" {
		t.Error("Timestamp did not survive the round trip")
	}
}
