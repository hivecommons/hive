package dashboard

import (
	"net/http"
	"testing"
)

// TestHandleAgentConfigModels_AppliesToLaunchConfig is the regression test for
// hivecommons/hive#7374: PUT /api/config/agent/{name}/models persisted the new
// model to hive.yaml and the agent overlay but left the manager's per-agent
// model override — the value the launch command is actually built from —
// untouched, so the next restart respawned the CLI on the OLD model.
func TestHandleAgentConfigModels_AppliesToLaunchConfig(t *testing.T) {
	s, deps := apiServer(t)

	// A stale override, exactly as a card dropdown / auto-heal / governor
	// selection would have left behind.
	if err := deps.AgentMgr.SetModelOverride("scanner", "gpt-4o-mini-2024-07-18"); err != nil {
		t.Fatalf("seed override: %v", err)
	}

	rec := doPut(s, "/api/config/agent/scanner/models",
		map[string]interface{}{"backend": "copilot", "model": "auto"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	if got := deps.Config.Agents["scanner"].Model; got != "auto" {
		t.Fatalf("config model = %q, want %q", got, "auto")
	}

	proc, err := deps.AgentMgr.GetStatus("scanner")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if proc.ModelOverride != "auto" {
		t.Errorf("launch model override = %q, want %q (stale override still shadows the saved model)", proc.ModelOverride, "auto")
	}
	if proc.BackendOverride != "copilot" {
		t.Errorf("launch backend override = %q, want %q", proc.BackendOverride, "copilot")
	}
}

// TestHandleAgentConfigModels_NoChangeLeavesOverrides pins the other half of
// the contract: a PUT that changes nothing must not push a new override (and
// therefore must not restart the agent), so the dialog's save-everything path
// cannot bounce sessions on a no-op save.
func TestHandleAgentConfigModels_NoChangeLeavesOverrides(t *testing.T) {
	s, deps := apiServer(t)

	if err := deps.AgentMgr.SetModelOverride("scanner", "opus"); err != nil {
		t.Fatalf("seed override: %v", err)
	}

	// scanner is configured as claude/sonnet; resend those same values.
	rec := doPut(s, "/api/config/agent/scanner/models",
		map[string]interface{}{"backend": "claude", "model": "sonnet"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	proc, err := deps.AgentMgr.GetStatus("scanner")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if proc.ModelOverride != "opus" {
		t.Errorf("unchanged PUT rewrote the override: got %q, want %q", proc.ModelOverride, "opus")
	}
}
