package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func newVarServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{
		Project:    config.ProjectConfig{Org: "o", Repos: []string{"r"}},
		GitHub:     config.GitHubConfig{Token: "ghp_x"},
		Agents:     map[string]config.AgentConfig{"scanner": {Backend: "copilot"}},
		SourcePath: t.TempDir() + "/hive.yaml",
	}
	// A real (discarding) logger is required: RegisterAPI constructs a
	// ContributeWSHub with the server's logger, and on a live hive host the
	// hub's load* methods read existing /data/contributors state and log
	// through it — a nil logger panics and fails the whole suite.
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := NewServer(0, logger)
	srv.deps = &Dependencies{Config: cfg, RefreshFunc: func() {}, PersistFunc: func() {}, SkipReloadFunc: func() {}}
	srv.RegisterAPI(srv.deps)
	return srv
}

func putVar(t *testing.T, srv *Server, name, body string) int {
	t.Helper()
	req := httptest.NewRequest("PUT", "/api/config/variables/"+name, strings.NewReader(body))
	req.SetPathValue("name", name)
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleVariableUpsert(w, req)
	return w.Code
}

func TestVariableUpsert_StaticAndEnvAllowed(t *testing.T) {
	srv := newVarServer(t)
	if code := putVar(t, srv, "DEPLOY_ENV", `{"type":"static","value":"production","scope":"both"}`); code != 200 {
		t.Fatalf("static upsert: got %d", code)
	}
	if got := srv.deps.Config.Variables.Defs["DEPLOY_ENV"].Value; got != "production" {
		t.Errorf("static not saved: %q", got)
	}
	if code := putVar(t, srv, "REGION", `{"type":"env","env":"HIVE_REGION","scope":"config"}`); code != 200 {
		t.Fatalf("env upsert: got %d", code)
	}
}

func TestVariableUpsert_RejectsScriptAndHTTP(t *testing.T) {
	srv := newVarServer(t)
	if code := putVar(t, srv, "X", `{"type":"script","command":["echo","hi"]}`); code != 403 {
		t.Errorf("script from dashboard should be 403, got %d", code)
	}
	if code := putVar(t, srv, "Y", `{"type":"http","url":"http://x/y"}`); code != 403 {
		t.Errorf("http from dashboard should be 403, got %d", code)
	}
	if _, ok := srv.deps.Config.Variables.Defs["X"]; ok {
		t.Error("script var must not have been saved")
	}
}

func TestVariableUpsert_RejectsSecretInStatic(t *testing.T) {
	srv := newVarServer(t)
	if code := putVar(t, srv, "TOK", `{"type":"static","value":"sk-abcdef1234567890abcdef"}`); code != 400 {
		t.Errorf("secret-looking static value should be 400, got %d", code)
	}
}

func TestVariableUpsert_RejectsBadName(t *testing.T) {
	srv := newVarServer(t)
	if code := putVar(t, srv, "1bad-name", `{"type":"static","value":"x"}`); code != 400 {
		t.Errorf("bad name should be 400, got %d", code)
	}
}

func TestVariableDelete_GuardsSeedTypes(t *testing.T) {
	srv := newVarServer(t)
	// Seed a script var directly (as if from the seed config).
	srv.deps.Config.Variables.Defs = map[string]config.VarDef{
		"SEED_SCRIPT": {Type: "script", Command: []string{"echo", "x"}},
		"UI_STATIC":   {Type: "static", Value: "v"},
	}
	// Deleting a script var via the dashboard is forbidden.
	req := httptest.NewRequest("DELETE", "/api/config/variables/SEED_SCRIPT", nil)
	req.SetPathValue("name", "SEED_SCRIPT")
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleVariableDelete(w, req)
	if w.Code != 403 {
		t.Errorf("deleting script var should be 403, got %d", w.Code)
	}
	// Deleting a static var is allowed.
	req2 := httptest.NewRequest("DELETE", "/api/config/variables/UI_STATIC", nil)
	req2.SetPathValue("name", "UI_STATIC")
	markOwnerRequest(req2)
	w2 := httptest.NewRecorder()
	markOwnerRequest(req2)
	srv.handleVariableDelete(w2, req2)
	if w2.Code != 200 {
		t.Errorf("deleting static var should be 200, got %d", w2.Code)
	}
	if _, ok := srv.deps.Config.Variables.Defs["UI_STATIC"]; ok {
		t.Error("static var should have been deleted")
	}
}

func TestLooksLikeApiKeyValue(t *testing.T) {
	secrets := []string{"sk-abcdef123456", "ghp_xxxxxxxxxxxx", "Bearer abc", "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5"}
	safe := []string{"production", "us-east-1", "v2", "my-value", "some/path", ""}
	for _, s := range secrets {
		if !looksLikeApiKeyValue(s) {
			t.Errorf("should flag %q as secret", s)
		}
	}
	for _, s := range safe {
		if looksLikeApiKeyValue(s) {
			t.Errorf("should NOT flag %q", s)
		}
	}
}

func agentVarRequest(t *testing.T, srv *Server, method, agent, name, body string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/config/agent/" + agent + "/variables"
	if name != "" {
		path += "/" + name
	}
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.SetPathValue("name", agent)
	req.SetPathValue("var", name)
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	switch method {
	case "GET":
		srv.handleAgentVariablesList(w, req)
	case "PUT":
		srv.handleAgentVariableUpsert(w, req)
	case "DELETE":
		srv.handleAgentVariableDelete(w, req)
	}
	return w
}

func TestAgentVariableUpsert(t *testing.T) {
	tests := []struct {
		name     string
		agent    string
		varName  string
		body     string
		wantCode int
	}{
		{"static allowed", "scanner", "LANE_GOAL", `{"type":"static","value":"find bugs","scope":"template"}`, 200},
		{"env allowed", "scanner", "REPO_PATH", `{"type":"env","env":"HIVE_REPO_PATH","scope":"both"}`, 200},
		{"script forbidden", "scanner", "X", `{"type":"script"}`, 403},
		{"http forbidden", "scanner", "Y", `{"type":"http"}`, 403},
		{"config scope rejected", "scanner", "Z", `{"type":"static","value":"v","scope":"config"}`, 400},
		{"secret static rejected", "scanner", "TOK", `{"type":"static","value":"sk-abcdef1234567890abcdef"}`, 400},
		{"bad name", "scanner", "1bad-name", `{"type":"static","value":"x"}`, 400},
		{"unknown agent", "ghost", "A", `{"type":"static","value":"x"}`, 404},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newVarServer(t)
			w := agentVarRequest(t, srv, "PUT", tc.agent, tc.varName, tc.body)
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.wantCode, w.Body.String())
			}
			_, saved := srv.deps.Config.Agents["scanner"].Variables[tc.varName]
			if saved != (tc.wantCode == 200) {
				t.Errorf("saved = %v, want %v", saved, tc.wantCode == 200)
			}
			if _, leaked := srv.deps.Config.Variables.Defs[tc.varName]; leaked {
				t.Error("per-agent upsert must not write hive-level defs")
			}
		})
	}
}

func TestAgentVariablesList_InheritsAndNeverReturnsValues(t *testing.T) {
	srv := newVarServer(t)
	srv.deps.Config.Variables.Defs = map[string]config.VarDef{
		"DEPLOY_ENV": {Type: "static", Value: "production-secretish"},
		"REGION":     {Type: "env", Env: "HIVE_REGION"},
	}
	ac := srv.deps.Config.Agents["scanner"]
	ac.Variables = map[string]config.VarDef{"DEPLOY_ENV": {Type: "static", Value: "staging-secretish"}}
	srv.deps.Config.Agents["scanner"] = ac

	w := agentVarRequest(t, srv, "GET", "scanner", "", "")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "secretish") {
		t.Fatalf("list must never return values: %s", body)
	}
	var resp struct {
		Variables []varView `json:"variables"`
		Inherited []struct {
			Name       string `json:"name"`
			Source     string `json:"source"`
			Overridden bool   `json:"overridden"`
		} `json:"inherited"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Variables) != 1 || resp.Variables[0].Name != "DEPLOY_ENV" {
		t.Errorf("agent variables = %+v", resp.Variables)
	}
	want := map[string]bool{"DEPLOY_ENV": true, "REGION": false}
	if len(resp.Inherited) != len(want) {
		t.Fatalf("inherited = %+v", resp.Inherited)
	}
	for _, iv := range resp.Inherited {
		if iv.Overridden != want[iv.Name] {
			t.Errorf("%s overridden = %v, want %v", iv.Name, iv.Overridden, want[iv.Name])
		}
	}

	if w := agentVarRequest(t, srv, "GET", "ghost", "", ""); w.Code != 404 {
		t.Errorf("unknown agent list = %d, want 404", w.Code)
	}
}

func TestAgentVariableDelete(t *testing.T) {
	srv := newVarServer(t)
	srv.deps.Config.Variables.Defs = map[string]config.VarDef{"HIVE_ONLY": {Type: "static", Value: "v"}}
	ac := srv.deps.Config.Agents["scanner"]
	ac.Variables = map[string]config.VarDef{"MINE": {Type: "static", Value: "v"}}
	srv.deps.Config.Agents["scanner"] = ac

	tests := []struct {
		name     string
		agent    string
		varName  string
		wantCode int
	}{
		{"inherited var is not deletable here", "scanner", "HIVE_ONLY", 404},
		{"unknown agent", "ghost", "MINE", 404},
		{"agent var deleted", "scanner", "MINE", 200},
		{"already gone", "scanner", "MINE", 404},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if w := agentVarRequest(t, srv, "DELETE", tc.agent, tc.varName, ""); w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", w.Code, tc.wantCode)
			}
		})
	}
	if _, ok := srv.deps.Config.Variables.Defs["HIVE_ONLY"]; !ok {
		t.Error("hive-level variable must survive a per-agent delete")
	}
	if len(srv.deps.Config.Agents["scanner"].Variables) != 0 {
		t.Error("agent variable should have been deleted")
	}
}
