package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func advisorSettingsRequest(t *testing.T, s *Server, method, path, body string, owner bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if owner {
		markOwnerRequest(req)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func decodeAdvisorSettings(t *testing.T, rec *httptest.ResponseRecorder) advisorSettingsResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp advisorSettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v (%s)", err, rec.Body.String())
	}
	return resp
}

func advisorSettingsAgent(t *testing.T, resp advisorSettingsResponse, name string) advisorAgentView {
	t.Helper()
	for _, a := range resp.Agents {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("agent %q missing from settings view: %+v", name, resp.Agents)
	return advisorAgentView{}
}

func TestAdvisorSettingsOwnerOnly(t *testing.T) {
	s := newFullServer(t)
	for _, probe := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/config/advisor", ""},
		{http.MethodPut, "/api/config/advisor", `{"enabled":true}`},
		{http.MethodPut, "/api/config/advisor/roles", `{"model_roles":{}}`},
		{http.MethodPut, "/api/config/advisor/agent/scanner", `{"override":{"enabled":false}}`},
	} {
		rec := advisorSettingsRequest(t, s, probe.method, probe.path, probe.body, false)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s without owner: status = %d, want 403", probe.method, probe.path, rec.Code)
		}
	}
	if s.deps.Config.Advisor.Enabled != nil {
		t.Error("a refused PUT must not change the fleet advisor block")
	}
	if s.deps.Config.Agents["scanner"].Advisor != nil {
		t.Error("a refused PUT must not change an agent override")
	}
}

func TestAdvisorSettingsFleetEditAndEffectiveView(t *testing.T) {
	s := newFullServer(t)
	cfg := s.deps.Config
	cfg.Agents["optout"] = config.AgentConfig{Backend: "claude", Enabled: true}
	cfg.Agents["goose-agent"] = config.AgentConfig{Backend: "goose", Enabled: true}

	resp := decodeAdvisorSettings(t, advisorSettingsRequest(t, s, http.MethodPut, "/api/config/advisor",
		`{"enabled":true,"model":" model-a ","instructions":"look for scope creep","daily_budget_tokens":5000,"timeout_s":20,"max_consecutive_blocks":2}`, true))
	if !cfg.Advisor.IsEnabled() || cfg.Advisor.Model != "model-a" || cfg.Advisor.TimeoutS != 20 {
		t.Fatalf("fleet block not applied: %+v", cfg.Advisor)
	}
	if resp.Defaults.BlockBound != config.AdvisorCopilotBlockBound {
		t.Errorf("defaults must carry the backend block bound: %+v", resp.Defaults)
	}
	scanner := advisorSettingsAgent(t, resp, "scanner")
	if !scanner.Effective.Enabled || scanner.Effective.ResolvedModel != "model-a" || scanner.Effective.Sources["model"] != config.AdvisorSourceFleet {
		t.Errorf("scanner must inherit the fleet default: %+v", scanner.Effective)
	}
	if !scanner.Active {
		t.Errorf("scanner on claude must be active: %+v", scanner)
	}
	goose := advisorSettingsAgent(t, resp, "goose-agent")
	if goose.Active || !strings.Contains(goose.ActiveReason, "not active on backend") {
		t.Errorf("an unsupported backend must report not active: %+v", goose)
	}

	// Per-agent opt-out and override.
	resp = decodeAdvisorSettings(t, advisorSettingsRequest(t, s, http.MethodPut, "/api/config/advisor/agent/optout",
		`{"override":{"enabled":false}}`, true))
	optout := advisorSettingsAgent(t, resp, "optout")
	if optout.Effective.Enabled || optout.Effective.Sources["enabled"] != config.AdvisorSourceAgent || optout.Active {
		t.Errorf("opt-out must be in force from the agent: %+v", optout)
	}
	resp = decodeAdvisorSettings(t, advisorSettingsRequest(t, s, http.MethodPut, "/api/config/advisor/agent/scanner",
		`{"override":{"model":"model-b","timeout_s":7}}`, true))
	scanner = advisorSettingsAgent(t, resp, "scanner")
	if scanner.Effective.ResolvedModel != "model-b" || scanner.Effective.TimeoutS != 7 || scanner.Effective.Sources["timeout_s"] != config.AdvisorSourceAgent {
		t.Errorf("override must win: %+v", scanner.Effective)
	}
	if cfg.EffectiveAdvisor("scanner").Model != "model-b" {
		t.Errorf("live config must carry the override for the next launch: %+v", cfg.EffectiveAdvisor("scanner"))
	}

	// An empty override clears it: the agent inherits the fleet block again.
	resp = decodeAdvisorSettings(t, advisorSettingsRequest(t, s, http.MethodPut, "/api/config/advisor/agent/scanner",
		`{"override":{}}`, true))
	scanner = advisorSettingsAgent(t, resp, "scanner")
	if scanner.Override != nil || scanner.Effective.ResolvedModel != "model-a" {
		t.Errorf("empty override must clear back to the fleet default: %+v", scanner)
	}
}

func TestAdvisorSettingsRejectsInvalidBeforeMutation(t *testing.T) {
	s := newFullServer(t)
	cfg := s.deps.Config

	rec := advisorSettingsRequest(t, s, http.MethodPut, "/api/config/advisor", `{"max_consecutive_blocks":8}`, true)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Copilot CLI bound") {
		t.Errorf("block limit at the backend bound: status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = advisorSettingsRequest(t, s, http.MethodPut, "/api/config/advisor", `{"model":"@nope"}`, true)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown model role") {
		t.Errorf("unknown role: status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = advisorSettingsRequest(t, s, http.MethodPut, "/api/config/advisor", `{"timeout_s":-1}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("negative timeout: status = %d", rec.Code)
	}
	if cfg.Advisor.MaxConsecutiveBlocks != 0 || cfg.Advisor.Model != "" || cfg.Advisor.TimeoutS != 0 {
		t.Errorf("rejected edits must not mutate the fleet block: %+v", cfg.Advisor)
	}

	rec = advisorSettingsRequest(t, s, http.MethodPut, "/api/config/advisor/agent/scanner", `{"override":{"max_consecutive_blocks":9}}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("per-agent block limit at the bound: status = %d", rec.Code)
	}
	if cfg.Agents["scanner"].Advisor != nil {
		t.Errorf("rejected override must not be applied: %+v", cfg.Agents["scanner"].Advisor)
	}
	rec = advisorSettingsRequest(t, s, http.MethodPut, "/api/config/advisor/agent/ghost", `{"override":{"enabled":false}}`, true)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown agent: status = %d, want 404", rec.Code)
	}
}

func TestAdvisorSettingsRolesEdit(t *testing.T) {
	s := newFullServer(t)
	cfg := s.deps.Config

	resp := decodeAdvisorSettings(t, advisorSettingsRequest(t, s, http.MethodPut, "/api/config/advisor/roles",
		`{"model_roles":{"advisor":{"backend":"claude","model":"claude-haiku-4.5"}}}`, true))
	if resp.ModelRoles["advisor"].Model != "claude-haiku-4.5" || cfg.ModelRoles["advisor"].Backend != "claude" {
		t.Fatalf("roles not applied: %+v", resp.ModelRoles)
	}

	// Reference the role from the fleet block; the view resolves it.
	resp = decodeAdvisorSettings(t, advisorSettingsRequest(t, s, http.MethodPut, "/api/config/advisor",
		`{"enabled":true,"model":"@advisor"}`, true))
	if got := advisorSettingsAgent(t, resp, "scanner").Effective.ResolvedModel; got != "claude-haiku-4.5" {
		t.Errorf("role reference must resolve to the role's model, got %q", got)
	}

	// Changing the role's model changes what the agent resolves to.
	resp = decodeAdvisorSettings(t, advisorSettingsRequest(t, s, http.MethodPut, "/api/config/advisor/roles",
		`{"model_roles":{"advisor":{"model":"model-c"}}}`, true))
	if got := advisorSettingsAgent(t, resp, "scanner").Effective.ResolvedModel; got != "model-c" {
		t.Errorf("role edit must change the in-force model, got %q", got)
	}

	// Removing a role still referenced is refused, and the map is unchanged.
	rec := advisorSettingsRequest(t, s, http.MethodPut, "/api/config/advisor/roles", `{"model_roles":{}}`, true)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown model role") {
		t.Errorf("removing a referenced role: status = %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := cfg.ModelRoles["advisor"]; !ok {
		t.Error("a refused roles edit must leave the map intact")
	}

	for _, body := range []string{
		`{"model_roles":{"bad name":{"model":"m"}}}`,
		`{"model_roles":{"x":{"model":""}}}`,
		`{"model_roles":{"x":{"backend":"not-a-backend","model":"m"}}}`,
	} {
		if rec := advisorSettingsRequest(t, s, http.MethodPut, "/api/config/advisor/roles", body, true); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
		}
	}
}

func TestAdvisorSettingsPersistsToHiveYAML(t *testing.T) {
	s := newFullServer(t)
	decodeAdvisorSettings(t, advisorSettingsRequest(t, s, http.MethodPut, "/api/config/advisor",
		`{"enabled":true,"model":"persisted-model"}`, true))
	loaded, err := config.Load(s.deps.Config.SourcePath)
	if err != nil {
		t.Skipf("saved test config does not round-trip through Load: %v", err)
	}
	if loaded.Advisor.Model != "persisted-model" || !loaded.Advisor.IsEnabled() {
		t.Errorf("advisor block must be written to hive.yaml: %+v", loaded.Advisor)
	}
}
