package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func meetingsModelGet(t *testing.T, s *Server, role string, verified bool) (int, MeetingsModelView) {
	t.Helper()
	rec := actionsRequest(s, http.MethodGet, "/api/meetings/model", ``, role, verified)
	var v MeetingsModelView
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code, v
}

// GET /api/meetings/model lists the supported backends, the agents'
// backends and the defaults; PUT (owner only) saves the agent-backend
// choice and refuses a backend with no completion profile.
func TestMeetingsModelAPI(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Agents = map[string]config.AgentConfig{
		"a": {Backend: "claude", Model: "claude-opus-5"}, "b": {Backend: "claude"}, "c": {Backend: "copilot"},
	}
	for _, role := range []string{"", "read"} {
		if code, _ := meetingsModelGet(t, s, role, false); code != http.StatusForbidden {
			t.Errorf("GET as %q = %d", role, code)
		}
	}
	code, v := meetingsModelGet(t, s, "read-write", false)
	if code != http.StatusOK || v.Mode != config.MeetingsLLMModeAuto || v.DefaultBackend != "claude" ||
		v.DefaultModel != config.DefaultMeetingsClaudeCLIModel || strings.Join(v.SupportedBackends, ",") != "claude,copilot,litellm,llm-d,omp,vllm" ||
		strings.Join(v.AgentBackends, ",") != "claude,copilot" || v.DefaultModels["claude"] != config.DefaultMeetingsClaudeCLIModel ||
		len(v.Modes) != len(config.MeetingsLLMModes) {
		t.Fatalf("GET = %d %+v", code, v)
	}
	if rec := actionsRequest(s, http.MethodPut, "/api/meetings/model", `{"mode":"agent-backend"}`, "read-write", false); rec.Code != http.StatusForbidden {
		t.Fatalf("read-write PUT = %d", rec.Code)
	}
	if !strings.Contains(v.RefusedBackends["codex"], "shell tool") || v.RefusedBackends["gemini"] == "" {
		t.Fatalf("refused backends %v", v.RefusedBackends)
	}
	rec := actionsRequest(s, http.MethodPut, "/api/meetings/model", `{"mode":"agent-backend","backend":"codex"}`, "owner", true)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "cannot answer for meetings") {
		t.Fatalf("codex PUT = %d %s", rec.Code, rec.Body.String())
	}
	rec = actionsRequest(s, http.MethodPut, "/api/meetings/model", `{"mode":"agent-backend","backend":"corp-gw"}`, "owner", true)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "configured model gateway") {
		t.Fatalf("unknown gateway PUT = %d %s", rec.Code, rec.Body.String())
	}
	if rec := actionsRequest(s, http.MethodPut, "/api/meetings/model", `not json`, "owner", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d", rec.Code)
	}
	rec = actionsRequest(s, http.MethodPut, "/api/meetings/model", `{"mode":"Agent-Backend","backend":"Claude","model":" claude-sonnet-5-5 "}`, "owner", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}
	var saved MeetingsModelView
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.RestartRequired || saved.Mode != "agent-backend" || saved.Backend != "claude" || saved.Model != "claude-sonnet-5-5" ||
		saved.Label != "claude CLI claude-sonnet-5-5" {
		t.Fatalf("saved %+v", saved)
	}
	l := deps.Config.Meetings.LLM
	if l.Mode != "agent-backend" || l.Backend != "claude" || l.Model != "claude-sonnet-5-5" {
		t.Fatalf("config %+v", l)
	}
	// The same choice again changes nothing.
	rec = actionsRequest(s, http.MethodPut, "/api/meetings/model", `{"mode":"agent-backend"}`, "owner", true)
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil || saved.RestartRequired {
		t.Fatalf("unchanged save %+v %v", saved, err)
	}
	t.Setenv(config.MeetingsLLMBackendEnv, "claude")
	if _, v := meetingsModelGet(t, s, "owner", true); len(v.EnvOverrides) != 1 || v.EnvOverrides[0] != config.MeetingsLLMBackendEnv {
		t.Fatalf("env overrides %v", v.EnvOverrides)
	}
	// The meetings settings show the backend too.
	if got := s.meetingsSettings(t.Context(), deps.Config.Meetings).Effective["meetings.llm.backend"]; got != "claude" {
		t.Fatalf("effective backend %q", got)
	}
}

// A hive's named model gateways can answer for meetings; a watsonx one is
// listed with its reason.
func TestMeetingsModelAPIGateways(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Governor.Gateways = []config.GatewayConfig{
		{Name: "corp-gw", Kind: "litellm", Endpoint: "https://gw.example/v1", DefaultModel: "gw-model"},
		{Name: "ibm", Kind: config.GatewayKindWatsonx, Endpoint: "https://us-south.ml.cloud.ibm.com/ml/gateway"},
	}
	_, v := meetingsModelGet(t, s, "owner", true)
	if !strings.Contains(strings.Join(v.SupportedBackends, ","), "corp-gw") || !strings.Contains(v.RefusedBackends["ibm"], "IAM") {
		t.Fatalf("gateways %v %v", v.SupportedBackends, v.RefusedBackends)
	}
	rec := actionsRequest(s, http.MethodPut, "/api/meetings/model", `{"mode":"agent-backend","backend":"corp-gw"}`, "owner", true)
	if rec.Code != http.StatusOK || deps.Config.Meetings.LLM.Backend != "corp-gw" {
		t.Fatalf("gateway PUT = %d %s", rec.Code, rec.Body.String())
	}
	if rec := actionsRequest(s, http.MethodPut, "/api/meetings/model", `{"mode":"agent-backend","backend":"ibm"}`, "owner", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("watsonx gateway PUT = %d", rec.Code)
	}
}

func TestMeetingsModelAPINoConfig(t *testing.T) {
	s := NewServer(0, slog.New(slog.DiscardHandler))
	s.registerMeetingsModelRoutes()
	if code, _ := meetingsModelGet(t, s, "owner", true); code != http.StatusServiceUnavailable {
		t.Fatalf("no config = %d", code)
	}
	if rec := actionsRequest(s, http.MethodPut, "/api/meetings/model", `{}`, "owner", true); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no config PUT = %d", rec.Code)
	}
	if rec := actionsRequest(s, http.MethodPut, "/api/meetings/model", `{}`, "read-write", false); rec.Code != http.StatusForbidden {
		t.Fatalf("read-write PUT = %d", rec.Code)
	}
}

// The Meeting attendee tab renders the Model section from the agents'
// backend and model catalog and saves it on its own.
func TestMeetingAttendeeModelSectionWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"(typeof maModelHtml === 'function' ? maModelHtml() : '')",
		"fetch('/api/meetings/model')",
		"fetch('/api/meetings/model', { method: 'PUT'",
		"typeof backendOptionList === 'function' ? backendOptionList() : []",
		"typeof modelsForBackend === 'function' ? modelsForBackend(backend) : []",
		"if (typeof fetchBackendsConfig === 'function') await fetchBackendsConfig(false);",
		`aria-labelledby="ma-model-h"`,
		`data-action="maSaveModel"`,
		`data-change-action="maModelInput"`,
		"yamlHint('meetings.llm.backend')",
		"'agent-backend': 'An agent backend (the CLI and sign-in the agents use)'",
		" (not for meetings)",
		"Why some backends are unavailable for meetings",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("Meeting attendee model wiring missing %q", want)
		}
	}
}

// TestMeetingAttendeeModelSectionBehaviour runs the Model section's rules:
// backends from the agents' catalog with the unsupported ones disabled,
// models from the same catalog with the meetings default first, and a
// backend change resetting the model.
func TestMeetingAttendeeModelSectionBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the Model section rules were NOT executed by this run")
	}
	html := indexHTML(t)
	parts := []string{
		"const assert = require('assert');",
		"const escapeHtml = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/\"/g, '&quot;');",
		"const yamlHint = p => '<code>' + p + '</code>';",
		"const host = { innerHTML: '' }; const document = { querySelector: () => host };",
		"const backendOptionList = () => ['claude', 'copilot', 'litellm'];",
		"const backendLabel = b => b;",
		"const modelsForBackend = b => b === 'claude' ? [{ value: 'claude-sonnet-5-5', label: 'Sonnet 5.5' }, { value: 'claude-opus-5', label: 'Opus 5' }] : [{ value: 'gpt-x', label: 'gpt-x' }];",
		"let _maModel = null;",
		jsConstBlock(t, html, "MA_MODEL_MODE_LABELS"),
	}
	for _, f := range []string{"maModelBackends", "maModelSupported", "maModelRefusal", "maModelRefusalsHtml", "maModelEffectiveBackend", "maModelOptions", "maModelBodyHtml",
		"maModelRender", "maModelSetData", "maModelInput"} {
		parts = append(parts, jsFunc(t, html, f))
	}
	parts = append(parts,
		`const data = { mode: 'agent-backend', backend: '', model: '', label: 'claude CLI claude-sonnet-5-5', modes: ['auto', 'openai', 'anthropic', 'agent-backend', 'none'], supported_backends: ['claude', 'litellm'], refused_backends: { gemini: 'no tool-off flag' }, agent_backends: ['claude', 'gemini'], default_backend: 'claude', default_models: { claude: 'claude-sonnet-5-5' }, env_overrides: [] };`,
		`maModelSetData(data, false);`,
		`let h = maModelBodyHtml();`,
		`assert.ok(h.includes('value="copilot" disabled title="it has no meetings completion profile and is not a configured model gateway">copilot (not for meetings)'), 'unsupported backend disabled with the reason');`,
		`assert.ok(h.includes('value="gemini" disabled title="no tool-off flag"') && h.includes('<strong>gemini</strong>: no tool-off flag'), 'refusal reason shown');`,
		`assert.ok(h.includes('value="gemini" disabled'), 'agent backends outside the catalog listed');`,
		`assert.ok(h.includes('value="litellm">litellm<'), 'litellm selectable');`,
		`assert.ok(h.includes('Default (claude)') && h.includes('Default (claude-sonnet-5-5)') && h.includes('Opus 5'), h);`,
		`assert.ok(h.includes('claude CLI claude-sonnet-5-5'), 'label shown');`,
		`maModelInput('model', 'claude-opus-5'); assert.ok(host.innerHTML.includes('value="claude-opus-5" selected'));`,
		`maModelInput('backend', 'litellm'); assert.strictEqual(_maModel.form.model, ''); assert.ok(host.innerHTML.includes('gpt-x'));`,
		`maModelInput('model', 'custom-1'); assert.ok(host.innerHTML.includes('custom-1 (current)'));`,
		`maModelSetData(Object.assign({}, data, { mode: 'claude-cli', env_overrides: ['HIVE_MEETINGS_LLM_MODE'] }), true); h = maModelBodyHtml();`,
		`assert.ok(/data-ma-model="backend"[^>]* disabled/.test(h), 'claude-cli fixes the backend');`,
		`assert.strictEqual(maModelEffectiveBackend(), 'claude');`,
		`assert.ok(h.includes('Restart the hive') && h.includes('HIVE_MEETINGS_LLM_MODE'), h);`,
		`maModelSetData(Object.assign({}, data, { mode: 'none' }), false); assert.ok(!maModelBodyHtml().includes('data-ma-model="backend"'), 'no picker outside agent-backend');`,
		`_maModel = { error: 'boom' }; assert.ok(maModelBodyHtml().includes('boom'));`,
		`_maModel = { loading: true }; assert.ok(maModelBodyHtml().includes('Loading'));`,
		`console.log('ok');`,
	)
	out, err := runNodeStdin(node, strings.Join(parts, "\n"))
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("Model section rules failed: %v\n%s", err, out)
	}
}
