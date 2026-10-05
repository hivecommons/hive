package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"gopkg.in/yaml.v3"
)

func publicSettingsRequest(s *Server, method, body string, owner bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api/knowledge/public", strings.NewReader(body))
	if owner {
		r.Header.Set("Authorization", "Bearer dashboard-secret")
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestPublicKnowledgeSettingsPersistenceAndImmediateDisable(t *testing.T) {
	t.Setenv(publicKnowledgeEnabledEnv, "1")
	t.Setenv(publicKnowledgeTagsEnv, " env-tag ")
	s := publicKnowledgeServer(t)
	s.deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	s.deps.Config.Dashboard.PublicURL = "https://knowledge.example"
	w := publicSettingsRequest(s, "GET", "", true)
	var initial publicKnowledgeSettings
	if err := json.Unmarshal(w.Body.Bytes(), &initial); err != nil {
		t.Fatal(err)
	}
	if !initial.Enabled || initial.Source != "env" || initial.Tags[0] != "env-tag" || initial.URL != "https://knowledge.example/mcp/knowledge" {
		t.Fatalf("unexpected initial settings: %+v", initial)
	}

	w = publicSettingsRequest(s, "PUT", `{"enabled":false,"tags":[" Linux ","linux",""]}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	if got := mcpCall(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); got.Code != http.StatusNotFound {
		t.Fatalf("disabled endpoint: %d", got.Code)
	}
	data, err := os.ReadFile(s.deps.Config.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	var restored config.Config
	if err := yaml.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	p := restored.Knowledge.Public
	if p == nil || p.Enabled || len(p.Tags) != 1 || p.Tags[0] != "linux" {
		t.Fatalf("persisted settings: %+v", p)
	}
	s.deps.Config.Knowledge.Public = p
	if got := s.publicKnowledgeSettings(); got.Enabled || got.Source != "config" {
		t.Fatalf("restored settings: %+v", got)
	}
	entries := s.audit.Recent(1)
	if len(entries) != 1 || entries[0].Action != "knowledge_public_update" {
		t.Fatalf("missing audit: %+v", entries)
	}

	t.Setenv(publicKnowledgeEnabledEnv, "0")
	w = publicSettingsRequest(s, "PUT", `{"enabled":true,"tags":["linux"]}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", w.Code, w.Body.String())
	}
	result := mcpCall(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"knowledge_export","arguments":{}}}`)
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), "Caching Pattern") || strings.Contains(result.Body.String(), "Nil Map Gotcha") || strings.Contains(result.Body.String(), "Secret Roadmap") {
		t.Fatalf("public projection: %d %s", result.Code, result.Body.String())
	}
}

func TestPublicKnowledgeSettingsEmptyTagsOverrideEnvironment(t *testing.T) {
	t.Setenv(publicKnowledgeEnabledEnv, "0")
	t.Setenv(publicKnowledgeTagsEnv, "private")
	s := publicKnowledgeServer(t)
	s.deps.Config.Knowledge.Public = &config.PublicKnowledgeConfig{Enabled: true}
	w := publicSettingsRequest(s, "GET", "", true)
	if !strings.Contains(w.Body.String(), `"tags":[]`) {
		t.Fatalf("tags should be an empty array: %s", w.Body.String())
	}
	result := mcpCall(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"knowledge_export"}}`)
	if !strings.Contains(result.Body.String(), "Nil Map Gotcha") {
		t.Fatalf("empty tags must override env scope: %s", result.Body.String())
	}
}

func TestPublicKnowledgeSettingsOwnerGate(t *testing.T) {
	s := publicKnowledgeServer(t)
	for _, method := range []string{"GET", "PUT"} {
		if w := publicSettingsRequest(s, method, `{"enabled":true,"tags":[]}`, false); w.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s: %d", method, w.Code)
		}
	}
	for _, role := range []string{"read", "read-write", "merger", "owner"} {
		r := httptest.NewRequest("PUT", "/api/knowledge/public", strings.NewReader(`{"enabled":true,"tags":[]}`))
		r.Header.Set("X-Hive-Role", role)
		// Even an owner label without the auth middleware's proof must fail.
		w := httptest.NewRecorder()
		s.handlePublicKnowledgeSettingsUpdate(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: %d", role, w.Code)
		}
	}
	if s.deps.Config.Knowledge.Public != nil {
		t.Fatal("unauthorized mutation")
	}
}

func TestPublicKnowledgeSettingsInvalidAndFailedSave(t *testing.T) {
	t.Setenv(publicKnowledgeEnabledEnv, "0")
	s := publicKnowledgeServer(t)
	s.deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	for _, body := range []string{`{}`, `{"enabled":null}`, `{"enabled":"yes"}`, `{"enabled":true,"tags":[1]}`} {
		if w := publicSettingsRequest(s, "PUT", body, true); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, w.Code)
		}
	}
	// Exercise the real persistence guard without writing any config file.
	s.deps.Config.Project.Org = ""
	w := publicSettingsRequest(s, "PUT", `{"enabled":true,"tags":[]}`, true)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("save failure: %d", w.Code)
	}
	if s.publicKnowledgeSettings().Enabled || s.deps.Config.Knowledge.Public != nil {
		t.Fatal("failed save exposed knowledge")
	}
	s.deps.Config.SourcePath = ""
	if w := publicSettingsRequest(s, "PUT", `{"enabled":true,"tags":[]}`, true); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no persistence: %d", w.Code)
	}
}
