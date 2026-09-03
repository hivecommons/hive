package dashboard

import (
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
