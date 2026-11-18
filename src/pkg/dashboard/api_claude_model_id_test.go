package dashboard

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

const (
	dottedClaudeModel = "claude-sonnet-5.5"
	dashedClaudeModel = "claude-sonnet-5-5"
)

func findSystemAlert(s *Server, id string) (SystemAlert, bool) {
	s.systemAlertsMu.Lock()
	defer s.systemAlertsMu.Unlock()
	for _, a := range s.systemAlerts {
		if a.ID == id {
			return a, true
		}
	}
	return SystemAlert{}, false
}

func TestCanonicalizeModelForBackend(t *testing.T) {
	got, warning := canonicalizeModelForBackend("architect", config.ClaudeBackend, dottedClaudeModel)
	if got != dashedClaudeModel || !strings.Contains(warning, dottedClaudeModel) || !strings.Contains(warning, dashedClaudeModel) {
		t.Fatalf("claude: got (%q, %q)", got, warning)
	}
	for _, tc := range []struct{ backend, model string }{
		{config.ClaudeBackend, dashedClaudeModel},
		{config.ClaudeBackend, "sonnet"},
		{"copilot", dottedClaudeModel},
		{"openrouter", "anthropic/claude-sonnet-5.5"},
	} {
		if got, warning := canonicalizeModelForBackend("a", tc.backend, tc.model); got != tc.model || warning != "" {
			t.Errorf("%s/%s: got (%q, %q), want unchanged", tc.backend, tc.model, got, warning)
		}
	}
}

// The bug: an agent that ran on copilot with the copilot spelling of its
// model is switched to claude. The switch must rewrite the carried model to
// the Claude CLI spelling and warn, instead of keeping an id the CLI rejects.
func TestHandleSwitch_ToClaudeRewritesCarriedDottedModel(t *testing.T) {
	s, deps := apiServer(t)
	if err := deps.AgentMgr.SetModelOverride("scanner", dottedClaudeModel); err != nil {
		t.Fatalf("SetModelOverride: %v", err)
	}
	ac := deps.Config.Agents["scanner"]
	ac.Model = dottedClaudeModel
	deps.Config.Agents["scanner"] = ac

	rec := doPost(s, "/api/switch/scanner/claude", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody18905(t, rec)
	if w, _ := body["warning"].(string); !strings.Contains(w, dashedClaudeModel) {
		t.Fatalf("response must carry the rewrite warning, got %v", body)
	}
	proc, err := deps.AgentMgr.GetStatus("scanner")
	if err != nil || proc == nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if proc.ModelOverride != dashedClaudeModel {
		t.Fatalf("ModelOverride = %q, want %q", proc.ModelOverride, dashedClaudeModel)
	}
	if got := deps.Config.Agents["scanner"].Model; got != dashedClaudeModel {
		t.Fatalf("config model = %q, want %q", got, dashedClaudeModel)
	}
	if _, ok := findSystemAlert(s, modelIDNormalizedAlertPrefix+"scanner"); !ok {
		t.Fatalf("expected a dashboard warning for the rewrite")
	}
}

func TestHandleSwitch_StoppedAgentToClaudeRewritesDottedModel(t *testing.T) {
	s, dir := notRunningServer(t)
	ac := s.deps.Config.Agents[notRunningTestAgent]
	ac.Backend, ac.Model = "copilot", dottedClaudeModel
	s.deps.Config.Agents[notRunningTestAgent] = ac

	rec := doPost(s, "/api/switch/"+notRunningTestAgent+"/claude", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	got := s.deps.Config.Agents[notRunningTestAgent]
	if got.Backend != config.ClaudeBackend || got.Model != dashedClaudeModel {
		t.Fatalf("config = backend %q model %q, want claude/%s", got.Backend, got.Model, dashedClaudeModel)
	}
	overlay := overlayText(t, dir)
	if !strings.Contains(overlay, dashedClaudeModel) || strings.Contains(overlay, dottedClaudeModel) {
		t.Fatalf("overlay must carry the dashed id only:\n%s", overlay)
	}
}

// Switching to a backend other than claude keeps the model verbatim: the
// dotted id is the valid copilot spelling.
func TestHandleSwitch_ToCopilotKeepsDottedModel(t *testing.T) {
	s, _ := notRunningServer(t)
	ac := s.deps.Config.Agents[notRunningTestAgent]
	ac.Model = dottedClaudeModel
	s.deps.Config.Agents[notRunningTestAgent] = ac

	rec := doPost(s, "/api/switch/"+notRunningTestAgent+"/copilot", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Agents[notRunningTestAgent].Model; got != dottedClaudeModel {
		t.Fatalf("model = %q, want unchanged %q", got, dottedClaudeModel)
	}
	if _, ok := findSystemAlert(s, modelIDNormalizedAlertPrefix+notRunningTestAgent); ok {
		t.Fatalf("no warning expected for the copilot backend")
	}
}

// The Settings model endpoint stores the Claude CLI spelling for a claude
// agent and returns the warning for the UI to show.
func TestHandleAgentConfigModels_NormalizesDottedClaudeModel(t *testing.T) {
	s, deps := apiServer(t)
	rec := doPut(s, "/api/config/agent/scanner/models", map[string]any{"model": dottedClaudeModel})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody18905(t, rec)
	if w, _ := body["warning"].(string); !strings.Contains(w, dashedClaudeModel) {
		t.Fatalf("response must carry the rewrite warning, got %v", body)
	}
	if got := deps.Config.Agents["scanner"].Model; got != dashedClaudeModel {
		t.Fatalf("config model = %q, want %q", got, dashedClaudeModel)
	}
	proc, err := deps.AgentMgr.GetStatus("scanner")
	if err != nil || proc == nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if proc.ModelOverride != dashedClaudeModel {
		t.Fatalf("ModelOverride = %q, want %q", proc.ModelOverride, dashedClaudeModel)
	}

	// A later save with a valid id clears the warning.
	if rec := doPut(s, "/api/config/agent/scanner/models", map[string]any{"model": "opus"}); rec.Code != http.StatusOK {
		t.Fatalf("second save status = %d (%s)", rec.Code, rec.Body.String())
	}
	if _, ok := findSystemAlert(s, modelIDNormalizedAlertPrefix+"scanner"); ok {
		t.Fatalf("warning must clear once a valid id is saved")
	}
}

// Rewrites done by the config loader are surfaced as warnings at startup and
// written to the agent overlay so they do not repeat on every boot.
func TestSurfaceModelIDNormalizations(t *testing.T) {
	s, dir := notRunningServer(t)
	s.deps.Config.ModelIDNormalizations = []config.ModelIDNormalization{
		{Agent: notRunningTestAgent, Backend: config.ClaudeBackend, From: dottedClaudeModel, To: dashedClaudeModel},
	}
	ac := s.deps.Config.Agents[notRunningTestAgent]
	ac.Model = dashedClaudeModel
	s.deps.Config.Agents[notRunningTestAgent] = ac

	s.surfaceModelIDNormalizations()

	a, ok := findSystemAlert(s, modelIDNormalizedAlertPrefix+notRunningTestAgent)
	if !ok || a.Severity != "warning" || !strings.Contains(a.Message, dottedClaudeModel) {
		t.Fatalf("alert = %+v (present %v)", a, ok)
	}
	if !strings.Contains(overlayText(t, dir), dashedClaudeModel) {
		t.Fatalf("overlay must carry the normalized id")
	}
}

// The claude picker list never offers the copilot (dotted) spelling.
func TestCanonicalClaudeModelList(t *testing.T) {
	got := canonicalClaudeModelList([]string{dottedClaudeModel, dashedClaudeModel, "sonnet", "claude-opus-4.6"})
	want := []string{dashedClaudeModel, "sonnet", "claude-opus-4-6"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("canonicalClaudeModelList = %v, want %v", got, want)
	}
}
