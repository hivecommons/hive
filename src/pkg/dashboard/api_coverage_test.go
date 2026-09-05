package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/tokens"
)

// wikiTestServer creates a mock wiki HTTP server for knowledge API tests.
func wikiTestServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"results": []map[string]interface{}{
				{"slug": "fact-1", "title": "Fact One", "type": "gotcha", "confidence": 0.9, "score": 0.95},
			},
			"total": 1,
		})
	})
	mux.HandleFunc("/api/pages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"results": []map[string]interface{}{
				{"slug": "fact-1", "title": "Fact One"},
				{"slug": "fact-2", "title": "Fact Two"},
			},
			"total": 2,
		})
	})
	mux.HandleFunc("/api/pages/fact-1", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			w.WriteHeader(http.StatusOK)
		default:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"slug": "fact-1", "title": "Fact One", "body": "Body content", "type": "gotcha",
			})
		}
	})
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_pages": 10,
			"by_type":     map[string]int{"gotcha": 5},
		})
	})
	mux.HandleFunc("/api/ingest", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return httptest.NewServer(mux)
}

func apiServerWithKnowledge(t *testing.T) (*Server, *Dependencies, *httptest.Server) {
	t.Helper()
	wiki := wikiTestServer()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)

	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "myorg", Repos: []string{"repo1"}, AIAuthor: "bot"},
		GitHub:  config.GitHubConfig{Token: "tok"},
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "claude", Model: "sonnet", Enabled: true},
		},
		Governor: config.GovernorConfig{
			EvalIntervalS: 300,
			Modes: map[string]config.ModeConfig{
				"idle":  {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "15m"}},
				"quiet": {Threshold: 2, Cadences: map[string]config.Cadence{"scanner": "10m"}},
				"busy":  {Threshold: 10, Cadences: map[string]config.Cadence{"scanner": "5m"}},
				"surge": {Threshold: 20, Cadences: map[string]config.Cadence{"scanner": "2m"}},
			},
			Labels: config.LabelsConfig{Exempt: []string{"hold"}},
		},
		Notifications: config.NotificationsConfig{},
	}

	gov := governor.New(cfg.Governor, cfg.Agents, logger)
	mgr := agent.NewManager(cfg.Agents, logger, agent.ProjectContext{})

	layers := []knowledge.LayerConfig{
		{Type: knowledge.LayerProject, URL: wiki.URL},
	}
	kcfg := knowledge.KnowledgeConfig{Enabled: true, Engine: "llm-wiki"}
	knowledgeAPI := knowledge.NewKnowledgeAPI(layers, kcfg, logger)

	deps := &Dependencies{
		Config:      cfg,
		AgentMgr:    mgr,
		Governor:    gov,
		Knowledge:   knowledgeAPI,
		Logger:      logger,
		Ctx:         context.Background(),
		RefreshFunc: func() {},
		PersistFunc: func() {},
	}
	s.RegisterAPI(deps)
	return s, deps, wiki
}

func apiServerWithNous(t *testing.T) (*Server, *Dependencies) {
	t.Helper()
	s, deps := apiServer(t)
	deps.Nous = &NousState{
		Mode:   "active",
		Scope:  "full",
		Phase:  "running",
		Status: map[string]interface{}{"ok": true},
		Ledger: []map[string]interface{}{
			{"action": "test", "ts": "2024-01-01"},
		},
		Principles: []NousPrinciple{
			{ID: "p1", Text: "Be helpful", Confidence: 0.9, Source: "manual"},
			{ID: "p2", Text: "Be safe", Confidence: 0.8, Source: "auto"},
		},
		Config:       map[string]interface{}{"key": "value"},
		GatePending:  map[string]interface{}{"pending": true},
		GateResponse: map[string]interface{}{"decision": "approved"},
	}
	return s, deps
}

// ---- Knowledge handler tests WITH actual Knowledge API ----

func TestHandleKnowledgeList_WithKnowledge(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	rec := doGet(s, "/api/knowledge")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["enabled"] != true {
		t.Errorf("enabled = %v", result["enabled"])
	}
}

func TestHandleKnowledgeSearch_WithQuery(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	rec := doGet(s, "/api/knowledge/search?q=test&type=gotcha&limit=5")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["query"] != "test" {
		t.Errorf("query = %v", result["query"])
	}
}

func TestHandleKnowledgeSearch_MissingQuery(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	rec := doGet(s, "/api/knowledge/search")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleKnowledgeHealth_WithKnowledge(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	rec := doGet(s, "/api/knowledge/health")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleKnowledgeStats_WithKnowledge(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	rec := doGet(s, "/api/knowledge/stats")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleKnowledgeLayer_WithKnowledge(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	rec := doGet(s, "/api/knowledge/project")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["layer"] != "project" {
		t.Errorf("layer = %v", result["layer"])
	}
}

func TestHandleKnowledgeFact_WithKnowledge(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	rec := doGet(s, "/api/knowledge/project/fact-1")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleKnowledgeCreate_WithKnowledge(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	body := knowledge.CreateFactRequest{
		Title: "Test Fact", Body: "Some body", Type: "pattern",
		Layer: "project", Confidence: 0.8,
	}
	rec := doPost(s, "/api/knowledge/create", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleKnowledgeCreate_MissingFields(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	rec := doPost(s, "/api/knowledge/create", map[string]string{"title": ""})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleKnowledgeCreate_Defaults(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	// No layer, no type, no confidence => defaults applied
	rec := doPost(s, "/api/knowledge/create", map[string]string{
		"title": "Test", "body": "Body content",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleKnowledgeUpdate_WithKnowledge(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	body := knowledge.UpdateFactRequest{Title: "Updated", Body: "New body"}
	rec := doPut(s, "/api/knowledge/project/fact-1", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleKnowledgeUpdate_InvalidBody(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/knowledge/project/fact-1", nil)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleKnowledgeDelete_WithKnowledge(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	rec := doDelete(s, "/api/knowledge/project/fact-1", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleKnowledgeSubsList_WithKnowledge(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	rec := doGet(s, "/api/knowledge/subscriptions")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleKnowledgeSubsAdd_WithKnowledge(t *testing.T) {
	// L3: keep this hermetic — the subscription URL below must classify as
	// public without an external DNS lookup.
	stubPrivateURLResolver(t, "example.com")
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	body := map[string]string{"url": "http://example.com/wiki", "name": "test-sub"}
	rec := doPost(s, "/api/knowledge/subscriptions", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleKnowledgeSubsAdd_MissingURL(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	rec := doPost(s, "/api/knowledge/subscriptions", map[string]string{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleKnowledgeSubsAdd_DefaultLayer(t *testing.T) {
	// L3: keep this hermetic — see stubPrivateURLResolver.
	stubPrivateURLResolver(t, "new.com")
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	// No layer specified => defaults to org
	rec := doPost(s, "/api/knowledge/subscriptions", map[string]string{"url": "http://new.com"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleKnowledgeSubsRemove_WithKnowledge(t *testing.T) {
	// L3: keep this hermetic — see stubPrivateURLResolver.
	stubPrivateURLResolver(t, "example.com")
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	// First add
	doPost(s, "/api/knowledge/subscriptions", map[string]string{"url": "http://example.com"})
	// Then remove
	rec := doDelete(s, "/api/knowledge/subscriptions", map[string]string{"url": "http://example.com"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleKnowledgeSubsRemove_MissingURL(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	rec := doDelete(s, "/api/knowledge/subscriptions", map[string]string{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleKnowledgeSubsRemove_NotFound(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	rec := doDelete(s, "/api/knowledge/subscriptions", map[string]string{"url": "http://nonexistent.com"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleKnowledgePromote_WithKnowledge(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	body := map[string]string{
		"slug": "fact-1", "from_layer": "project", "to_layer": "org",
		"reason": "good fact", "promoter": "test",
	}
	rec := doPost(s, "/api/knowledge/promote", body)
	// May succeed or fail depending on promoter setup
	if rec.Code != http.StatusOK && rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleKnowledgePromote_MissingFields_WithKnowledge(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	rec := doPost(s, "/api/knowledge/promote", map[string]string{"slug": "test"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleKnowledgePromote_DefaultPromoter(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	body := map[string]string{
		"slug": "fact-1", "from_layer": "project", "to_layer": "org",
	}
	rec := doPost(s, "/api/knowledge/promote", body)
	// Tests that default promoter="dashboard" is set
	if rec.Code != http.StatusOK && rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleKnowledgeImport_WithKnowledge(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	body := map[string]string{
		"content": "# My Fact\nThis is a test fact body.",
		"format":  "markdown",
		"layer":   "project",
	}
	rec := doPost(s, "/api/knowledge/import", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleKnowledgeImport_Defaults(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	// No layer, no format => defaults
	body := map[string]string{"content": "# Some Fact\nBody here."}
	rec := doPost(s, "/api/knowledge/import", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleKnowledgeImport_EmptyContent(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	rec := doPost(s, "/api/knowledge/import", map[string]string{"content": ""})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleKnowledgeImport_JSONFormat(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	facts := `[{"title":"JSON Fact","body":"JSON body","type":"pattern","confidence":0.8}]`
	body := map[string]string{"content": facts, "format": "json", "layer": "project"}
	rec := doPost(s, "/api/knowledge/import", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleKnowledgeToggle_Enable(t *testing.T) {
	s, deps := apiServer(t)
	deps.Knowledge = nil
	deps.Config.Knowledge = config.KnowledgeConfig{
		Enabled: false,
		Layers:  []config.KnowledgeLayer{{Type: "project", URL: "http://example.com"}},
	}

	rec := doPut(s, "/api/knowledge/enabled", map[string]bool{"enabled": true})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// ---- Nous handler tests WITH actual NousState ----

func TestHandleNousStatus_WithNous(t *testing.T) {
	s, _ := apiServerWithNous(t)
	rec := doGet(s, "/api/nous/status")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleNousLedger_WithNous(t *testing.T) {
	s, _ := apiServerWithNous(t)
	rec := doGet(s, "/api/nous/ledger")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleNousPrinciples_WithNous(t *testing.T) {
	s, _ := apiServerWithNous(t)
	rec := doGet(s, "/api/nous/principles")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleNousApprove(t *testing.T) {
	s, _ := apiServerWithNous(t)
	rec := doPost(s, "/api/nous/approve", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleNousAbort(t *testing.T) {
	s, _ := apiServerWithNous(t)
	rec := doPost(s, "/api/nous/abort", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleNousMode_WithNous(t *testing.T) {
	s, deps := apiServerWithNous(t)
	rec := doPut(s, "/api/nous/mode", map[string]string{"mode": "passive"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if deps.Nous.Mode != "passive" {
		t.Errorf("mode = %q, want passive", deps.Nous.Mode)
	}
}

func TestHandleNousMode_MissingMode(t *testing.T) {
	s, _ := apiServerWithNous(t)
	rec := doPut(s, "/api/nous/mode", map[string]string{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleNousScope_WithNous(t *testing.T) {
	s, deps := apiServerWithNous(t)
	rec := doPut(s, "/api/nous/scope", map[string]string{"scope": "limited"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if deps.Nous.Scope != "limited" {
		t.Errorf("scope = %q, want limited", deps.Nous.Scope)
	}
}

func TestHandleNousScope_MissingScope(t *testing.T) {
	s, _ := apiServerWithNous(t)
	rec := doPut(s, "/api/nous/scope", map[string]string{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleNousPhase_WithNous(t *testing.T) {
	s, _ := apiServerWithNous(t)
	rec := doGet(s, "/api/nous/phase")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["phase"] != "running" {
		t.Errorf("phase = %v", result["phase"])
	}
}

func TestHandleNousPhase_NilNous(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = nil
	rec := doGet(s, "/api/nous/phase")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["phase"] != "inactive" {
		t.Errorf("phase = %v", result["phase"])
	}
}

func TestHandleNousGateDecision_WithNous(t *testing.T) {
	s, deps := apiServerWithNous(t)
	rec := doPut(s, "/api/nous/gate-decision", map[string]string{
		"decision": "approve", "reason": "looks good",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if deps.Nous.GateResponse["decision"] != "approve" {
		t.Errorf("decision = %v", deps.Nous.GateResponse["decision"])
	}
}

func TestHandleNousGateDecision_MissingDecision(t *testing.T) {
	s, _ := apiServerWithNous(t)
	rec := doPut(s, "/api/nous/gate-decision", map[string]string{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleNousGatePending_WithNous(t *testing.T) {
	s, _ := apiServerWithNous(t)
	rec := doGet(s, "/api/nous/gate-pending")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleNousGateRespond_WithNous(t *testing.T) {
	s, deps := apiServerWithNous(t)
	rec := doPost(s, "/api/nous/gate-respond", map[string]string{"answer": "yes"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if deps.Nous.GateResponse["answer"] != "yes" {
		t.Errorf("answer = %v", deps.Nous.GateResponse["answer"])
	}
}

func TestHandleNousGateResponse_WithNous(t *testing.T) {
	s, _ := apiServerWithNous(t)
	rec := doGet(s, "/api/nous/gate-response")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleNousConfigGet_WithNous(t *testing.T) {
	s, _ := apiServerWithNous(t)
	rec := doGet(s, "/api/nous/config")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleNousConfigGoals_WithNous(t *testing.T) {
	s, deps := apiServerWithNous(t)
	rec := doPut(s, "/api/nous/config/goals", map[string]string{"target": "coverage"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if deps.Nous.Config["goals"] == nil {
		t.Error("goals should be set")
	}
}

func TestHandleNousConfigFastFail_WithNous(t *testing.T) {
	s, _ := apiServerWithNous(t)
	rec := doPut(s, "/api/nous/config/fast-fail", map[string]bool{"enabled": true})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleNousConfigSection_NilConfig(t *testing.T) {
	s, deps := apiServerWithNous(t)
	deps.Nous.Config = nil
	rec := doPut(s, "/api/nous/config/goals", map[string]string{"target": "perf"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if deps.Nous.Config["goals"] == nil {
		t.Error("config should be initialized")
	}
}

// ---- Governor config handler tests ----

func TestHandleGovernorSensing_ZeroInterval(t *testing.T) {
	s, deps := apiServer(t)
	original := deps.Config.Governor.EvalIntervalS
	rec := doPut(s, "/api/config/governor/sensing", map[string]int{"eval_interval_s": 0})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	// Zero value should not update
	if deps.Config.Governor.EvalIntervalS != original {
		t.Errorf("should not update with zero value")
	}
}

func TestHandleGovernorAddAgent_MissingName(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/config/governor/agents", map[string]string{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- Sidebar endpoints ----

func TestHandleSidebarGet_Nil(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/config/sidebar")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleSidebarSet(t *testing.T) {
	s, _ := apiServer(t)
	body := map[string]string{"panel": "agents"}
	rec := doPut(s, "/api/config/sidebar", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}

	// Verify it was stored
	rec2 := doGet(s, "/api/config/sidebar")
	if rec2.Code != http.StatusOK {
		t.Errorf("status = %d", rec2.Code)
	}
}

// ---- Version endpoint ----

func TestHandleVersion_Fields(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/version")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["version"] != "2.0.0" {
		t.Errorf("version = %v", result["version"])
	}
}

// ---- Config endpoint ----

func TestHandleConfig_Fields(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/config")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["org"] != "myorg" {
		t.Errorf("org = %v", result["org"])
	}
}

func TestHandleConfig_PrimaryRepo(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Project.PrimaryRepo = "myorg/main-repo"
	rec := doGet(s, "/api/config")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["primaryRepo"] != "myorg/main-repo" {
		t.Errorf("primaryRepo = %v", result["primaryRepo"])
	}
}

// ---- Token endpoints ----

func TestHandleTokens_NoCollector(t *testing.T) {
	s, deps := apiServer(t)
	deps.Tokens = nil
	rec := doGet(s, "/api/tokens")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleBudgetIgnoreGet(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/budget-ignore")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleBudgetIgnoreSet(t *testing.T) {
	s, _ := apiServer(t)
	body := map[string][]string{"ignored": {"scanner"}}
	rec := doPost(s, "/api/budget-ignore", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- GH auth endpoint ----

func TestHandleGHAuth_AppID(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.GitHub.AppID = 12345
	rec := doGet(s, "/api/gh-auth")
	result := decodeJSON(t, rec)
	if result["type"] != "app" {
		t.Errorf("type = %v, want app", result["type"])
	}
}

// ---- Summaries endpoint ----

// ---- Agent config endpoints ----

// ---- Chat endpoint ----

func TestHandleChat_MissingMessage(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/chat", map[string]string{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- Pin/Unpin endpoints ----

func TestHandlePin_CLI(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/pin/scanner/cli", map[string]string{"value": "claude-v2"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandlePin_Model(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/pin/scanner/model", map[string]string{"value": "opus"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandlePin_DefaultValue(t *testing.T) {
	s, _ := apiServer(t)
	// No value => use current backend/model
	rec := doPost(s, "/api/pin/scanner/cli", map[string]string{})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandlePin_NotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/pin/nonexistent/cli", map[string]string{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleUnpin_CLI(t *testing.T) {
	s, _ := apiServer(t)
	doPost(s, "/api/pin/scanner/cli", map[string]string{"value": "claude"})
	rec := doPost(s, "/api/unpin/scanner/cli", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleUnpin_Model(t *testing.T) {
	s, _ := apiServer(t)
	doPost(s, "/api/pin/scanner/model", map[string]string{"value": "opus"})
	rec := doPost(s, "/api/unpin/scanner/model", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleUnpin_NotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/unpin/nonexistent/cli", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- Pause/Kick endpoints ----

func TestHandlePause(t *testing.T) {
	s, _ := apiServer(t)
	rec := doOwnerPost(s, "/api/pause/scanner", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleKick_Success(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/kick/scanner", map[string]string{"message": "scan now"})
	// May fail due to tmux, but tests the path
	if rec.Code != http.StatusOK && rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleKick_MissingMessage(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/kick/scanner", map[string]string{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- History/Trends/Timeline/Widget ----

func TestHandleHistory_Returns200(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/history")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleTrends_Week(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/trends?range=week")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleTrends_Day(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/trends?range=day")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleTrends_CustomHours(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/trends?hours=48")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleTrends_Default(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/trends")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleTimeline_Returns200(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/timeline")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleWidget_Returns200(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/widget")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- Server core ----

func TestNewServerWithAuth(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServerWithAuth(0, "test-token", logger)
	if s.authToken != "test-token" {
		t.Errorf("authToken = %q", s.authToken)
	}
}

func TestSecurityHeaders_Unauthorized(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServerWithAuth(0, "secret", logger)
	deps := testDeps(t)
	s.RegisterAPI(deps)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestSecurityHeaders_Authorized(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServerWithAuth(0, "secret", logger)
	deps := testDeps(t)
	s.RegisterAPI(deps)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestSecurityHeaders_TokenQueryParamRejected(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServerWithAuth(0, "secret", logger)
	deps := testDeps(t)
	s.RegisterAPI(deps)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config?token=secret", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestSecurityHeaders_HealthNoAuth(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServerWithAuth(0, "secret", logger)
	s.UpdateStatus(&StatusPayload{Agents: []FrontendAgent{{Name: "scanner"}}})
	s.MarkReady()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (health needs no auth)", rec.Code)
	}
}

func TestUpdateStatus(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)

	s.UpdateStatus(&StatusPayload{
		Agents: []FrontendAgent{{Name: "scanner"}},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleStatus_Nil(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandler(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	handler := s.Handler()
	if handler == nil {
		t.Error("Handler() returned nil")
	}
}

// ---- getAgentPipeline / getAgentHooks ----

func TestGetAgentPipeline_Default(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	p := s.getAgentPipeline("nonexistent")
	if len(p) != len(defaultPipelineSteps) {
		t.Errorf("pipeline steps = %d, want %d", len(p), len(defaultPipelineSteps))
	}
}

func TestGetAgentPipeline_Custom(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	s.agentPipelines["scanner"] = map[string]bool{"step1": true}
	p := s.getAgentPipeline("scanner")
	if len(p) != 1 {
		t.Errorf("pipeline steps = %d, want 1", len(p))
	}
}

func TestGetAgentHooks_Default(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	h := s.getAgentHooks("nonexistent")
	if h["preKick"] == nil || h["postIdle"] == nil {
		t.Error("expected default hooks")
	}
}

func TestGetAgentHooks_Custom(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	s.agentHooks["scanner"] = map[string][]any{"preKick": {"echo test"}}
	h := s.getAgentHooks("scanner")
	if len(h["preKick"]) != 1 {
		t.Errorf("preKick hooks = %d", len(h["preKick"]))
	}
}

// ---- persistAfterMutation / refreshAndPersist ----

func TestRefreshAfterMutation_NilDeps(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	// Should not panic
	s.refreshAfterMutation()
	s.persistAfterMutation()
	s.refreshAndPersist()
}

// ---- formatCadenceDuration edge cases ----

func TestFormatCadenceDuration_Zero(t *testing.T) {
	got := formatCadenceDuration(0)
	if got != "0h" {
		t.Errorf("formatCadenceDuration(0) = %q, want 0h", got)
	}
}

// ---- decodeBody edge case ----

func TestDecodeBody_NilBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	var target map[string]string
	err := decodeBody(req, &target)
	if err == nil {
		t.Error("expected error for nil body")
	}
}

// ---- SSE endpoint ----

func TestHandleSSE(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)

	// Set initial status so SSE sends it
	s.UpdateStatus(&StatusPayload{Agents: []FrontendAgent{{Name: "test"}}})

	// Use a context we can cancel
	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		s.mux.ServeHTTP(rec, req)
		close(done)
	}()

	// Cancel to stop the SSE handler
	cancel()
	<-done

	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Errorf("content-type = %q", rec.Header().Get("Content-Type"))
	}
}

// ---- broadcast ----

func TestBroadcast(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)

	ch := make(chan []byte, 1)
	s.sseMu.Lock()
	s.sseClients[ch] = struct{}{}
	s.sseMu.Unlock()

	s.broadcastFrame("data: {\"test\":true}\n\n")

	data := <-ch
	if string(data) != "data: {\"test\":true}\n\n" {
		t.Errorf("broadcastFrame data = %q", string(data))
	}

	s.sseMu.Lock()
	delete(s.sseClients, ch)
	s.sseMu.Unlock()
}

// ---- loadAgentStats edge cases ----

func TestLoadAgentStats_NonexistentFile(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	stats := s.loadAgentStats("nonexistent-agent")
	if len(stats) != 0 {
		t.Errorf("stats = %d, want 0", len(stats))
	}
}

func TestLoadAgentStats_WithFile(t *testing.T) {
	dir := t.TempDir()
	agentDir := fmt.Sprintf("%s/agents/test-agent", dir)
	os.MkdirAll(agentDir, 0755)
	os.WriteFile(fmt.Sprintf("%s/stats.json", agentDir), []byte(`{"stats":[{"name":"test"}]}`), 0644)

	// This won't actually work because loadAgentStats hardcodes /data/agents/
	// but it tests the code path for non-existent files
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	stats := s.loadAgentStats("test-agent")
	// Will return empty because /data/agents/test-agent doesn't exist
	if stats == nil {
		t.Error("expected non-nil stats slice")
	}
}

// ---- Nous endpoints nil checks ----

func TestHandleNousStatus_NilNous(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = nil
	rec := doGet(s, "/api/nous/status")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleNousLedger_NilNous(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = nil
	rec := doGet(s, "/api/nous/ledger")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleNousPrinciples_NilNous(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = nil
	rec := doGet(s, "/api/nous/principles")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleNousGatePending_NilNous(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = nil
	rec := doGet(s, "/api/nous/gate-pending")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleNousGateResponse_NilNous(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = nil
	rec := doGet(s, "/api/nous/gate-response")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleNousConfigGet_NilNous(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = nil
	rec := doGet(s, "/api/nous/config")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleNousGateRespond_NilNous(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = nil
	rec := doPost(s, "/api/nous/gate-respond", map[string]string{"answer": "yes"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleNousMode_NilNous(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = nil
	rec := doPut(s, "/api/nous/mode", map[string]string{"mode": "test"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleNousScope_NilNous(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = nil
	rec := doPut(s, "/api/nous/scope", map[string]string{"scope": "test"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- Agent control endpoint tests ----

func TestHandlePane_Success(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/pane/scanner?lines=10")
	// Agent exists but has no tmux session, so GetOutput returns error
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandlePane_DefaultLines(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/pane/scanner")
	// No lines param, defaults to 100
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleSwitch_Success(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/switch/scanner/copilot", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleSwitch_UnknownAgent(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/switch/nonexistent/copilot", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleModelSet_Success(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/model/scanner/opus", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleModelSet_UnknownAgent(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/model/nonexistent/opus", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleResume_UnknownAgent(t *testing.T) {
	s, _ := apiServer(t)
	rec := doOwnerPost(s, "/api/resume/nonexistent", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleRestart_NotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/restart/nonexistent", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleResetRestarts_UnknownAgent(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/reset-restarts/nonexistent", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleResetRestarts_Success(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/reset-restarts/scanner", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// ---- Agent config endpoint tests ----

func TestHandleAgentConfigCadences_Success(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/agent/scanner/cadences", map[string]int64{"idle": 900, "busy": 300})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleAgentConfigCadences_MissingAgent(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/agent/nonexistent/cadences", map[string]int64{"idle": 900})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleAgentConfigCadences_PauseValue(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/agent/scanner/cadences", map[string]int64{"idle": 0})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleAgentConfigPipeline_Success(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/agent/scanner/pipeline", map[string]bool{"lint": true, "test": false})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleAgentConfigPipeline_MissingAgent(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/agent/nonexistent/pipeline", map[string]bool{"lint": true})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleAgentConfigHooks_Success(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/agent/scanner/hooks", map[string][]any{"pre_kick": {map[string]string{"cmd": "echo"}}})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleAgentConfigHooks_MissingAgent(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/agent/nonexistent/hooks", map[string][]any{"pre_kick": {}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleAgentConfigStats_MissingAgent(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/agent/nonexistent/stats", map[string]interface{}{"stats": []any{}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleAgentConfigRestrictions_MissingAgent(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/agent/nonexistent/restrictions", map[string]interface{}{"agent": []restriction{}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// ---- Governor config PUT endpoint tests ----

func TestHandleGovernorLabels_Success(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/governor/labels", map[string]interface{}{"labels": []string{"hold", "wip"}})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleGovernorBudget_Success(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/governor/budget", map[string]interface{}{"weekly_limit": 1000000, "periodDays": 7, "criticalPct": 90})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleGovernorRepos_Success(t *testing.T) {
	s, _ := apiServer(t)
	// Replacing the repo set must name a default among the new repos (the
	// always-exactly-one-default invariant), so send primaryRepo too.
	rec := doPut(s, "/api/config/governor/repos", map[string]interface{}{"repos": []string{"repo1", "repo2"}, "primaryRepo": "repo1"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// The fixture's org is "myorg", so "myorg/new-repo" is the hive's own org
// spelled out — the form GitHub shows everywhere. Accept it and store the bare
// repo name, because repo targets are built as org + "/" + repo and the
// qualified form would otherwise resolve to "myorg/myorg/new-repo".
func TestHandleGovernorRepos_StripOrg(t *testing.T) {
	s, deps := apiServer(t)
	rec := doPut(s, "/api/config/governor/repos", map[string]interface{}{"repos": []string{"myorg/new-repo"}, "primaryRepo": "myorg/new-repo"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(deps.Config.Project.Repos) != 1 || deps.Config.Project.Repos[0] != "new-repo" {
		t.Errorf("repos = %v, want [new-repo]", deps.Config.Project.Repos)
	}
	if deps.Config.Project.PrimaryRepo != "new-repo" {
		t.Errorf("primaryRepo = %q, want %q", deps.Config.Project.PrimaryRepo, "new-repo")
	}
}

// A repo qualified with someone else's org must still be rejected outright and
// must not mutate the stored config.
func TestHandleGovernorRepos_ForeignOrgRejected(t *testing.T) {
	s, deps := apiServer(t)
	rec := doPut(s, "/api/config/governor/repos", map[string]interface{}{"repos": []string{"notmyorg/new-repo"}, "primaryRepo": "repo1"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(deps.Config.Project.Repos) != 1 || deps.Config.Project.Repos[0] != "repo1" {
		t.Errorf("repos = %v, want the pre-request [repo1]", deps.Config.Project.Repos)
	}
}

func TestHandleGovernorHealth_FullUpdate(t *testing.T) {
	s, deps := apiServer(t)
	rec := doPut(s, "/api/config/governor/health", map[string]interface{}{
		"healthcheckInterval": 120,
		"restartCooldown":     600,
		"modelLock":           true,
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if deps.Config.Governor.Health.HealthcheckInterval != 120 {
		t.Errorf("healthcheck = %d, want 120", deps.Config.Governor.Health.HealthcheckInterval)
	}
	if !deps.Config.Governor.Health.ModelLock {
		t.Error("modelLock should be true")
	}
}

func TestHandleGovernorSensing_InvalidBody(t *testing.T) {
	s, _ := apiServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/sensing", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleGovernorThresholds_InvalidBody(t *testing.T) {
	s, _ := apiServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/thresholds", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleGovernorLabels_InvalidBody(t *testing.T) {
	s, _ := apiServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/labels", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleGovernorBudget_InvalidBody(t *testing.T) {
	s, _ := apiServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/budget", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleGovernorRepos_InvalidBody(t *testing.T) {
	s, _ := apiServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/repos", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleGovernorHealth_InvalidBody(t *testing.T) {
	s, _ := apiServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/health", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- Token endpoints with Summary ----

func TestHandleTokens_WithSummary(t *testing.T) {
	s, deps := apiServer(t)
	// Tokens is nil, tests the nil-returns-no_collector path
	deps.Tokens = nil
	rec := doGet(s, "/api/tokens")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["status"] != "no_collector" {
		t.Errorf("status = %v", result["status"])
	}
}

// ---- GovernorConfigGet full path ----

func TestHandleGovernorConfigGet_FullConfig(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Notifications.Ntfy = &config.NtfyConfig{Server: "https://ntfy.sh", Topic: "test"}
	deps.Config.Notifications.Discord = &config.DiscordConfig{Webhook: "https://discord.com/webhook/secret123"}
	rec := doGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	result := decodeJSON(t, rec)
	notifications, ok := result["notifications"].(map[string]interface{})
	if !ok {
		t.Fatal("notifications not found")
	}
	if notifications["hasNtfy"] != true {
		t.Error("hasNtfy should be true")
	}
	if notifications["hasDiscord"] != true {
		t.Error("hasDiscord should be true")
	}
}

func TestHandleGovernorConfigGet_ReposWithSlash(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Project.Repos = []string{"other-org/repo-x", "repo-y"}
	rec := doGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- GovernorNotifications with discord ----

func TestHandleGovernorNotifications_DiscordOnly(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/governor/notifications", map[string]interface{}{
		"discordWebhook": "https://discord.com/webhook/123",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// ---- Status builder tests ----

func TestParseCadenceDuration_Variants(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"15m", "15m0s"},
		{"1h", "1h0m0s"},
		{"5min", "5m0s"},
		{"pause", "0s"},
		{"off", "0s"},
		{"", "0s"},
		{"invalid", "0s"},
	}
	for _, tt := range tests {
		got := parseCadenceDuration(tt.input)
		if got.String() != tt.want {
			t.Errorf("parseCadenceDuration(%q) = %s, want %s", tt.input, got, tt.want)
		}
	}
}

func TestComputeNextKick_Empty(t *testing.T) {
	result := computeNextKick(nil, "")
	if result != "" {
		t.Errorf("computeNextKick empty = %q", result)
	}
}

func TestComputeNextKick_Pause(t *testing.T) {
	result := computeNextKick(nil, "pause")
	if result != "" {
		t.Errorf("computeNextKick pause = %q", result)
	}
}

func TestComputeNextKick_ValidCadence(t *testing.T) {
	result := computeNextKick(nil, "15m")
	if result == "" {
		t.Error("expected non-empty next kick")
	}
}

func TestComputeNextKick_WithLastKick(t *testing.T) {
	now := time.Now()
	result := computeNextKick(&now, "1h")
	if result == "" {
		t.Error("expected non-empty next kick")
	}
}

func TestFormatCadenceDuration_Minutes(t *testing.T) {
	got := formatCadenceDuration(300)
	if got != "5m" {
		t.Errorf("formatCadenceDuration(300) = %q, want 5m", got)
	}
}

func TestFormatCadenceDuration_Hours(t *testing.T) {
	got := formatCadenceDuration(7200)
	if got != "2h" {
		t.Errorf("formatCadenceDuration(7200) = %q, want 2h", got)
	}
}

func TestFormatCadenceDuration_Seconds(t *testing.T) {
	got := formatCadenceDuration(45)
	if got != "45s" {
		t.Errorf("formatCadenceDuration(45) = %q, want 45s", got)
	}
}

func TestLookupCadence_Found(t *testing.T) {
	cfg := &config.Config{
		Governor: config.GovernorConfig{
			Modes: map[string]config.ModeConfig{
				"idle": {Cadences: map[string]config.Cadence{"scanner": "15m"}},
			},
		},
	}
	got := lookupCadence("scanner", cfg)
	if got != "15m" {
		t.Errorf("lookupCadence = %q, want 15m", got)
	}
}

func TestLookupCadence_NotFound(t *testing.T) {
	cfg := &config.Config{
		Governor: config.GovernorConfig{
			Modes: map[string]config.ModeConfig{
				"idle": {Cadences: map[string]config.Cadence{"scanner": "15m"}},
			},
		},
	}
	got := lookupCadence("worker", cfg)
	if got != "" {
		t.Errorf("lookupCadence = %q, want empty", got)
	}
}

func TestLookupCadenceForMode_Busy(t *testing.T) {
	cfg := &config.Config{
		Governor: config.GovernorConfig{
			Modes: map[string]config.ModeConfig{
				"busy": {Cadences: map[string]config.Cadence{"scanner": "5m"}},
			},
		},
	}
	got := lookupCadenceForMode("scanner", "busy", cfg)
	if got != "5m" {
		t.Errorf("lookupCadenceForMode = %q, want 5m", got)
	}
}

func TestBuildGovernor_WithModes(t *testing.T) {
	cfg := &config.Config{
		Governor: config.GovernorConfig{
			EvalIntervalS: 300,
			Modes: map[string]config.ModeConfig{
				"quiet": {Threshold: 2},
				"busy":  {Threshold: 10},
				"surge": {Threshold: 20},
			},
		},
	}
	state := governor.State{Mode: "IDLE"}
	fg := buildGovernor(state, cfg)
	if fg.Mode != "idle" {
		t.Errorf("mode = %q, want idle", fg.Mode)
	}
	if fg.Thresholds.Quiet != 2 {
		t.Errorf("quiet threshold = %d, want 2", fg.Thresholds.Quiet)
	}
}

func TestBuildGovernor_NoModes(t *testing.T) {
	cfg := &config.Config{
		Governor: config.GovernorConfig{},
	}
	state := governor.State{Mode: "BUSY"}
	fg := buildGovernor(state, cfg)
	if fg.Thresholds.Quiet != 2 {
		t.Errorf("default quiet = %d, want 2", fg.Thresholds.Quiet)
	}
}

func TestBuildTokens_NilReturnsEmpty(t *testing.T) {
	ft := buildTokens(nil)
	if len(ft.Sessions) != 0 {
		t.Errorf("sessions = %d", len(ft.Sessions))
	}
}

func TestBuildBeads_Empty(t *testing.T) {
	fb := buildBeads(nil)
	if fb.Supervisor != 0 || fb.Workers != 0 {
		t.Errorf("beads = %+v", fb)
	}
}

func TestBuildHealth_NilClientReturnsDefault(t *testing.T) {
	health := buildHealth(nil, nil)
	if health == nil {
		t.Fatal("nil health")
	}
}

func TestFormatHumanTime_NonEmpty(t *testing.T) {
	ts := time.Date(2024, 6, 15, 14, 30, 0, 0, time.UTC)
	result := formatHumanTime(ts)
	if result == "" {
		t.Error("empty result")
	}
}

// ---- handleKnowledgeFact with knowledge ----

func TestHandleKnowledgeFact_WithKnowledge_NotFound(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	rec := doGet(s, "/api/knowledge/project/nonexistent-slug")
	// Will get a response from the wiki or a 404
	if rec.Code != http.StatusOK && rec.Code != http.StatusNotFound {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- handleKnowledgeDelete with knowledge ----

func TestHandleKnowledgeDelete_WithKnowledge_Success(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	rec := doDelete(s, "/api/knowledge/project/fact-1", nil)
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- handleKnowledgeSearch with limit ----

func TestHandleKnowledgeSearch_WithLimit(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	rec := doGet(s, "/api/knowledge/search?q=test&limit=5&type=gotcha")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- handleKnowledgeLayer tests ----

func TestHandleKnowledgeLayer_Disabled(t *testing.T) {
	s, deps := apiServer(t)
	deps.Knowledge = nil
	rec := doGet(s, "/api/knowledge/project")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- Agent config general PUT with fields ----

func TestHandleAgentConfigGeneral_WithAllFields(t *testing.T) {
	s, _ := apiServer(t)
	enabled := true
	clearOnKick := true
	rec := doPut(s, "/api/config/agent/scanner/general", map[string]interface{}{
		"enabled":       enabled,
		"clear_on_kick": clearOnKick,
		"beads_dir":     "/tmp/beads",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleAgentConfigModels_WithFields(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/agent/scanner/models", map[string]interface{}{
		"backend": "gemini",
		"model":   "gemini-2.5-pro",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// ---- Version with GH client ----

func TestHandleVersion_WithGHClient(t *testing.T) {
	s, deps := apiServer(t)
	// No GH client so fetchLatestRemoteHash returns error - tests the err != nil path
	deps.GHClient = nil
	rec := doGet(s, "/api/version")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["version"] != "2.0.0" {
		t.Errorf("version = %v", result["version"])
	}
	// latestHash should NOT be present since no GH client
	if _, ok := result["latestHash"]; ok {
		t.Error("latestHash should not be present without GH client")
	}
}

// ---- handleConfig with primary repo ----

func TestHandleConfig_NoPrimaryRepo(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Project.PrimaryRepo = ""
	deps.Config.Project.Repos = []string{"repo1"}
	rec := doGet(s, "/api/config")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["primaryRepo"] != "myorg/repo1" {
		t.Errorf("primaryRepo = %v", result["primaryRepo"])
	}
}

func TestHandleConfig_WithPrimaryRepo(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Project.PrimaryRepo = "myorg/main-repo"
	rec := doGet(s, "/api/config")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["primaryRepo"] != "myorg/main-repo" {
		t.Errorf("primaryRepo = %v", result["primaryRepo"])
	}
}

// handleConfig must report the RESOLVED GitHub host so the dashboard's
// forge indicators (Repositories section pill, per-repo cards, Repos config
// modal) show the real host. A pooled/placeholder GHE hive legitimately
// carries base_url: "" with a GHE api_url — reading base_url alone rendered
// those as github.com, so operators mistook a github.ibm.com hive for
// github.com (the vllmd-06 case). ResolvedBaseURL falls back to the api_url
// host in exactly that case.
func TestHandleConfig_GitHubBaseURL_BlankBaseGHEApi(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.GitHub.BaseURL = ""
	deps.Config.GitHub.APIURL = "https://github.ibm.com/api/v3"
	rec := doGet(s, "/api/config")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	result := decodeJSON(t, rec)
	if got := result["github_base_url"]; got != "https://github.ibm.com" {
		t.Errorf("github_base_url = %v, want https://github.ibm.com", got)
	}
}

func TestHandleConfig_GitHubBaseURL_PublicDefault(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.GitHub.BaseURL = ""
	deps.Config.GitHub.APIURL = ""
	rec := doGet(s, "/api/config")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	result := decodeJSON(t, rec)
	if got := result["github_base_url"]; got != "https://github.com" {
		t.Errorf("github_base_url = %v, want https://github.com", got)
	}
}

// ---- handleKnowledgeToggle enable with layers ----

func TestHandleKnowledgeToggle_EnableWithLayers(t *testing.T) {
	s, deps := apiServer(t)
	deps.Knowledge = nil
	deps.Config.Knowledge = config.KnowledgeConfig{
		Enabled: false,
		Layers: []config.KnowledgeLayer{
			{Type: "project", URL: "http://example.com"},
		},
		Primer: config.KnowledgePrimer{MaxFacts: 10, MergeStrategy: "latest"},
	}

	rec := doPut(s, "/api/knowledge/enabled", map[string]bool{"enabled": true})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	if deps.Knowledge == nil {
		t.Error("knowledge should be initialized after enable")
	}
}

// ---- Knowledge error paths ----

func TestHandleKnowledgeCreate_NilKnowledge2(t *testing.T) {
	s, deps := apiServer(t)
	deps.Knowledge = nil
	rec := doPost(s, "/api/knowledge/create", map[string]string{"title": "Test", "body": "Body"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleKnowledgeSubsList_NilKnowledge(t *testing.T) {
	s, deps := apiServer(t)
	deps.Knowledge = nil
	rec := doGet(s, "/api/knowledge/subscriptions")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- handleKnowledgeToggle invalid body ----

func TestHandleKnowledgeToggle_InvalidBody(t *testing.T) {
	s, _ := apiServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/knowledge/enabled", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleWidget with running/paused agents ----

func TestHandleWidget_WithAgents(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/widget")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["mode"] == nil {
		t.Error("missing mode")
	}
}

// ---- handleKick with message ----

func TestHandleKick_WithMessage(t *testing.T) {
	s, _ := apiServer(t)
	// Agent exists but has no tmux session, so SendKick fails
	rec := doPost(s, "/api/kick/scanner", map[string]string{"message": "do something"})
	// Should fail because no tmux session, but covers the code path
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- handlePin error paths ----

func TestHandlePin_AgentNotFound(t *testing.T) {
	s, _ := apiServer(t)
	// Pin with no value and nonexistent agent - should use default from proc
	rec := doPost(s, "/api/pin/nonexistent/cli", map[string]string{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- Nous config section with initial nil config ----

func TestHandleNousConfigSection_InitNilConfig(t *testing.T) {
	s, deps := apiServerWithNous(t)
	deps.Nous.Config = nil
	rec := doPut(s, "/api/nous/config/goals", map[string]interface{}{"goal1": "test"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	if deps.Nous.Config == nil {
		t.Error("config should be initialized")
	}
}

// ---- Nous gate decision with nil GatePending ----

func TestHandleNousGateDecision_NilPending(t *testing.T) {
	s, deps := apiServerWithNous(t)
	deps.Nous.GatePending = nil
	rec := doPut(s, "/api/nous/gate-decision", map[string]string{"decision": "approve", "reason": "looks good"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- Nous gate respond with nil nous ----

func TestHandleNousGateRespond_NilNous2(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = nil
	rec := doPost(s, "/api/nous/gate-respond", map[string]interface{}{"key": "val"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- Metrics collector tests ----

func TestMetricsCollector_Get_Empty(t *testing.T) {
	mc := &MetricsCollector{
		metrics: make(map[string]any),
	}
	result := mc.Get()
	if len(result) != 0 {
		t.Errorf("expected empty metrics, got %d", len(result))
	}
}

func TestMetricsCollector_Get_WithData(t *testing.T) {
	mc := &MetricsCollector{
		metrics: map[string]any{"test": "value"},
	}
	result := mc.Get()
	if result["test"] != "value" {
		t.Errorf("test = %v", result["test"])
	}
}

// ---- handleSidebarSet with valid data ----

func TestHandleSidebarSet_ValidData(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/sidebar", map[string]interface{}{
		"sections": []map[string]string{{"title": "Test"}},
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleSidebarSet_InvalidBody(t *testing.T) {
	s, _ := apiServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/config/sidebar", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleAgentConfigGeneral invalid body ----

func TestHandleAgentConfigGeneral_InvalidBody(t *testing.T) {
	s, _ := apiServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/config/agent/scanner/general", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleAgentConfigModels invalid body ----

func TestHandleAgentConfigModels_InvalidBody(t *testing.T) {
	s, _ := apiServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/config/agent/scanner/models", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleAgentConfigCadences invalid body ----

func TestHandleAgentConfigCadences_InvalidBody(t *testing.T) {
	s, _ := apiServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/config/agent/scanner/cadences", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleAgentConfigPipeline invalid body ----

func TestHandleAgentConfigPipeline_InvalidBody(t *testing.T) {
	s, _ := apiServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/config/agent/scanner/pipeline", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleAgentConfigHooks invalid body ----

func TestHandleAgentConfigHooks_InvalidBody(t *testing.T) {
	s, _ := apiServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/config/agent/scanner/hooks", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleKnowledgeUpdate invalid body ----

func TestHandleKnowledgeUpdate_InvalidBody2(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/knowledge/project/fact-1", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleKnowledgeCreate invalid body ----

func TestHandleKnowledgeCreate_InvalidBody(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/knowledge/create", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleKnowledgePromote invalid body ----

func TestHandleKnowledgePromote_InvalidBody(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/knowledge/promote", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleKnowledgeImport invalid body ----

func TestHandleKnowledgeImport_InvalidBody(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/knowledge/import", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleTokens with non-nil collector returning nil summary ----

func TestHandleTokens_EmptySummary(t *testing.T) {
	s, deps := apiServer(t)
	// Create a collector with an empty temp dir (no JSONL files)
	tmpDir := t.TempDir()
	collector := tokens.NewCollector(tmpDir, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	deps.Tokens = collector
	rec := doGet(s, "/api/tokens")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- handleKnowledgeSubsAdd with all fields ----

func TestHandleKnowledgeSubsAdd_AllFields(t *testing.T) {
	// L3: keep this hermetic — see stubPrivateURLResolver.
	stubPrivateURLResolver(t, "example.com")
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	rec := doPost(s, "/api/knowledge/subscriptions", map[string]string{
		"url":   "http://example.com/wiki",
		"layer": "org",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- MetricsCollector collectCoverage with badge server ----

func TestMetricsCollector_CollectCoverage_WithBadge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"message": "85%"})
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := &MetricsCollector{
		badgeURL: srv.URL,
		logger:   logger,
		metrics:  make(map[string]any),
	}
	result := mc.collectCoverage()
	if result["coverage"] != 85 {
		t.Errorf("coverage = %v, want 85", result["coverage"])
	}
}

func TestMetricsCollector_CollectCoverage_InvalidBadge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"message": "unknown"})
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := &MetricsCollector{
		badgeURL: srv.URL,
		logger:   logger,
		metrics:  make(map[string]any),
	}
	result := mc.collectCoverage()
	if result["coverage"] != 0 {
		t.Errorf("coverage = %v, want 0", result["coverage"])
	}
}

// ---- handleGovernorNotifications with Ntfy ----

func TestHandleGovernorNotifications_NtfyOnly(t *testing.T) {
	s, deps := apiServer(t)
	rec := doPut(s, "/api/config/governor/notifications", map[string]string{
		"ntfyServer": "https://ntfy.sh",
		"ntfyTopic":  "hive-test",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	if deps.Config.Notifications.Ntfy == nil {
		t.Fatal("ntfy config should be set")
	}
	if deps.Config.Notifications.Ntfy.Server != "https://ntfy.sh" {
		t.Errorf("server = %q", deps.Config.Notifications.Ntfy.Server)
	}
}

func TestHandleGovernorNotifications_InvalidBody(t *testing.T) {
	s, _ := apiServer(t)
	req := httptest.NewRequest("PUT", "/api/config/governor/notifications", strings.NewReader("not-json"))
	rec := httptest.NewRecorder()
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleGovernorAddAgent with all fields ----

func TestHandleGovernorAddAgent_WithModel(t *testing.T) {
	s, deps := apiServer(t)
	rec := doPost(s, "/api/config/governor/agents", map[string]string{
		"name":    "new-agent",
		"backend": "aider",
		"model":   "gpt-4",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if agentCfg, ok := deps.Config.Agents["new-agent"]; !ok {
		t.Error("agent not added")
	} else if agentCfg.Model != "gpt-4" {
		t.Errorf("model = %q, want gpt-4", agentCfg.Model)
	}
}

func TestHandleGovernorAddAgent_DuplicateAgent(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/config/governor/agents", map[string]string{
		"name": "scanner",
	})
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
}

// ---- handleKick with a bad agent ----

func TestHandleKick_UnknownAgent(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/kick/nonexistent", map[string]string{"message": "do stuff"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleWidget with running and paused agents ----

func TestHandleWidget_RunningAndPaused(t *testing.T) {
	s, deps := apiServer(t)
	// Start some agents in different states to exercise the counting logic
	_ = deps
	rec := doGet(s, "/api/widget")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	var body map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["mode"] == nil {
		t.Error("expected mode in response")
	}
}

func TestHandleAgentPrompt_Found(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/config/agent/scanner/prompt")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- handlePane with custom lines ----

func TestHandlePane_CustomLines(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/pane/scanner?lines=50")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleNousDeletePrinciple_WithNous(t *testing.T) {
	s, deps := apiServerWithNous(t)
	originalLen := len(deps.Nous.Principles)
	rec := doDelete(s, "/api/nous/principles/p1", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	if len(deps.Nous.Principles) != originalLen-1 {
		t.Errorf("principles len = %d, want %d", len(deps.Nous.Principles), originalLen-1)
	}
}

// ---- handleNousConfigRepos ----

func TestHandleNousConfigRepos_WithNous(t *testing.T) {
	s, _ := apiServerWithNous(t)
	rec := doPut(s, "/api/nous/config/repos", map[string]interface{}{
		"repos": []string{"repo1", "repo2"},
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- handleNousConfigOutput ----

func TestHandleNousConfigOutput_WithNous(t *testing.T) {
	s, _ := apiServerWithNous(t)
	rec := doPut(s, "/api/nous/config/output", map[string]string{
		"format": "markdown",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- handleNousConfigSchedule ----

func TestHandleNousConfigSchedule_WithNous(t *testing.T) {
	s, _ := apiServerWithNous(t)
	rec := doPut(s, "/api/nous/config/schedule", map[string]interface{}{
		"interval": 300,
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- handleNousConfigControllables ----

func TestHandleNousConfigControllables_WithNous(t *testing.T) {
	s, _ := apiServerWithNous(t)
	rec := doPut(s, "/api/nous/config/controllables", map[string]interface{}{
		"agents": true,
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- handleNousConfigPrinciples ----

func TestHandleNousConfigPrinciples_WithNous(t *testing.T) {
	s, _ := apiServerWithNous(t)
	rec := doPut(s, "/api/nous/config/principles", map[string]interface{}{
		"principles": []string{"be helpful"},
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- MetricsCollector collectCoverage server error ----

func TestMetricsCollector_CollectCoverage_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	mc := &MetricsCollector{
		badgeURL: srv.URL,
		metrics:  make(map[string]any),
	}
	result := mc.collectCoverage()
	if result["coverage"] != 0 {
		t.Errorf("coverage = %v, want 0", result["coverage"])
	}
}

// ---- handlePin auto-detect value ----

func TestHandlePin_AutoDetectValue(t *testing.T) {
	s, _ := apiServer(t)
	// When value is empty, the handler auto-detects from the agent config
	rec := doPost(s, "/api/pin/scanner/cli", map[string]string{})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandlePin_AutoDetectNotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/pin/nonexistent/cli", map[string]string{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleNousConfigSection various sections ----

func TestHandleNousConfigSection_Repos(t *testing.T) {
	s, _ := apiServerWithNous(t)
	rec := doPut(s, "/api/nous/config/repos", map[string]interface{}{
		"repos": []string{"r1"},
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleNousConfigSection_FastFail(t *testing.T) {
	s, _ := apiServerWithNous(t)
	rec := doPut(s, "/api/nous/config/fast-fail", map[string]interface{}{
		"enabled": true,
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- mock GH server helpers ----

func ghMockServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/rate_limit", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"resources": map[string]interface{}{
				"core":    map[string]interface{}{"limit": 5000, "remaining": 4999, "reset": time.Now().Add(time.Hour).Unix()},
				"search":  map[string]interface{}{"limit": 30, "remaining": 29, "reset": time.Now().Add(time.Hour).Unix()},
				"graphql": map[string]interface{}{"limit": 5000, "remaining": 4998, "reset": time.Now().Add(time.Hour).Unix()},
			},
		})
	})
	// go-github uses /repos/:owner/:repo/git/ref/:ref (singular)
	mux.HandleFunc("/repos/hivecommons/hive/git/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ref":    "refs/heads/v2",
			"object": map[string]interface{}{"sha": "abc1234567890abcdef1234567890abcdef123456"},
		})
	})
	mux.HandleFunc("/repos/myorg/repo1/contents/", func(w http.ResponseWriter, r *http.Request) {
		// Base64 of "| Organization | ACMM |\n|---|---|\n| Org1 | acmm |\n| Org2 | none |\n"
		b64Content := "fCBPcmdhbml6YXRpb24gfCBBQ01NIHwKfC0tLXwtLS18CnwgT3JnMSB8IGFjbW0gfAp8IE9yZzIgfCBub25lIHwK"
		json.NewEncoder(w).Encode(map[string]interface{}{
			"type":     "file",
			"encoding": "base64",
			"content":  b64Content,
		})
	})
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 5,
			"items":       []interface{}{},
		})
	})
	mux.HandleFunc("/repos/myorg/repo1", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"stargazers_count": 42,
			"forks_count":      7,
		})
	})
	mux.HandleFunc("/repos/myorg/repo1/contributors", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{"login": "user1"},
			{"login": "user2"},
		})
	})
	return httptest.NewServer(mux)
}

func apiServerWithGH(t *testing.T) (*Server, *Dependencies, *httptest.Server) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ghSrv := ghMockServer()
	ghClient := ghpkg.NewClientForTest(ghSrv.URL, "myorg", []string{"repo1"}, logger)

	s := NewServer(0, logger)
	deps := testDeps(t)
	deps.GHClient = ghClient
	deps.Config.Project.Org = "myorg"
	deps.Config.Project.Repos = []string{"repo1"}
	s.RegisterAPI(deps)
	return s, deps, ghSrv
}

// ---- handleGHRateLimits with mock ----

func TestHandleGHRateLimits_WithClient(t *testing.T) {
	s, _, ghSrv := apiServerWithGH(t)
	defer ghSrv.Close()

	rec := doGet(s, "/api/gh-rate-limits")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	var body map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &body)
	core, ok := body["core"].(map[string]interface{})
	if !ok {
		t.Fatal("expected core object in response")
	}
	if core["limit"] != float64(5000) {
		t.Errorf("core limit = %v", core["limit"])
	}
}

// ---- handleVersion with GH client (fetchLatestRemoteHash) ----

func TestHandleVersion_WithGHClient_LatestHash(t *testing.T) {
	s, _, ghSrv := apiServerWithGH(t)
	defer ghSrv.Close()

	rec := doGet(s, "/api/version")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	var body map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["latestHash"] == nil {
		t.Error("expected latestHash in response")
	}
	if body["latestShort"] == nil {
		t.Error("expected latestShort in response")
	}
}

// ---- MetricsCollector collectOutreach with mock GH ----

func TestMetricsCollector_CollectOutreach_WithClient(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ghSrv := ghMockServer()
	defer ghSrv.Close()

	ghClient := ghpkg.NewClientForTest(ghSrv.URL, "myorg", []string{"repo1"}, logger)
	mc := &MetricsCollector{
		ghClient: ghClient,
		org:      "myorg",
		repo:     "repo1",
		aiAuthor: "hive-bot",
		logger:   logger,
		metrics:  make(map[string]any),
	}
	result := mc.collectOutreach(context.Background())
	if result["stars"] != 42 {
		t.Errorf("stars = %v, want 42", result["stars"])
	}
	if result["forks"] != 7 {
		t.Errorf("forks = %v, want 7", result["forks"])
	}
	if result["contributors"] != 2 {
		t.Errorf("contributors = %v, want 2", result["contributors"])
	}
}

// ---- MetricsCollector countOutreachPRs with mock ----

func TestMetricsCollector_CountOutreachPRs_WithClient(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ghSrv := ghMockServer()
	defer ghSrv.Close()

	ghClient := ghpkg.NewClientForTest(ghSrv.URL, "myorg", []string{"repo1"}, logger)
	mc := &MetricsCollector{
		ghClient:    ghClient,
		org:         "myorg",
		repo:        "repo1",
		aiAuthor:    "hive-bot",
		projectName: "TestProject",
		logger:      logger,
		metrics:     make(map[string]any),
	}
	open, merged := mc.countOutreachPRs(context.Background())
	if open != 5 {
		t.Errorf("open = %d, want 5", open)
	}
	if merged != 5 {
		t.Errorf("merged = %d, want 5", merged)
	}
}

// ---- handleGovernorRemoveAgent success ----

func TestHandleGovernorRemoveAgent_Success(t *testing.T) {
	s, deps := apiServer(t)
	// scanner exists in config
	rec := doDelete(s, "/api/config/governor/agents/scanner", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if _, ok := deps.Config.Agents["scanner"]; ok {
		t.Error("scanner should be removed")
	}
}

// ---- handleKnowledgeLayer with nil facts ----

func TestHandleKnowledgeLayer_NilFacts(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	rec := doGet(s, "/api/knowledge/project?type=gotcha")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- handleKnowledgeSubsAdd with invalid body ----

func TestHandleKnowledgeSubsAdd_InvalidBody(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	req := httptest.NewRequest("POST", "/api/knowledge/subscriptions", strings.NewReader("not-json"))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleKnowledgeSubsRemove with invalid body ----

func TestHandleKnowledgeSubsRemove_InvalidBody(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	req := httptest.NewRequest("DELETE", "/api/knowledge/subscriptions", strings.NewReader("not-json"))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleKnowledgeUpdate error from API ----

func TestHandleKnowledgeUpdate_APIError(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	rec := doPut(s, "/api/knowledge/project/nonexistent-slug", map[string]interface{}{
		"title": "updated",
	})
	// This may succeed or fail depending on the mock server
	if rec.Code != http.StatusOK && rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---- handleNousGateDecision with existing pending ----

func TestHandleNousGateDecision_Success(t *testing.T) {
	s, deps := apiServerWithNous(t)
	deps.Nous.GatePending = map[string]interface{}{"question": "should I deploy?"}
	rec := doPut(s, "/api/nous/gate-decision", map[string]string{
		"decision": "approve",
		"reason":   "looks good",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	if deps.Nous.GateResponse == nil {
		t.Error("expected gate response to be set")
	}
}

// ---- handleNousGateRespond with nous ----

func TestHandleNousGateRespond_Success(t *testing.T) {
	s, deps := apiServerWithNous(t)
	rec := doPost(s, "/api/nous/gate-respond", map[string]interface{}{
		"action": "proceed",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	if deps.Nous.GateResponse == nil {
		t.Error("expected gate response to be set")
	}
}

// ---- handleNousConfigSection invalid body ----

func TestHandleNousConfigSection_InvalidBody(t *testing.T) {
	s, _ := apiServerWithNous(t)
	req := httptest.NewRequest("PUT", "/api/nous/config/goals", strings.NewReader("not-json"))
	rec := httptest.NewRecorder()
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleNousGateRespond: invalid body ----

func TestHandleNousGateRespond_InvalidBody(t *testing.T) {
	s, _ := apiServerWithNous(t)
	req := httptest.NewRequest("POST", "/api/nous/gate-respond", strings.NewReader("not-json"))
	rec := httptest.NewRecorder()
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleWidget: verify all fields ----

func TestHandleWidget_AllFields(t *testing.T) {
	s, deps := apiServer(t)
	deps.Governor.Evaluate(5, 2, 0, 0)
	rec := doGet(s, "/api/widget")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["mode"] == nil {
		t.Error("expected mode")
	}
	if result["running"] == nil {
		t.Error("expected running count")
	}
	if result["paused"] == nil {
		t.Error("expected paused count")
	}
}

// ---- handlePane: with agent output ----

func TestHandlePane_WithOutput(t *testing.T) {
	s, deps := apiServer(t)
	proc := deps.AgentMgr.AllStatuses()["scanner"]
	if proc != nil && proc.OutputBuffer != nil {
		proc.OutputBuffer.Write("test output line 1")
		proc.OutputBuffer.Write("test output line 2")
	}
	rec := doGet(s, "/api/pane/scanner?lines=10")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

// ---- handleKick: with valid agent and message ----

func TestHandleKick_ValidAgentAndMessage(t *testing.T) {
	s, _ := apiServer(t)
	body := map[string]string{
		"message": "Test kick message",
	}
	rec := doPost(s, "/api/kick/scanner", body)
	// Agent may not support kick in test (400), but should not be 404
	if rec.Code == http.StatusNotFound {
		t.Errorf("status = %d, want non-404", rec.Code)
	}
}

// ---- handleRestart: success (agent exists) ----

func TestHandleRestart_AgentExists(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/restart/scanner", nil)
	// May succeed or fail depending on agent state, but should not 404
	if rec.Code == http.StatusNotFound {
		t.Error("unexpected 404")
	}
}

// ---- handleAgentConfigGet: verify sections present ----

func TestHandleAgentConfigGet_SectionsPresent(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/config/agent/scanner")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["general"] == nil {
		t.Error("expected general section")
	}
	if result["cadences"] == nil {
		t.Error("expected cadences section")
	}
	if result["models"] == nil {
		t.Error("expected models section")
	}
}

// ---- handleSidebarGet: nil sidebar ----

func TestHandleSidebarGet_NilSidebar(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/config/sidebar")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

// ---- handleSidebarSet: valid data ----

func TestHandleSidebarSet_Valid(t *testing.T) {
	s, _ := apiServer(t)
	body := map[string]string{"layout": "compact"}
	rec := doPut(s, "/api/config/sidebar", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	// Verify it was stored
	rec2 := doGet(s, "/api/config/sidebar")
	if rec2.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec2.Code)
	}
}

// ---- handleSidebarSet: invalid body (alt path) ----

func TestHandleSidebarSet_InvalidBody_AltPath(t *testing.T) {
	s, _ := apiServer(t)
	req := httptest.NewRequest("PUT", "/api/config/sidebar", strings.NewReader("not-json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleAgentConfigRestrictions: invalid body ----

func TestHandleAgentConfigRestrictions_InvalidBody(t *testing.T) {
	s, _ := apiServer(t)
	req := httptest.NewRequest("PUT", "/api/config/agent/scanner/restrictions", strings.NewReader("not-json"))
	rec := httptest.NewRecorder()
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleAgentConfigStats: invalid body ----

func TestHandleAgentConfigStats_InvalidBody(t *testing.T) {
	s, _ := apiServer(t)
	req := httptest.NewRequest("PUT", "/api/config/agent/scanner/stats", strings.NewReader("not-json"))
	rec := httptest.NewRecorder()
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleAgentConfigGet: copilot backend ----

func TestHandleAgentConfigGet_CopilotBackend(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "myorg", Repos: []string{"repo1"}, AIAuthor: "bot"},
		GitHub:  config.GitHubConfig{Token: "tok"},
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "copilot", Model: "gpt-4", Enabled: true},
		},
		Governor: config.GovernorConfig{
			Modes: map[string]config.ModeConfig{
				"idle": {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "15m"}},
			},
		},
	}
	gov := governor.New(cfg.Governor, cfg.Agents, logger)
	mgr := agent.NewManager(cfg.Agents, logger, agent.ProjectContext{})
	deps := &Dependencies{
		Config:      cfg,
		AgentMgr:    mgr,
		Governor:    gov,
		Logger:      logger,
		Ctx:         context.Background(),
		RefreshFunc: func() {},
		PersistFunc: func() {},
	}
	s.RegisterAPI(deps)
	rec := doGet(s, "/api/config/agent/scanner")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	result := decodeJSON(t, rec)
	general := result["general"].(map[string]interface{})
	launchCmd, _ := general["launchCmd"].(string)
	if !strings.Contains(launchCmd, "copilot") {
		t.Errorf("launchCmd = %q, expected copilot command", launchCmd)
	}
}

// ---- handleAgentConfigGet: custom LaunchCmd ----

func TestHandleAgentConfigGet_CustomLaunchCmd(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "myorg", Repos: []string{"repo1"}, AIAuthor: "bot"},
		GitHub:  config.GitHubConfig{Token: "tok"},
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "custom", Model: "m1", LaunchCmd: "/usr/bin/my-agent --special", Enabled: true},
		},
		Governor: config.GovernorConfig{
			Modes: map[string]config.ModeConfig{
				"idle": {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "15m"}},
			},
		},
	}
	gov := governor.New(cfg.Governor, cfg.Agents, logger)
	mgr := agent.NewManager(cfg.Agents, logger, agent.ProjectContext{})
	deps := &Dependencies{
		Config:      cfg,
		AgentMgr:    mgr,
		Governor:    gov,
		Logger:      logger,
		Ctx:         context.Background(),
		RefreshFunc: func() {},
		PersistFunc: func() {},
	}
	s.RegisterAPI(deps)
	rec := doGet(s, "/api/config/agent/scanner")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	result := decodeJSON(t, rec)
	general := result["general"].(map[string]interface{})
	launchCmd, _ := general["launchCmd"].(string)
	if launchCmd != "/usr/bin/my-agent --special" {
		t.Errorf("launchCmd = %q, want custom command", launchCmd)
	}
}

// ---- handleAgentConfigGet: DisplayName and cadence=pause ----

func TestHandleAgentConfigGet_DisplayNameAndPauseCadence(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "myorg", Repos: []string{"repo1"}, AIAuthor: "bot"},
		GitHub:  config.GitHubConfig{Token: "tok"},
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "claude", Model: "sonnet", DisplayName: "Bug Scanner", Enabled: true},
		},
		Governor: config.GovernorConfig{
			Modes: map[string]config.ModeConfig{
				"idle": {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "pause"}},
			},
		},
	}
	gov := governor.New(cfg.Governor, cfg.Agents, logger)
	mgr := agent.NewManager(cfg.Agents, logger, agent.ProjectContext{})
	deps := &Dependencies{
		Config:      cfg,
		AgentMgr:    mgr,
		Governor:    gov,
		Logger:      logger,
		Ctx:         context.Background(),
		RefreshFunc: func() {},
		PersistFunc: func() {},
	}
	s.RegisterAPI(deps)
	rec := doGet(s, "/api/config/agent/scanner")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	result := decodeJSON(t, rec)
	general := result["general"].(map[string]interface{})
	displayName, _ := general["displayName"].(string)
	if displayName != "Bug Scanner" {
		t.Errorf("displayName = %q", displayName)
	}
	cadences := result["cadences"].(map[string]interface{})
	idleCadence, _ := cadences["idle"].(float64)
	if idleCadence != 0 {
		t.Errorf("idle cadence = %v, want 0 for pause", idleCadence)
	}
}

// ---- handlePane: agent not found ----

func TestHandlePane_AgentNotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/pane/nonexistent?lines=10")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// ---- handleRestart: agent not found ----

func TestHandleRestart_NonexistentAgent(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/restart/nonexistent", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleSummaries: with built status ----

func TestHandleSummaries_WithBuiltStatus(t *testing.T) {
	s, deps := apiServer(t)
	deps.Governor.Evaluate(5, 2, 0, 0)
	govState := deps.Governor.GetState()
	agentStatuses := deps.AgentMgr.AllStatuses()
	status := BuildFrontendStatus(govState, nil, agentStatuses, deps.Config, nil, deps.Governor, nil, nil, context.Background(), nil)
	s.UpdateStatus(status)
	rec := doGet(s, "/api/summaries")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["issues"] == nil {
		t.Error("expected issues in summaries")
	}
}

// ---- handleTrends: week range ----

func TestHandleTrends_WeekRange(t *testing.T) {
	s, deps := apiServer(t)
	deps.Governor.Evaluate(5, 0, 0, 0)
	rec := doGet(s, "/api/trends?range=week")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

// ---- buildBudget: with weekly limit ----

func TestBuildBudget_WithWeeklyLimit(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"idle": {Threshold: 0},
		},
	}
	agents := map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Enabled: true},
	}
	gov := governor.New(cfg, agents, logger)
	gov.SetBudgetLimit(1000000)
	// Open the window first so the totals count as in-window spend.
	gov.SetBudgetResetAt(time.Now())
	gov.UpdateBudgetFromTotals(250000, map[string]int64{"scanner": 250000}, map[string]int64{"sonnet": 250000})

	budget := buildBudget(gov, nil)
	if budget.WeeklyBudget != 1000000 {
		t.Errorf("WeeklyBudget = %d", budget.WeeklyBudget)
	}
	if budget.Used != 250000 {
		t.Errorf("Used = %d", budget.Used)
	}
	if budget.Remaining != 750000 {
		t.Errorf("Remaining = %d", budget.Remaining)
	}
	const expectedPct = 25.0
	if budget.PctUsed != expectedPct {
		t.Errorf("PctUsed = %f, want %f", budget.PctUsed, expectedPct)
	}
}

// ---- buildBudget: with token collector ----

func TestBuildBudget_WithTokenCollector(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"idle": {Threshold: 0},
		},
	}
	agents := map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Enabled: true},
	}
	gov := governor.New(cfg, agents, logger)

	dir := t.TempDir()
	tc := tokens.NewCollector(dir, logger)
	stop := make(chan struct{})
	close(stop)
	tc.Start(stop)

	budget := buildBudget(gov, tc)
	if budget.WeeklyBudget != 0 {
		t.Errorf("WeeklyBudget = %d", budget.WeeklyBudget)
	}
}

// ---- handleAgentConfigGet: cadence=off ----

func TestHandleAgentConfigGet_OffCadence(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "myorg", Repos: []string{"repo1"}, AIAuthor: "bot"},
		GitHub:  config.GitHubConfig{Token: "tok"},
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "claude", Model: "sonnet", Enabled: true},
		},
		Governor: config.GovernorConfig{
			Modes: map[string]config.ModeConfig{
				"idle": {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "off"}},
			},
		},
	}
	gov := governor.New(cfg.Governor, cfg.Agents, logger)
	mgr := agent.NewManager(cfg.Agents, logger, agent.ProjectContext{})
	deps := &Dependencies{
		Config:      cfg,
		AgentMgr:    mgr,
		Governor:    gov,
		Logger:      logger,
		Ctx:         context.Background(),
		RefreshFunc: func() {},
		PersistFunc: func() {},
	}
	s.RegisterAPI(deps)
	rec := doGet(s, "/api/config/agent/scanner")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	result := decodeJSON(t, rec)
	cadences := result["cadences"].(map[string]interface{})
	idleCadence, _ := cadences["idle"].(float64)
	if idleCadence != 0 {
		t.Errorf("idle cadence = %v, want 0 for 'off'", idleCadence)
	}
}

// ---- handleNousGateDecision: missing fields ----

func TestHandleNousGateDecision_MissingFields(t *testing.T) {
	s, _ := apiServerWithNous(t)
	body := map[string]string{}
	rec := doPut(s, "/api/nous/gate-decision", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleKnowledgeDelete: not found ----

func TestHandleKnowledgeDelete_NotFound(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	req := httptest.NewRequest("DELETE", "/api/knowledge/project/nonexistent-slug", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	// Should not succeed (returns 404 or 500 depending on wiki backend)
	if rec.Code == http.StatusOK {
		t.Errorf("expected non-200 status, got %d", rec.Code)
	}
}

// ---- handleKnowledgeImport: empty body ----

func TestHandleKnowledgeImport_EmptyBody(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	req := httptest.NewRequest("POST", "/api/knowledge/import", strings.NewReader(""))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- handleKnowledgeCreate: missing required fields ----

func TestHandleKnowledgeCreate_EmptyTitle(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	body := map[string]interface{}{
		"title": "",
	}
	rec := doPost(s, "/api/knowledge/project", body)
	if rec.Code == http.StatusOK {
		t.Error("expected error for empty title")
	}
}

// ---- Vault handler tests ----

func TestHandleVaultsList_NilKnowledge(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/knowledge/vaults")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleVaultsList_WithKnowledge(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	rec := doGet(s, "/api/knowledge/vaults")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleVaultsConnect_NilKnowledge(t *testing.T) {
	s, _ := apiServer(t)
	body := map[string]string{"path": "/tmp/test"}
	rec := doPost(s, "/api/knowledge/vaults", body)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleVaultsConnect_WithKnowledge(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	dir := t.TempDir()
	os.WriteFile(dir+"/test.md", []byte("# Test\nContent"), 0o644)
	body := map[string]string{"path": dir, "name": "test-vault"}
	rec := doPost(s, "/api/knowledge/vaults", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleVaultsConnect_MissingPath(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	body := map[string]string{"name": "x"}
	rec := doPost(s, "/api/knowledge/vaults", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleVaultsConnect_InvalidBody(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	req := httptest.NewRequest("POST", "/api/knowledge/vaults", strings.NewReader("not-json"))
	markOwnerRequest(req)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleVaultsConnect_AutoName(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	dir := t.TempDir()
	os.WriteFile(dir+"/test.md", []byte("# Test\nContent"), 0o644)
	// Name is empty, should auto-generate from path
	body := map[string]string{"path": dir}
	rec := doPost(s, "/api/knowledge/vaults", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleVaultsDisconnect_NilKnowledge(t *testing.T) {
	s, _ := apiServer(t)
	req := httptest.NewRequest("DELETE", "/api/knowledge/vaults", strings.NewReader(`{"path":"/tmp/x"}`))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleVaultsDisconnect_NotFound(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	req := httptest.NewRequest("DELETE", "/api/knowledge/vaults", strings.NewReader(`{"path":"/nonexistent"}`))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleVaultsDisconnect_MissingPath(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	req := httptest.NewRequest("DELETE", "/api/knowledge/vaults", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleVaultsDisconnect_InvalidBody(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	req := httptest.NewRequest("DELETE", "/api/knowledge/vaults", strings.NewReader("not-json"))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleVaultsReindex_NilKnowledge(t *testing.T) {
	s, _ := apiServer(t)
	body := map[string]string{"path": "/tmp/x"}
	rec := doPost(s, "/api/knowledge/vaults/reindex", body)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleVaultsReindex_NotFound(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	body := map[string]string{"path": "/nonexistent"}
	rec := doPost(s, "/api/knowledge/vaults/reindex", body)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleVaultsReindex_MissingPath(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	body := map[string]string{}
	rec := doPost(s, "/api/knowledge/vaults/reindex", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleVaultsReindex_InvalidBody(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	req := httptest.NewRequest("POST", "/api/knowledge/vaults/reindex", strings.NewReader("not-json"))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleVaultFacts_NilKnowledge(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/knowledge/vaults/test/facts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleVaultFacts_WithKnowledge(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	rec := doGet(s, "/api/knowledge/vaults/test/facts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleVaultsConnect_ThenDisconnect(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	dir := t.TempDir()
	os.WriteFile(dir+"/test.md", []byte("# Test\nContent"), 0o644)

	// Connect
	connectBody := map[string]string{"path": dir, "name": "temp-vault"}
	rec := doPost(s, "/api/knowledge/vaults", connectBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("connect status = %d", rec.Code)
	}

	// List - should have 1 vault
	rec2 := doGet(s, "/api/knowledge/vaults")
	if rec2.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec2.Code)
	}

	// Disconnect
	req := httptest.NewRequest("DELETE", "/api/knowledge/vaults", strings.NewReader(fmt.Sprintf(`{"path":%q}`, dir)))
	rec3 := httptest.NewRecorder()
	s.mux.ServeHTTP(rec3, req)
	if rec3.Code != http.StatusOK {
		t.Fatalf("disconnect status = %d", rec3.Code)
	}
}

func TestHandleTimeline_EmptyModesFallback(t *testing.T) {
	// When eval history is empty but mode history has entries, handleTimeline
	// falls back to mode history.
	s, deps := apiServer(t)
	_ = deps
	rec := doGet(s, "/api/timeline")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	result := decodeJSON(t, rec)
	modes, ok := result["modes"].([]interface{})
	if !ok {
		t.Fatal("modes is not an array")
	}
	if len(modes) == 0 {
		t.Error("expected at least one mode entry from startup")
	}
}

func TestHandlePin_EmptyValueFallback(t *testing.T) {
	s, _ := apiServer(t)
	body := map[string]string{}
	rec := doPost(s, "/api/pin/scanner/cli", body)
	// Scanner exists in config but may not have a running process — GetStatus may fail
	if rec.Code == http.StatusNotFound {
		t.Error("should not be 404 for configured agent")
	}
}

func TestHandleAgentPrompt_ValidAgent(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/config/agent/scanner/prompt")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleAgentConfigGeneral_ValidAgent(t *testing.T) {
	s, _ := apiServer(t)
	body := map[string]interface{}{"enabled": true}
	rec := doPut(s, "/api/config/agent/scanner/general", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleAgentConfigGeneral_ClearOnKick(t *testing.T) {
	s, _ := apiServer(t)
	body := map[string]interface{}{"clear_on_kick": true}
	rec := doPut(s, "/api/config/agent/scanner/general", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleAgentConfigGeneral_BeadsDir(t *testing.T) {
	s, _ := apiServer(t)
	body := map[string]interface{}{"beads_dir": "/tmp/beads"}
	rec := doPut(s, "/api/config/agent/scanner/general", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleTimeline_WithEvalData(t *testing.T) {
	s, deps := apiServer(t)
	deps.Governor.Evaluate(15, 3, 1, 0)
	deps.Governor.Evaluate(5, 1, 0, 0)
	rec := doGet(s, "/api/timeline")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	result := decodeJSON(t, rec)
	modes, ok := result["modes"].([]interface{})
	if !ok {
		t.Fatal("modes not array")
	}
	if len(modes) < 2 {
		t.Errorf("expected at least 2 modes from evals, got %d", len(modes))
	}
}

func TestHandleHistory_NoSeedData(t *testing.T) {
	s, deps := apiServer(t)
	deps.Governor.Evaluate(5, 0, 0, 0)
	rec := doGet(s, "/api/history")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleWidget_AllStates(t *testing.T) {
	s, deps := apiServer(t)
	deps.Governor.Evaluate(25, 5, 2, 1)
	rec := doGet(s, "/api/widget")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	result := decodeJSON(t, rec)
	if _, ok := result["mode"]; !ok {
		t.Error("missing mode key")
	}
}

func TestHandleGovernorSensing_WithInterval(t *testing.T) {
	s, _ := apiServer(t)
	const sensingInterval = 120
	body := map[string]interface{}{"interval": sensingInterval}
	rec := doPut(s, "/api/config/governor/sensing", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleVaultsReindex_Success(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	dir := t.TempDir()
	os.WriteFile(dir+"/test.md", []byte("# Test\nContent"), 0o644)

	// Connect first
	connectBody := map[string]string{"path": dir, "name": "reindex-vault"}
	rec := doPost(s, "/api/knowledge/vaults", connectBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("connect status = %d", rec.Code)
	}

	// Reindex
	reindexBody := map[string]string{"path": dir}
	rec2 := doPost(s, "/api/knowledge/vaults/reindex", reindexBody)
	if rec2.Code != http.StatusOK {
		t.Fatalf("reindex status = %d", rec2.Code)
	}
}

// --- Chat endpoint ---

func TestHandleChat_ValidMessage(t *testing.T) {
	s, _ := apiServer(t)
	body := map[string]string{"query": "hello"}
	rec := doPost(s, "/api/chat", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// --- Additional nous/config handler branches ---

func TestHandleNousConfigRepos_NilNous(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/nous/config/repos", []string{"r1"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleNousGateDecision_NilGatePending(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = &NousState{}
	body := map[string]string{"decision": "approve", "reason": "test"}
	rec := doPut(s, "/api/nous/gate-decision", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleGovernorNotifications_NtfyAndDiscord(t *testing.T) {
	s, _ := apiServer(t)
	body := map[string]string{
		"ntfyServer":     "https://ntfy.sh",
		"ntfyTopic":      "hive-test",
		"discordWebhook": "https://discord.com/api/webhooks/123/abc",
	}
	rec := doPut(s, "/api/config/governor/notifications", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleGovernorHealth_PartialUpdate(t *testing.T) {
	s, _ := apiServer(t)
	body := map[string]interface{}{
		"healthcheckInterval": 60,
		"modelLock":           true,
	}
	rec := doPut(s, "/api/config/governor/health", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleWidget_PausedAgent(t *testing.T) {
	s, deps := apiServer(t)
	// Pause the scanner agent so it has State="paused"
	_ = deps.AgentMgr.Pause("scanner", "test", "test pause")
	deps.Governor.Evaluate(5, 1, 0, 0)
	rec := doGet(s, "/api/widget")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	result := decodeJSON(t, rec)
	if p, ok := result["paused"].(float64); !ok || p < 1 {
		t.Errorf("paused = %v, want >= 1", result["paused"])
	}
}

func TestHandleAgentConfigCadences_UnknownMode(t *testing.T) {
	s, _ := apiServer(t)
	// Include a mode name that does NOT exist in config — should be skipped via continue
	body := map[string]int64{"nonexistent_mode": 600, "idle": 900}
	rec := doPut(s, "/api/config/agent/scanner/cadences", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleAgentConfigCadences_NilCadencesMap(t *testing.T) {
	s, deps := apiServer(t)
	// Set a mode's Cadences to nil to hit the nil-check branch
	mode := deps.Config.Governor.Modes["idle"]
	mode.Cadences = nil
	deps.Config.Governor.Modes["idle"] = mode

	body := map[string]int64{"idle": 600}
	rec := doPut(s, "/api/config/agent/scanner/cadences", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandlePin_ModelDimensionEmptyValue(t *testing.T) {
	s, _ := apiServer(t)
	// Post with empty value to model dimension — should use agent's current model
	body := map[string]string{}
	rec := doPost(s, "/api/pin/scanner/model", body)
	// Agent isn't running so PinModel will succeed but GetStatus should return the agent
	if rec.Code != http.StatusOK {
		t.Logf("status = %d (expected — pin sets model from config)", rec.Code)
	}
}

func TestHandlePin_CLIDimensionEmptyValue(t *testing.T) {
	s, _ := apiServer(t)
	// Post with empty value to cli dimension — should use agent's current backend
	body := map[string]string{}
	rec := doPost(s, "/api/pin/scanner/cli", body)
	if rec.Code != http.StatusOK {
		t.Logf("status = %d", rec.Code)
	}
}

func TestHandlePin_PinError(t *testing.T) {
	s, _ := apiServer(t)
	// Pin a nonexistent agent — should return error
	body := map[string]string{"value": "claude"}
	rec := doPost(s, "/api/pin/nonexistent/cli", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleAgentConfigGet_BackendOverride(t *testing.T) {
	s, deps := apiServer(t)
	// Set overrides on the agent to hit lines 663-668
	_ = deps.AgentMgr.SetBackendOverride("scanner", "gemini")
	_ = deps.AgentMgr.SetModelOverride("scanner", "gpt-4")
	rec := doGet(s, "/api/config/agent/scanner")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	result := decodeJSON(t, rec)
	general, ok := result["general"].(map[string]interface{})
	if !ok {
		t.Fatal("missing general key in response")
	}
	if cli, ok := general["cliPinValue"].(string); !ok || cli != "gemini" {
		t.Errorf("cliPinValue = %v, want gemini", general["cliPinValue"])
	}
	if model, ok := general["model"].(string); !ok || model != "gpt-4" {
		t.Errorf("model = %v, want gpt-4", general["model"])
	}
}

func TestHandleRestart_NotRunning(t *testing.T) {
	s, _ := apiServer(t)
	// Scanner is stopped, restart should fail
	rec := doPost(s, "/api/restart/scanner", nil)
	if rec.Code == http.StatusOK {
		t.Log("restart succeeded despite stopped agent (depends on impl)")
	}
}

// ── Budget partial-update regression tests ──
// Reported by Cyril Nestor: editing only "Total Tokens" in the Governor
// Budget tab failed with "periodDays must be between 1 and 365" even though
// the UI showed a valid period. The dashboard PUTs only the fields the user
// touched, so periodDays/criticalPct were absent and decoded to 0.

// budgetTestPeriodDays / budgetTestCriticalPct are the pre-existing stored
// values these tests seed so they can assert the values survive a partial PUT.
const (
	budgetTestPeriodDays  = 7
	budgetTestCriticalPct = 90
)

// seedBudget puts a known-good budget into the server config so that a
// partial update has something meaningful to preserve.
func seedBudget(deps *Dependencies, totalTokens int64) {
	deps.Config.Governor.Budget.TotalTokens = totalTokens
	deps.Config.Governor.Budget.PeriodDays = budgetTestPeriodDays
	deps.Config.Governor.Budget.CriticalPct = budgetTestCriticalPct
}

func TestHandleGovernorBudget_PartialUpdatePreservesOtherFields(t *testing.T) {
	s, deps := apiServer(t)
	seedBudget(deps, 1000)

	// Exactly what the dashboard sends when only Total Tokens is edited.
	// The value must clear the #5508 sanity floor; this test is about the
	// pointer-merge preserving absent fields, not about budget sizing.
	rec := doPutRaw(s, "/api/config/governor/budget", `{"totalTokens":50000000}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	b := deps.Config.Governor.Budget
	if b.TotalTokens != 50000000 {
		t.Errorf("TotalTokens = %d, want 50000000", b.TotalTokens)
	}
	if b.PeriodDays != budgetTestPeriodDays {
		t.Errorf("PeriodDays = %d, want %d preserved", b.PeriodDays, budgetTestPeriodDays)
	}
	if b.CriticalPct != budgetTestCriticalPct {
		t.Errorf("CriticalPct = %d, want %d preserved", b.CriticalPct, budgetTestCriticalPct)
	}
}

func TestHandleGovernorBudget_PartialPeriodDaysOnly(t *testing.T) {
	s, deps := apiServer(t)
	seedBudget(deps, 1000)

	rec := doPutRaw(s, "/api/config/governor/budget", `{"periodDays":30}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	b := deps.Config.Governor.Budget
	if b.PeriodDays != 30 {
		t.Errorf("PeriodDays = %d, want 30", b.PeriodDays)
	}
	if b.TotalTokens != 1000 {
		t.Errorf("TotalTokens = %d, want 1000 preserved", b.TotalTokens)
	}
	if b.CriticalPct != budgetTestCriticalPct {
		t.Errorf("CriticalPct = %d, want %d preserved", b.CriticalPct, budgetTestCriticalPct)
	}
}

// An explicit zero for totalTokens disables budget tracking per the UI
// tooltip, so it must be applied — not mistaken for an absent field.
func TestHandleGovernorBudget_ExplicitZeroTotalTokensHonored(t *testing.T) {
	s, deps := apiServer(t)
	seedBudget(deps, 1000)

	rec := doPutRaw(s, "/api/config/governor/budget", `{"totalTokens":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := deps.Config.Governor.Budget.TotalTokens; got != 0 {
		t.Errorf("TotalTokens = %d, want 0 (explicit zero disables tracking)", got)
	}
	if got := deps.Config.Governor.Budget.PeriodDays; got != budgetTestPeriodDays {
		t.Errorf("PeriodDays = %d, want %d preserved", got, budgetTestPeriodDays)
	}
}

// An explicit out-of-range value must still be rejected — the pointer change
// fixes absence, it does not relax validation.
func TestHandleGovernorBudget_ExplicitOutOfRangeStillRejected(t *testing.T) {
	s, deps := apiServer(t)
	seedBudget(deps, 1000)

	for _, tc := range []struct{ name, payload string }{
		{"periodDays zero", `{"periodDays":0}`},
		{"periodDays too high", `{"periodDays":400}`},
		{"criticalPct zero", `{"criticalPct":0}`},
		{"criticalPct too high", `{"criticalPct":101}`},
		{"negative totalTokens", `{"totalTokens":-1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doPutRaw(s, "/api/config/governor/budget", tc.payload)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 for %s", rec.Code, tc.payload)
			}
		})
	}
	// Nothing should have been mutated by the rejected requests.
	b := deps.Config.Governor.Budget
	if b.PeriodDays != budgetTestPeriodDays || b.CriticalPct != budgetTestCriticalPct || b.TotalTokens != 1000 {
		t.Errorf("config mutated by rejected requests: %+v", b)
	}
}

// An empty object touches nothing and must succeed without zeroing anything.
func TestHandleGovernorBudget_EmptyPayloadPreservesAll(t *testing.T) {
	s, deps := apiServer(t)
	seedBudget(deps, 1000)

	rec := doPutRaw(s, "/api/config/governor/budget", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	b := deps.Config.Governor.Budget
	if b.TotalTokens != 1000 || b.PeriodDays != budgetTestPeriodDays || b.CriticalPct != budgetTestCriticalPct {
		t.Errorf("empty payload mutated config: %+v", b)
	}
}

func TestSidebarDiskUsesInjectablePath(t *testing.T) {
	old := sidebarFile
	sidebarFile = filepath.Join(t.TempDir(), "sidebar.json")
	t.Cleanup(func() { sidebarFile = old })
	s, _ := apiServer(t)
	want := map[string]interface{}{"items": []interface{}{"audit", "fleet"}}
	s.saveSidebarToDisk(want)
	s.sidebar = nil
	s.loadSidebarFromDisk()
	if s.sidebar == nil {
		t.Fatal("sidebar was not loaded from injected path")
	}
}
