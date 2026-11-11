package dashboard

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
)

// A PUT that names only the backend the agent already runs on still makes
// the saved config the launch config. Before, the handler compared the
// request with the live overrides, saw no change, and left a stale model
// override in place: the agent kept launching on it, and it was persisted
// to hive-state again.
func TestHandleAgentConfigModels_BackendOnlyPutReplacesStaleModelOverride(t *testing.T) {
	s, deps := apiServer(t)
	cfg := deps.Config.Agents["scanner"]
	cfg.Model = "claude-sonnet-5-5"
	deps.Config.Agents["scanner"] = cfg
	if err := deps.AgentMgr.UpdateConfig("scanner", cfg); err != nil {
		t.Fatal(err)
	}
	if err := deps.AgentMgr.SetBackendOverride("scanner", "claude"); err != nil {
		t.Fatal(err)
	}
	if err := deps.AgentMgr.SetModelOverride("scanner", "opus"); err != nil {
		t.Fatal(err)
	}

	rec := doPut(s, "/api/config/agent/scanner/models", map[string]any{"backend": "claude"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	proc, err := deps.AgentMgr.GetStatus("scanner")
	if err != nil {
		t.Fatal(err)
	}
	if proc.ModelOverride != "claude-sonnet-5-5" {
		t.Errorf("live ModelOverride = %q, want the saved claude-sonnet-5-5", proc.ModelOverride)
	}
}

// A copilot→claude switch through PUT .../models with no model in the body
// re-spells the saved Copilot id for the Claude CLI, in the config and in
// the live override, so the agent never launches with claude-sonnet-5.5.
func TestHandleAgentConfigModels_CopilotToClaudeRespellsModel(t *testing.T) {
	s, deps := apiServer(t)
	rec := doPut(s, "/api/config/agent/scanner/models", map[string]any{"backend": "copilot", "model": "claude-sonnet-5.5"})
	if rec.Code != http.StatusOK {
		t.Fatalf("copilot save: status = %d: %s", rec.Code, rec.Body.String())
	}

	rec = doPut(s, "/api/config/agent/scanner/models", map[string]any{"backend": "claude"})
	if rec.Code != http.StatusOK {
		t.Fatalf("claude switch: status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := deps.Config.Agents["scanner"].Model; got != "claude-sonnet-5-5" {
		t.Errorf("config model = %q, want claude-sonnet-5-5", got)
	}
	proc, err := deps.AgentMgr.GetStatus("scanner")
	if err != nil {
		t.Fatal(err)
	}
	if proc.BackendOverride != "claude" || proc.ModelOverride != "claude-sonnet-5-5" {
		t.Errorf("live overrides = (%q, %q), want (claude, claude-sonnet-5-5)", proc.BackendOverride, proc.ModelOverride)
	}
}

// A model with no Claude equivalent is cleared on the switch, with a warning,
// rather than kept for the Claude CLI to refuse.
func TestHandleAgentConfigModels_CopilotToClaudeClearsUnrunnableModel(t *testing.T) {
	s, deps := apiServer(t)
	rec := doPut(s, "/api/config/agent/scanner/models", map[string]any{"backend": "copilot", "model": "gpt-5.5"})
	if rec.Code != http.StatusOK {
		t.Fatalf("copilot save: status = %d: %s", rec.Code, rec.Body.String())
	}

	rec = doPut(s, "/api/config/agent/scanner/models", map[string]any{"backend": "claude"})
	if rec.Code != http.StatusOK {
		t.Fatalf("claude switch: status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := deps.Config.Agents["scanner"].Model; got != "" {
		t.Errorf("config model = %q, want it cleared", got)
	}
	proc, _ := deps.AgentMgr.GetStatus("scanner")
	if proc == nil || proc.ModelOverride != "" {
		t.Errorf("live ModelOverride kept a model the Claude CLI cannot run: %+v", proc)
	}
	if w, _ := decodeJSON(t, rec)["warning"].(string); !strings.Contains(w, "gpt-5.5") {
		t.Errorf("response must warn that the model was cleared, got %q", w)
	}
}

// Every Claude id in the Copilot picker list must re-spell to an id the
// Claude backend's pinned list offers, so crux's derived mapping cannot drift
// from the dashboard's lists.
func TestClaudeLaunchModelTargetsInClaudeLists(t *testing.T) {
	for _, id := range copilotPinnedCLIModels {
		if !strings.HasPrefix(id, "claude-") || !strings.Contains(id, ".") {
			continue
		}
		got := crux.ClaudeLaunchModel(id)
		if !contains(claudePinnedCLIModels, got) {
			t.Errorf("ClaudeLaunchModel(%q) = %q, which claudePinnedCLIModels does not list", id, got)
		}
		if !contains(claudeStaticModels, got) {
			t.Errorf("ClaudeLaunchModel(%q) = %q, which claudeStaticModels does not list", id, got)
		}
	}
}
