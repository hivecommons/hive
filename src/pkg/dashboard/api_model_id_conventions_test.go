package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	for _, tc := range []struct{ backend, from, to string }{
		{config.ClaudeBackend, dottedClaudeModel, dashedClaudeModel},
		{"copilot", dashedClaudeModel, dottedClaudeModel},
		{"copilot", "claude-haiku-4-5", "claude-haiku-4.5"},
	} {
		got, warning := canonicalizeModelForBackend("architect", tc.backend, tc.from)
		if got != tc.to || !strings.Contains(warning, tc.from) || !strings.Contains(warning, tc.to) || !strings.Contains(warning, tc.backend) {
			t.Errorf("%s/%s: got (%q, %q), want %q with a warning", tc.backend, tc.from, got, warning, tc.to)
		}
	}
	for _, tc := range []struct{ backend, model string }{
		{config.ClaudeBackend, dashedClaudeModel},
		{config.ClaudeBackend, "sonnet"},
		{"copilot", dottedClaudeModel},
		{"copilot", "claude-sonnet-5"},
		{"copilot", "gpt-5-mini"},
		{"copilot", "auto"},
		{"openrouter", "anthropic/claude-sonnet-5.5"},
		{"gemini", dashedClaudeModel},
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

// The other direction: an agent on the Claude CLI spelling switched to
// copilot gets the Copilot spelling, with a warning.
func TestHandleSwitch_ToCopilotRewritesDashedModel(t *testing.T) {
	s, dir := notRunningServer(t)
	ac := s.deps.Config.Agents[notRunningTestAgent]
	ac.Model = dashedClaudeModel
	s.deps.Config.Agents[notRunningTestAgent] = ac

	rec := doPost(s, "/api/switch/"+notRunningTestAgent+"/copilot", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody18905(t, rec)
	if w, _ := body["warning"].(string); !strings.Contains(w, dottedClaudeModel) {
		t.Fatalf("response must carry the rewrite warning, got %v", body)
	}
	if got := s.deps.Config.Agents[notRunningTestAgent]; got.Backend != "copilot" || got.Model != dottedClaudeModel {
		t.Fatalf("config = backend %q model %q, want copilot/%s", got.Backend, got.Model, dottedClaudeModel)
	}
	if !strings.Contains(overlayText(t, dir), dottedClaudeModel) {
		t.Fatalf("overlay must carry the dotted id")
	}
	if _, ok := findSystemAlert(s, modelIDNormalizedAlertPrefix+notRunningTestAgent); !ok {
		t.Fatalf("expected a dashboard warning for the rewrite")
	}
}

// Ids already valid for the target backend, and non-Claude ids, are kept
// verbatim on a switch, with no warning.
func TestHandleSwitch_KeepsValidAndNonClaudeModels(t *testing.T) {
	for _, tc := range []struct{ backend, model string }{
		{"copilot", dottedClaudeModel},
		{"copilot", "gpt-5-mini"},
		{"copilot", "claude-sonnet-5"},
		{config.ClaudeBackend, dashedClaudeModel},
	} {
		t.Run(tc.backend+"/"+tc.model, func(t *testing.T) {
			s, _ := notRunningServer(t)
			ac := s.deps.Config.Agents[notRunningTestAgent]
			ac.Model = tc.model
			s.deps.Config.Agents[notRunningTestAgent] = ac

			rec := doPost(s, "/api/switch/"+notRunningTestAgent+"/"+tc.backend, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
			}
			if got := s.deps.Config.Agents[notRunningTestAgent].Model; got != tc.model {
				t.Errorf("model = %q, want unchanged %q", got, tc.model)
			}
			if _, ok := findSystemAlert(s, modelIDNormalizedAlertPrefix+notRunningTestAgent); ok {
				t.Errorf("no warning expected")
			}
		})
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

func bootNormalizedServer(t *testing.T) (*Server, string) {
	t.Helper()
	s, dir := notRunningServer(t)
	s.deps.Config.ModelIDNormalizations = []config.ModelIDNormalization{
		{Agent: notRunningTestAgent, Backend: config.ClaudeBackend, From: dottedClaudeModel, To: dashedClaudeModel},
	}
	ac := s.deps.Config.Agents[notRunningTestAgent]
	ac.Model = dashedClaudeModel
	s.deps.Config.Agents[notRunningTestAgent] = ac
	return s, dir
}

// Rewrites done by the config loader are written to the agent overlay so they
// do not repeat on every boot. Once persisted there is nothing left for the
// operator to do, so no banner is raised: it used to come back on every boot
// or upgrade of a hub-provisioned hive whose seed config carried the other
// spelling, with no way to dismiss it (#19747).
func TestSurfaceModelIDNormalizations_PersistedRaisesNoBanner(t *testing.T) {
	s, dir := bootNormalizedServer(t)

	s.surfaceModelIDNormalizations()

	if a, ok := findSystemAlert(s, modelIDNormalizedAlertPrefix+notRunningTestAgent); ok {
		t.Fatalf("persisted rewrite must not raise a banner, got %+v", a)
	}
	if !strings.Contains(overlayText(t, dir), dashedClaudeModel) {
		t.Fatalf("overlay must carry the normalized id")
	}
}

// A rewrite that cannot be persisted repeats on every boot, so it is still
// surfaced to the operator.
func TestSurfaceModelIDNormalizations_UnpersistedRaisesBanner(t *testing.T) {
	s, dir := bootNormalizedServer(t)
	notADir := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.deps.Config.Data.AgentsDir = notADir

	s.surfaceModelIDNormalizations()

	a, ok := findSystemAlert(s, modelIDNormalizedAlertPrefix+notRunningTestAgent)
	if !ok || a.Severity != "warning" || !strings.Contains(a.Message, dottedClaudeModel) {
		t.Fatalf("alert = %+v (present %v)", a, ok)
	}
}

// The model-id rewrite banner can be dismissed through the system-alert
// dismiss endpoint, which used to accept only plan-stall ids (#19747).
func TestSystemAlertDismiss_AcceptsModelIDNormalizedAlert(t *testing.T) {
	s, _ := apiServer(t)
	id := modelIDNormalizedAlertPrefix + "supervisor"
	s.AddSystemAlert(id, "warning", modelIDConventionWarning("supervisor", "copilot", "claude-sonnet-4-6", "claude-sonnet-4.6"))

	rec := doPost(s, "/api/system-alerts/dismiss", map[string]string{"id": id})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if _, ok := findSystemAlert(s, id); ok {
		t.Fatalf("alert must be cleared after dismiss")
	}

	// Other alert kinds stay non-dismissible.
	s.AddSystemAlert("launch-cmd-degraded-x", "error", "x")
	if rec := doPost(s, "/api/system-alerts/dismiss", map[string]string{"id": "launch-cmd-degraded-x"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a non-dismissible alert", rec.Code)
	}
}

// Each backend's picker list only offers its own spelling of a Claude model;
// non-Claude ids are untouched.
func TestCanonicalModelList(t *testing.T) {
	got := canonicalModelList(config.ClaudeBackend, []string{dottedClaudeModel, dashedClaudeModel, "sonnet", "claude-opus-4.6"})
	if want := []string{dashedClaudeModel, "sonnet", "claude-opus-4-6"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("claude list = %v, want %v", got, want)
	}
	got = canonicalModelList("copilot", []string{dashedClaudeModel, dottedClaudeModel, "claude-sonnet-5", "gpt-5-mini", "auto"})
	if want := []string{dottedClaudeModel, "claude-sonnet-5", "gpt-5-mini", "auto"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("copilot list = %v, want %v", got, want)
	}
}

// The Settings dialog's model picker (modelSelectOptionsHtml) shows a stored
// wrong-convention id as stored and flags it, in both directions, instead of
// preselecting the other spelling.
func TestSettingsModelPickerFlagsWrongConvention(t *testing.T) {
	html := indexHTML(t)
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	script := extractJSFunction(t, html, "esc") + "\n    }\n" +
		extractJSOneLiner(t, html, "_normalizeModel") + "\n" +
		extractJSOneLiner(t, html, "_modelsEqual") + "\n" +
		modelSelectHelpersJS(t, html) +
		`const claude = [{ value: 'claude-sonnet-5-5', label: 'claude-sonnet-5-5' }];
		const copilot = [{ value: 'claude-sonnet-5.5', label: 'claude-sonnet-5.5' }, { value: 'gpt-5-mini', label: 'gpt-5-mini' }];
		console.log(JSON.stringify({
			claudeDotted: modelSelectOptionsHtml('claude', 'claude-sonnet-5.5', claude),
			claudeValid: modelSelectOptionsHtml('claude', 'claude-sonnet-5-5', claude),
			copilotDashed: modelSelectOptionsHtml('copilot', 'claude-sonnet-5-5', copilot),
			copilotGpt: modelSelectOptionsHtml('copilot', 'gpt-5-mini', copilot),
		}));`
	raw, err := exec.Command("node", "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, raw)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &got); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	for key, want := range map[string]string{
		"claudeDotted":  `<option value="claude-sonnet-5.5" selected>claude-sonnet-5.5 (current, ⚠ not valid for claude; pick claude-sonnet-5-5)</option>`,
		"claudeValid":   `<option value="claude-sonnet-5-5" selected>`,
		"copilotDashed": `<option value="claude-sonnet-5-5" selected>claude-sonnet-5-5 (current, ⚠ not valid for copilot; pick claude-sonnet-5.5)</option>`,
		"copilotGpt":    `<option value="gpt-5-mini" selected>`,
	} {
		if !strings.Contains(got[key], want) {
			t.Errorf("%s: missing %q in\n%s", key, want, got[key])
		}
	}
	if strings.Contains(got["claudeDotted"], `<option value="claude-sonnet-5-5" selected>`) {
		t.Errorf("claude: the dashed option must not be preselected for a stored dotted id")
	}
	if strings.Contains(got["copilotDashed"], `<option value="claude-sonnet-5.5" selected>`) {
		t.Errorf("copilot: the dotted option must not be preselected for a stored dashed id")
	}
}
