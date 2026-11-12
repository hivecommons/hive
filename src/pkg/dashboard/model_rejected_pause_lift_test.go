package dashboard

import (
	"net/http"
	"testing"
)

// An operator's model or backend change must lift the pause the runtime puts
// on an agent whose CLI rejected its model (crux/agent ModelRejectedTrigger),
// and no other pause. On the old code every one of these handlers restarted
// through Restart, which keeps any pause, so the agent stayed parked on the
// corrected model until someone resumed it by hand.

// mrpModelRejectedTrigger is crux/agent.ModelRejectedTrigger.
const mrpModelRejectedTrigger = "model-rejected"

const mrpRejectReason = "model claude-sonnet-5.5 rejected by the CLI 3 times in a row; set a valid model for this agent (the change resumes it)"

func mrpAssertUnpaused(t *testing.T, deps *Dependencies, what string) {
	t.Helper()
	st, err := deps.AgentMgr.GetStatus("scanner")
	if err != nil {
		t.Fatal(err)
	}
	if st.Paused || st.PausedTrigger != "" {
		t.Fatalf("%s: paused=%v trigger=%q, want the model-rejected pause lifted", what, st.Paused, st.PausedTrigger)
	}
}

func TestModelSet_LiftsModelRejectedPause(t *testing.T) {
	s, deps := apiServer(t)
	if err := deps.AgentMgr.Pause("scanner", mrpModelRejectedTrigger, mrpRejectReason); err != nil {
		t.Fatal(err)
	}
	rec := doPost(s, "/api/model/scanner/opus", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	mrpAssertUnpaused(t, deps, "POST /api/model")
}

func TestSwitchBackend_LiftsModelRejectedPause(t *testing.T) {
	s, deps := apiServer(t)
	if err := deps.AgentMgr.Pause("scanner", mrpModelRejectedTrigger, mrpRejectReason); err != nil {
		t.Fatal(err)
	}
	rec := doPost(s, "/api/switch/scanner/gemini", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	mrpAssertUnpaused(t, deps, "POST /api/switch")
}

func TestAgentConfigModels_LiftsModelRejectedPause(t *testing.T) {
	s, deps := apiServer(t)
	if err := deps.AgentMgr.Pause("scanner", mrpModelRejectedTrigger, mrpRejectReason); err != nil {
		t.Fatal(err)
	}
	rec := doPut(s, "/api/config/agent/scanner/models", map[string]any{"model": "opus"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	mrpAssertUnpaused(t, deps, "PUT /api/config/agent/{name}/models")
}

func TestAgentConfigModels_BackendChangeLiftsModelRejectedPause(t *testing.T) {
	s, deps := apiServer(t)
	if err := deps.AgentMgr.Pause("scanner", mrpModelRejectedTrigger, mrpRejectReason); err != nil {
		t.Fatal(err)
	}
	rec := doPut(s, "/api/config/agent/scanner/models", map[string]any{"backend": "gemini", "model": "gemini-2.5-pro"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	mrpAssertUnpaused(t, deps, "PUT .../models with a backend change")
}

// Positive control for the other half of the rule: the operator's own pause
// is not the runtime's model-rejection park, and a model change keeps it.
func TestAgentConfigModels_OperatorPauseStays(t *testing.T) {
	s, deps := apiServer(t)
	if err := deps.AgentMgr.PauseBy("scanner", "dashboard-api", "manual pause", "owner"); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		name string
		do   func() int
	}{
		{"PUT .../models", func() int {
			return doPut(s, "/api/config/agent/scanner/models", map[string]any{"model": "opus"}).Code
		}},
		{"POST /api/model", func() int { return doPost(s, "/api/model/scanner/opus", nil).Code }},
		{"POST /api/switch", func() int { return doPost(s, "/api/switch/scanner/gemini", nil).Code }},
	} {
		if code := step.do(); code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", step.name, code)
		}
		st, err := deps.AgentMgr.GetStatus("scanner")
		if err != nil {
			t.Fatal(err)
		}
		if !st.Paused || st.PausedTrigger != "dashboard-api" || st.PausedBy != "owner" {
			t.Fatalf("%s: paused=%v trigger=%q by=%q, want the operator pause kept", step.name, st.Paused, st.PausedTrigger, st.PausedBy)
		}
	}
}
