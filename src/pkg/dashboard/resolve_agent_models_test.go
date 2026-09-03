package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/tokens"
)

// TestResolveAgentModels_AutoConfigResolvesToRealModel is the core BUG A case:
// an agent whose config model is "auto" runs sessions logged under a concrete
// resolved model (e.g. copilot "auto" → gpt-5.3-codex). resolveAgentModels must
// return the resolved model so the frontend badge lands on that row.
func TestResolveAgentModels_AutoConfigResolvesToRealModel(t *testing.T) {
	sessions := []tokens.SessionSummary{
		{SessionID: "s1", Agent: "scanner", Model: "gpt-5.3-codex", LastActive: 100},
	}
	got := resolveAgentModels(sessions)
	if got["scanner"] != "gpt-5.3-codex" {
		t.Fatalf("scanner resolved model = %q, want gpt-5.3-codex", got["scanner"])
	}
}

// TestResolveAgentModels_PicksLatestActive verifies that when an agent has
// multiple qualifying sessions, the one with the greatest LastActive wins.
func TestResolveAgentModels_PicksLatestActive(t *testing.T) {
	sessions := []tokens.SessionSummary{
		{SessionID: "old", Agent: "scanner", Model: "gpt-5.2-codex", LastActive: 50},
		{SessionID: "new", Agent: "scanner", Model: "gpt-5.3-codex", LastActive: 200},
		{SessionID: "mid", Agent: "scanner", Model: "claude-opus-4-8", LastActive: 120},
	}
	got := resolveAgentModels(sessions)
	if got["scanner"] != "gpt-5.3-codex" {
		t.Fatalf("scanner resolved model = %q, want gpt-5.3-codex (latest)", got["scanner"])
	}
}

// TestResolveAgentModels_SkipsAliasAndUnknown verifies alias/placeholder models
// never win, and that a real earlier session is chosen over a later alias one.
func TestResolveAgentModels_SkipsAliasAndUnknown(t *testing.T) {
	sessions := []tokens.SessionSummary{
		{SessionID: "real", Agent: "scanner", Model: "gpt-5.3-codex", LastActive: 100},
		{SessionID: "alias", Agent: "scanner", Model: "auto", LastActive: 300},
		{SessionID: "def", Agent: "scanner", Model: "default", LastActive: 400},
		{SessionID: "unk", Agent: "scanner", Model: "unknown", LastActive: 500},
		{SessionID: "empty", Agent: "scanner", Model: "", LastActive: 600},
		{SessionID: "ws", Agent: "scanner", Model: "  AUTO  ", LastActive: 700},
	}
	got := resolveAgentModels(sessions)
	if got["scanner"] != "gpt-5.3-codex" {
		t.Fatalf("scanner resolved model = %q, want gpt-5.3-codex (aliases skipped)", got["scanner"])
	}
}

// TestResolveAgentModels_NoQualifyingSession verifies an agent with only alias
// sessions is absent from the map (frontend then falls back to the config alias).
func TestResolveAgentModels_NoQualifyingSession(t *testing.T) {
	sessions := []tokens.SessionSummary{
		{SessionID: "a", Agent: "scanner", Model: "auto", LastActive: 100},
		{SessionID: "b", Agent: "scanner", Model: "", LastActive: 200},
	}
	got := resolveAgentModels(sessions)
	if _, ok := got["scanner"]; ok {
		t.Fatalf("scanner should be absent when it has no concrete-model session, got %q", got["scanner"])
	}
}

// TestResolveAgentModels_MultipleAgents verifies independent resolution and that
// an empty-agent session is ignored.
func TestResolveAgentModels_MultipleAgents(t *testing.T) {
	sessions := []tokens.SessionSummary{
		{SessionID: "s1", Agent: "scanner", Model: "gpt-5.3-codex", LastActive: 100},
		{SessionID: "s2", Agent: "reviewer", Model: "claude-opus-4-8", LastActive: 100},
		{SessionID: "s3", Agent: "", Model: "gpt-5.3-codex", LastActive: 100},
	}
	got := resolveAgentModels(sessions)
	if got["scanner"] != "gpt-5.3-codex" {
		t.Errorf("scanner = %q, want gpt-5.3-codex", got["scanner"])
	}
	if got["reviewer"] != "claude-opus-4-8" {
		t.Errorf("reviewer = %q, want claude-opus-4-8", got["reviewer"])
	}
	if len(got) != 2 {
		t.Errorf("map size = %d, want 2 (empty-agent session ignored)", len(got))
	}
}

// TestResolveAgentModels_Empty verifies no panic and an empty (non-nil) result.
func TestResolveAgentModels_Empty(t *testing.T) {
	got := resolveAgentModels(nil)
	if got == nil {
		t.Fatal("expected non-nil map")
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %v", got)
	}
}
