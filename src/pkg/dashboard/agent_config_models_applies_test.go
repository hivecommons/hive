package dashboard

import (
	"net/http"
	"testing"
)

// Regression for hivecommons/hive#7374: PUT /api/config/agent/{name}/models
// persisted the new model but never applied it to the LIVE launch
// configuration. launchInTmux prefers agent.ModelOverride (set by /api/model,
// a pin, or the governor) over agent.Config.Model, and this endpoint only
// refreshed Config — so "persist, then restart" respawned the agent on the
// OLD model with a 200 and a correct-looking overlay file.
//
// The repro shape: six agents pinned to gpt-4o-mini-2024-07-18 through the
// override path, then corrected to "auto" through the config endpoint.
func TestHandleAgentConfigModels_AppliesOverStaleOverride(t *testing.T) {
	s, deps := apiServer(t)
	if err := deps.AgentMgr.SetModelOverride("scanner", "gpt-4o-mini-2024-07-18"); err != nil {
		t.Fatal(err)
	}
	if err := deps.AgentMgr.SetBackendOverride("scanner", "copilot"); err != nil {
		t.Fatal(err)
	}

	rec := doPut(s, "/api/config/agent/scanner/models", map[string]any{"backend": "copilot", "model": "auto"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	if resp["applied"] != true {
		t.Errorf("response must report the change as applied: %v", resp)
	}
	if _, ok := resp["restarted"]; !ok {
		t.Errorf("response must say whether the agent was restarted: %v", resp)
	}

	// Config persisted (this part always worked)…
	if got := deps.Config.Agents["scanner"].Model; got != "auto" {
		t.Errorf("config model = %q, want auto", got)
	}
	// …AND the value the next launch will actually use.
	proc, err := deps.AgentMgr.GetStatus("scanner")
	if err != nil {
		t.Fatal(err)
	}
	if proc.ModelOverride != "auto" {
		t.Errorf("live ModelOverride = %q, want auto — a restart would relaunch on the old model (#7374)", proc.ModelOverride)
	}
	if proc.Config.Model != "auto" {
		t.Errorf("agent.Config.Model = %q, want auto", proc.Config.Model)
	}
}

// A backend change through the same endpoint must win over a stale backend
// override for the same reason.
func TestHandleAgentConfigModels_BackendAppliesOverStaleOverride(t *testing.T) {
	s, deps := apiServer(t)
	if err := deps.AgentMgr.SetBackendOverride("scanner", "copilot"); err != nil {
		t.Fatal(err)
	}
	rec := doPut(s, "/api/config/agent/scanner/models", map[string]any{"backend": "gemini", "model": "gemini-2.5-pro"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	proc, err := deps.AgentMgr.GetStatus("scanner")
	if err != nil {
		t.Fatal(err)
	}
	if proc.BackendOverride != "gemini" || proc.ModelOverride != "gemini-2.5-pro" {
		t.Errorf("live overrides = (%q, %q), want (gemini, gemini-2.5-pro)", proc.BackendOverride, proc.ModelOverride)
	}
}

// A request that changes nothing launch-relevant (same model the agent
// already runs, no backend, no effort) must not restart the agent: the
// endpoint is also how the dialog re-saves an unchanged form.
func TestHandleAgentConfigModels_NoChangeNoRestart(t *testing.T) {
	s, deps := apiServer(t)
	if err := deps.AgentMgr.SetModelOverride("scanner", "sonnet"); err != nil {
		t.Fatal(err)
	}
	before, _ := deps.AgentMgr.GetStatus("scanner")

	rec := doPut(s, "/api/config/agent/scanner/models", map[string]any{"model": "sonnet"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	if resp["restarted"] != false || resp["status"] != "updated" {
		t.Errorf("an unchanged model must not restart: %v", resp)
	}
	after, _ := deps.AgentMgr.GetStatus("scanner")
	if after.RestartCount != before.RestartCount {
		t.Errorf("RestartCount moved %d → %d on a no-op save", before.RestartCount, after.RestartCount)
	}
}
