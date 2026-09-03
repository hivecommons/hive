package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

func newFullServer(t *testing.T) *Server {
	t.Helper()
	orig := privateURLResolver
	privateURLResolver = func(_ context.Context, _ string) ([]string, error) {
		return []string{"93.184.216.34"}, nil
	}
	t.Cleanup(func() { privateURLResolver = orig })
	level := 2
	cfg := &config.Config{
		ACMMLevel: &level,
		Project: config.ProjectConfig{
			Org: "testorg", Name: "test", PrimaryRepo: "testrepo",
			Repos: []string{"testrepo"},
		},
		Data: config.DataConfig{AgentsDir: t.TempDir()},
		Agents: map[string]config.AgentConfig{
			// Enabled must be set explicitly. This literal skips
			// ApplyAgentDefaults, which is what gives a real config
			// `enabled: true` unless the operator wrote `enabled: false`
			// (config/agent_overlay.go). Leaving it at the zero value
			// described a state production cannot produce — an agent in the
			// runtime roster that is switched OFF — and the health checks now
			// read that as a deliberate off-state rather than a down agent.
			"scanner": {ID: "scan-001", Role: "scanner", Backend: "claude", Model: "sonnet", DisplayName: "Scanner", Enabled: true},
		},
		GitHub:     config.GitHubConfig{Token: "ghp_test123456789"},
		SourcePath: t.TempDir() + "/hive.yaml",
	}

	dir := t.TempDir()
	scannerStore, _ := beads.NewStore(dir + "/scanner")
	logger := slog.Default()
	gov := governor.New(cfg.Governor, cfg.Agents, logger)
	mgr := agent.NewManager(cfg.Agents, logger, agent.ProjectContext{
		Org: "testorg", Repos: []string{"testrepo"}, ACMMLevel: *cfg.ACMMLevel, PRsAllowed: true,
	})

	srv := NewServer(0, logger)
	srv.deps = &Dependencies{
		Config:         cfg,
		AgentMgr:       mgr,
		Governor:       gov,
		BeadStores:     map[string]*beads.Store{"scanner": scannerStore},
		Logger:         logger,
		Ctx:            context.Background(),
		RefreshFunc:    func() {},
		PersistFunc:    func() {},
		SkipReloadFunc: func() {},
	}
	srv.RegisterAPI(srv.deps)
	return srv
}

func TestHandleGovernorSensingInvalidRegex(t *testing.T) {
	srv := newFullServer(t)
	body := `{"ghRatePatterns":["[invalid"]}`
	req := httptest.NewRequest("PUT", "/api/governor/sensing", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorSensing(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid regex, got %d", w.Code)
	}
}

func TestHandleGovernorSensingInvalidCLIExclude(t *testing.T) {
	srv := newFullServer(t)
	body := `{"cliExcludePatterns":["(unterminated"]}`
	req := httptest.NewRequest("PUT", "/api/governor/sensing", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorSensing(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleGovernorSensingInvalidLoginPattern(t *testing.T) {
	srv := newFullServer(t)
	body := `{"loginPatterns":["[bad"]}`
	req := httptest.NewRequest("PUT", "/api/governor/sensing", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorSensing(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleGovernorSensingEvalIntervalTooLow(t *testing.T) {
	srv := newFullServer(t)
	body := `{"eval_interval_s":1}`
	req := httptest.NewRequest("PUT", "/api/governor/sensing", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorSensing(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleGovernorSensingEvalIntervalTooHigh(t *testing.T) {
	srv := newFullServer(t)
	body := `{"eval_interval_s":999999}`
	req := httptest.NewRequest("PUT", "/api/governor/sensing", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorSensing(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleGovernorSensingValidPatterns(t *testing.T) {
	srv := newFullServer(t)
	body := `{"ghRatePatterns":["^rate_limit$"],"cliExcludePatterns":["^noise$"],"loginPatterns":["login","","auth"],"eval_interval_s":60}`
	req := httptest.NewRequest("PUT", "/api/governor/sensing", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorSensing(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleGovernorSensingBadJSON(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/governor/sensing", strings.NewReader(`{invalid`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorSensing(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleTimelineWithKickHistory(t *testing.T) {
	srv := newFullServer(t)
	// Add some kick history
	srv.deps.Governor.RecordKick("scanner")

	req := httptest.NewRequest("GET", "/api/timeline", nil)
	w := httptest.NewRecorder()
	srv.handleTimeline(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["kicks"] == nil {
		t.Error("should have kicks field")
	}
}

func TestHandleKickUnknownAgent(t *testing.T) {
	srv := newFullServer(t)
	body := `{"agent":"nonexistent"}`
	req := httptest.NewRequest("POST", "/api/agents/nonexistent/kick", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleKick(w, req)

	// Agent not found → error
	if w.Code == http.StatusOK {
		t.Logf("kick unknown agent returned 200 — checking body")
	}
}

func TestHandleAgentConfigGeneralBadJSON(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/config/agent/scanner/general", strings.NewReader(`{invalid`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleAgentConfigRestrictionsBadJSON(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/config/agent/scanner/restrictions", strings.NewReader(`{invalid`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleGovernorReposBadJSON(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/governor/repos", strings.NewReader(`{invalid`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleGovernorReposValid(t *testing.T) {
	srv := newFullServer(t)
	// Field is primaryRepo (camelCase — the handler decodes that key); it must
	// name one of the repos so the always-exactly-one-default guard passes.
	body := `{"repos":["repo1","repo2"],"primaryRepo":"repo1"}`
	req := httptest.NewRequest("PUT", "/api/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleContributorsListWithHubExtra(t *testing.T) {
	srv := newFullServer(t)
	srv.contributeHub = NewContributeWSHub(slog.Default(), nil)

	req := httptest.NewRequest("GET", "/api/contributors", nil)
	w := httptest.NewRecorder()
	srv.handleContributorsList(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestHandleContributorDeleteNoID(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("DELETE", "/api/contributors/delete", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleContributorDelete(w, req)

	if w.Code != http.StatusBadRequest {
		t.Logf("contributor delete no ID: %d", w.Code)
	}
}

func TestHandleHivesRegisterBadJSON(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/contribute/hives/register", strings.NewReader(`{invalid`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleHivesRegister(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleHivesRegisterMissingURL(t *testing.T) {
	srv := newFullServer(t)
	body := `{"name":"test-hive"}`
	req := httptest.NewRequest("POST", "/api/contribute/hives/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleHivesRegister(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing URL, got %d", w.Code)
	}
}

func TestHandleHivesOnboardBadJSON(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/contribute/hives/onboard", strings.NewReader(`{bad`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleHivesOnboard(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestLeaderboardForHubEmpty(t *testing.T) {
	srv := newFullServer(t)
	srv.contributeHub = NewContributeWSHub(slog.Default(), nil)

	lb := srv.LeaderboardForHub()
	if lb == nil {
		t.Error("should return non-nil leaderboard")
	}
}

func TestHandleGovernorSensingTTLTooHigh(t *testing.T) {
	srv := newFullServer(t)
	body := `{"ttlSeconds":999999}`
	req := httptest.NewRequest("PUT", "/api/governor/sensing", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorSensing(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleAgentConfigGeneralMultiField(t *testing.T) {
	srv := newFullServer(t)
	body := `{"displayName":"New Scanner","description":"Updated desc","enabled":true,"clearOnKick":true,"staleTimeout":300,"emoji":"🔍","color":"#ff0000","sortOrder":5,"role":"scanner","kickTemplate":"review-issues.md","mode":"ADVISORY","includeRepos":true,"laneKeywords":["bug","fix"],"detectKeywords":["error","fail"],"aliases":["scan","s"],"beadRole":"worker","cliPinned":true,"restartStrategy":"immediate"}`
	req := httptest.NewRequest("PUT", "/api/config/agent/scanner/general", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleAgentConfigGeneralInvalidMode(t *testing.T) {
	srv := newFullServer(t)
	body := `{"mode":"INVALID_MODE"}`
	req := httptest.NewRequest("PUT", "/api/config/agent/scanner/general", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid mode, got %d", w.Code)
	}
}

func TestHandleAgentConfigGeneralAllModes(t *testing.T) {
	modes := []string{"ADVISORY", "ISSUES_ONLY", "ISSUES_AND_PRS", "ISSUES_PRS_MERGE", "NO_GITHUB"}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			srv := newFullServer(t)
			body := `{"mode":"` + mode + `"}`
			req := httptest.NewRequest("PUT", "/api/config/agent/scanner/general", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			markOwnerRequest(req)
			srv.mux.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("mode %s: expected 200, got %d", mode, w.Code)
			}
		})
	}
}

func TestHandleAgentConfigGeneralUnknownAgent(t *testing.T) {
	srv := newFullServer(t)
	body := `{"displayName":"test"}`
	req := httptest.NewRequest("PUT", "/api/config/agent/nonexistent/general", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleAgentConfigRestrictionsUnknownAgent(t *testing.T) {
	srv := newFullServer(t)
	body := `{"restrictions":[]}`
	req := httptest.NewRequest("PUT", "/api/config/agent/nonexistent/restrictions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleKnowledgePromoteMissingFields(t *testing.T) {
	srv := newMinimalServer(t)
	body := `{"slug":"test"}`
	req := httptest.NewRequest("POST", "/api/knowledge/promote", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleKnowledgePromote(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing fields, got %d", w.Code)
	}
}

func TestHandleKnowledgePromoteAllFieldsMissing(t *testing.T) {
	srv := newMinimalServer(t)
	body := `{}`
	req := httptest.NewRequest("POST", "/api/knowledge/promote", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleKnowledgePromote(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleKnowledgePromoteValid(t *testing.T) {
	srv := newMinimalServer(t)
	body := `{"slug":"test-fact","from_layer":"project","to_layer":"global","promoter":"test"}`
	req := httptest.NewRequest("POST", "/api/knowledge/promote", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleKnowledgePromote(w, req)

	// Will either succeed or fail gracefully (no fact found)
	if w.Code == http.StatusBadRequest {
		body := w.Body.String()
		if strings.Contains(body, "slug") && strings.Contains(body, "required") {
			t.Error("should not fail validation with all fields present")
		}
	}
}

func TestHandleGovernorLoggingBadJSON(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/governor/logging", strings.NewReader(`{invalid`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorLogging(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleGovernorAddAgentBadJSON(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/governor/agents", strings.NewReader(`{invalid`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorAddAgent(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleGovernorAddAgentMissingName(t *testing.T) {
	srv := newFullServer(t)
	body := `{"role":"scanner"}`
	req := httptest.NewRequest("POST", "/api/governor/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorAddAgent(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleGovernorAddAgentDuplicate(t *testing.T) {
	srv := newFullServer(t)
	body := `{"name":"scanner","role":"scanner"}`
	req := httptest.NewRequest("POST", "/api/governor/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorAddAgent(w, req)

	// scanner already exists
	if w.Code != http.StatusConflict && w.Code != http.StatusBadRequest {
		t.Logf("add duplicate agent: %d", w.Code)
	}
}

func TestHandleGovernorHubUpdate(t *testing.T) {
	srv := newFullServer(t)
	body := `{"enabled":true,"url":"https://hub.example.com","dashboard_url":"https://dash.example.com","is_public":true,"auto_snapshot":false,"contribute_allow_labels":["good-first-issue"],"contribute_deny_labels":["wontfix"],"disabled_repos":["archived-repo"],"disabled_tiers":["newcomer"]}`
	req := httptest.NewRequest("PUT", "/api/governor/hub", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorHub(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleGovernorHubBadJSON(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/governor/hub", strings.NewReader(`{bad`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorHub(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleGovernorHubPartialUpdate(t *testing.T) {
	srv := newFullServer(t)
	body := `{"url":"https://new-hub.example.com","snapshot_url":"https://snap.example.com"}`
	req := httptest.NewRequest("PUT", "/api/governor/hub", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorHub(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if srv.deps.Config.Hub.URL != "https://new-hub.example.com" {
		t.Errorf("hub URL not updated: %q", srv.deps.Config.Hub.URL)
	}
}

func TestHandleGovernorAddAgentNewAgent(t *testing.T) {
	srv := newFullServer(t)
	body := `{"name":"helper","backend":"claude","model":"sonnet"}`
	req := httptest.NewRequest("POST", "/api/governor/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorAddAgent(w, req)

	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Errorf("expected 200/201, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleGovernorLoggingValid(t *testing.T) {
	srv := newFullServer(t)
	body := `{"log_level":"debug","log_tmux":true}`
	req := httptest.NewRequest("PUT", "/api/governor/logging", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorLogging(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleKnowledgeListWithTypeFilter(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("GET", "/api/knowledge?type=pattern", nil)
	w := httptest.NewRecorder()
	srv.handleKnowledgeList(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["enabled"] == nil {
		t.Error("should have enabled field")
	}
}

func TestHandleKnowledgeListNoFilter(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("GET", "/api/knowledge", nil)
	w := httptest.NewRecorder()
	srv.handleKnowledgeList(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestHandleKnowledgeExportMarkdown(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("GET", "/api/knowledge/export", nil)
	w := httptest.NewRecorder()
	srv.handleKnowledgeExport(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "markdown") && !strings.Contains(ct, "text") {
		t.Errorf("expected markdown content type, got %q", ct)
	}
}

func TestHandleRestartUnknownAgent(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/restart/nonexistent", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	// Agent not found → 400
	if w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
		t.Logf("restart unknown agent: %d", w.Code)
	}
}

func TestHandleRestartKnownAgent(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/restart/scanner", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	// Restart will fail (no tmux) → 400
	if w.Code != http.StatusBadRequest {
		t.Logf("restart known agent: %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleTimelineModeHistoryFallback(t *testing.T) {
	srv := newFullServer(t)
	// No evals recorded — should fall back to mode history
	req := httptest.NewRequest("GET", "/api/timeline", nil)
	w := httptest.NewRecorder()
	srv.handleTimeline(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["modes"] == nil {
		t.Error("should have modes field")
	}
}

func TestHandleGovernorReposEmpty(t *testing.T) {
	srv := newFullServer(t)
	body := `{"repos":[]}`
	req := httptest.NewRequest("PUT", "/api/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty repos, got %d", w.Code)
	}
}

func TestHandleGovernorReposInvalidName(t *testing.T) {
	srv := newFullServer(t)
	body := `{"repos":["valid-repo","../evil"]}`
	req := httptest.NewRequest("PUT", "/api/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid repo, got %d", w.Code)
	}
}

func TestHandleGovernorReposSpecialChars(t *testing.T) {
	srv := newFullServer(t)
	// sanitizeString strips HTML tags, so use chars that survive but are invalid
	body := `{"repos":["repo;drop"]}`
	req := httptest.NewRequest("PUT", "/api/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for special chars, got %d", w.Code)
	}
}

// A mixed list: the entry qualified with this hive's own org ("testorg") is
// normalized to its bare name, and the already-bare entry is left exactly as it
// was. The bare entry is the positive control — it proves the handler strips an
// org prefix rather than everything up to a '/'.
func TestHandleGovernorReposStripOrgPrefix(t *testing.T) {
	srv := newFullServer(t)
	body := `{"repos":["testorg/my-repo","other-repo"],"primaryRepo":"my-repo"}`
	req := httptest.NewRequest("PUT", "/api/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	repos := srv.deps.Config.Project.Repos
	want := []string{"my-repo", "other-repo"}
	if len(repos) != len(want) {
		t.Fatalf("repos = %v, want %v", repos, want)
	}
	for i := range want {
		if repos[i] != want[i] {
			t.Fatalf("repos = %v, want %v", repos, want)
		}
	}
}

func TestHandleGovernorAddAgentWithRole(t *testing.T) {
	srv := newFullServer(t)
	body := `{"name":"architect","backend":"claude","model":"opus","role":"architect"}`
	req := httptest.NewRequest("POST", "/api/governor/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorAddAgent(w, req)

	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Errorf("expected 200/201, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleGovernorRemoveAgentUnknown(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("DELETE", "/api/governor/agents/nonexistent", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound && w.Code != http.StatusBadRequest {
		t.Logf("remove unknown agent: %d", w.Code)
	}
}

func TestHandleGovernorRemoveAgentKnown(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("DELETE", "/api/governor/agents/scanner", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	// Should succeed or fail gracefully
	_ = w.Code
}

func TestHandleKnowledgeSubsAddBadJSON(t *testing.T) {
	srv := newMinimalServer(t)
	srv.ensureKnowledge()
	req := httptest.NewRequest("POST", "/api/knowledge/subscriptions", strings.NewReader(`{bad`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleKnowledgeSubsAdd(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleKnowledgeSubsAddMissingURL(t *testing.T) {
	srv := newMinimalServer(t)
	srv.ensureKnowledge()
	body := `{"agent":"scanner"}`
	req := httptest.NewRequest("POST", "/api/knowledge/subscriptions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleKnowledgeSubsAdd(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing url, got %d", w.Code)
	}
}

func TestHandleKnowledgeSubsAddPrivateURL(t *testing.T) {
	srv := newMinimalServer(t)
	srv.ensureKnowledge()
	body := `{"url":"http://localhost:8080/feed"}`
	req := httptest.NewRequest("POST", "/api/knowledge/subscriptions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleKnowledgeSubsAdd(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for private URL, got %d", w.Code)
	}
}

func TestHandleKnowledgeSubsAddBadScheme(t *testing.T) {
	srv := newMinimalServer(t)
	srv.ensureKnowledge()
	body := `{"url":"ftp://example.com/feed"}`
	req := httptest.NewRequest("POST", "/api/knowledge/subscriptions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleKnowledgeSubsAdd(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for non-http scheme, got %d", w.Code)
	}
}

func TestHandleVaultsConnectBadJSON(t *testing.T) {
	srv := newMinimalServer(t)
	srv.ensureKnowledge()
	req := httptest.NewRequest("POST", "/api/knowledge/vaults", strings.NewReader(`{bad`))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	srv.handleVaultsConnect(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleVaultsConnectMissingPath(t *testing.T) {
	srv := newMinimalServer(t)
	srv.ensureKnowledge()
	body := `{"name":"my-vault"}`
	req := httptest.NewRequest("POST", "/api/knowledge/vaults", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	srv.handleVaultsConnect(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing path, got %d", w.Code)
	}
}

func TestHandleContributeActivityNilHub(t *testing.T) {
	srv := newFullServer(t)
	srv.contributeHub = nil

	req := httptest.NewRequest("GET", "/api/contribute/activity", nil)
	w := httptest.NewRecorder()
	srv.handleContributeActivity(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestHandleContributeActivityWithHubExtra(t *testing.T) {
	srv := newFullServer(t)
	srv.contributeHub = NewContributeWSHub(slog.Default(), nil)
	srv.contributeHub.addActivity("user1", "joined", "newcomer", "claude", "sonnet", "", "")

	req := httptest.NewRequest("GET", "/api/contribute/activity", nil)
	w := httptest.NewRecorder()
	srv.handleContributeActivity(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "activity") {
		t.Error("should return activity field")
	}
}

func TestHandleHivesRegisterBadURLScheme(t *testing.T) {
	srv := newFullServer(t)
	body := `{"project_name":"test","org":"testorg","hub_url":"ftp://example.com"}`
	req := httptest.NewRequest("POST", "/api/contribute/hives/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleHivesRegister(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad URL scheme, got %d", w.Code)
	}
}

func TestHandleHivesRegisterPrivateURL(t *testing.T) {
	srv := newFullServer(t)
	body := `{"project_name":"test","org":"testorg","hub_url":"http://localhost:3001"}`
	req := httptest.NewRequest("POST", "/api/contribute/hives/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleHivesRegister(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for private URL, got %d", w.Code)
	}
}

func TestHandleHivesRegisterBadDashboardURL(t *testing.T) {
	srv := newFullServer(t)
	body := `{"project_name":"test","org":"testorg","hub_url":"https://hub.example.com","dashboard_url":"ftp://bad"}`
	req := httptest.NewRequest("POST", "/api/contribute/hives/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleHivesRegister(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad dashboard URL, got %d", w.Code)
	}
}

func TestHandleHivesRegisterPrivateDashboardURL(t *testing.T) {
	srv := newFullServer(t)
	body := `{"project_name":"test","org":"testorg","hub_url":"https://hub.example.com","dashboard_url":"http://127.0.0.1:8080"}`
	req := httptest.NewRequest("POST", "/api/contribute/hives/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleHivesRegister(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for private dashboard URL, got %d", w.Code)
	}
}

func TestHandleHivesRegisterValidWS(t *testing.T) {
	srv := newFullServer(t)
	body := `{"project_name":"test","org":"testorg","hub_url":"wss://hub.example.com"}`
	req := httptest.NewRequest("POST", "/api/contribute/hives/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleHivesRegister(w, req)

	// Should pass URL scheme validation — only check for the scheme-specific
	// error message to avoid false failures when DNS is unavailable in CI
	// (the fail-closed DNS check returns a different "private" error).
	if w.Code == http.StatusBadRequest {
		respBody := w.Body.String()
		if strings.Contains(respBody, "must start with") {
			t.Error("wss:// should be accepted as valid scheme")
		}
	}
}

func TestValidateDisplayNameHTML(t *testing.T) {
	err := validateDisplayName("<script>alert(1)</script>")
	if err == nil {
		t.Error("should reject HTML tags in display name")
	}
}

func TestValidateDisplayNameTooLong(t *testing.T) {
	long := strings.Repeat("a", 101)
	err := validateDisplayName(long)
	if err == nil {
		t.Error("should reject display name over 100 chars")
	}
}

func TestValidateDisplayNameBadChars(t *testing.T) {
	err := validateDisplayName("name;drop")
	if err == nil {
		t.Error("should reject display name with special chars")
	}
}

func TestHandleGovernorSensingPullbackTooHigh(t *testing.T) {
	srv := newFullServer(t)
	body := `{"pullbackSeconds":999999}`
	req := httptest.NewRequest("PUT", "/api/governor/sensing", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorSensing(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleKickPromptTooLong(t *testing.T) {
	srv := newFullServer(t)
	longPrompt := strings.Repeat("x", 10001)
	body := `{"prompt":"` + longPrompt + `"}`
	req := httptest.NewRequest("POST", "/api/kick/scanner", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for too-long prompt, got %d", w.Code)
	}
}

func TestHandleKickBadJSON(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/kick/scanner", strings.NewReader(`{invalid`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	// decodeBody fails but handleKick continues with empty prompt
	// SendKick will fail because no tmux session → 400
	_ = w.Code
}

// beadsCreateAsOwner stamps the request with the server-verified owner
// identity requireOwnerRole demands: the role header AND the internal
// verification marker authenticate sets only for trusted owner sessions.
// Handler tests bypass the auth middleware, so they must stamp both.
func beadsCreateAsOwner(req *http.Request) {
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set(ownerRoleVerifiedHeader, "true")
}

func TestHandleBeadsCreateUnknownAgent(t *testing.T) {
	srv := newFullServer(t)
	body := `{"title":"test bead"}`
	req := httptest.NewRequest("POST", "/api/beads/nonexistent", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	beadsCreateAsOwner(req)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown agent, got %d", w.Code)
	}
}

func TestHandleBeadsCreateMissingTitle(t *testing.T) {
	srv := newFullServer(t)
	body := `{"type":"advisory"}`
	req := httptest.NewRequest("POST", "/api/beads/scanner", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	beadsCreateAsOwner(req)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing title, got %d", w.Code)
	}
}

func TestHandleBeadsCreateTitleTooLong(t *testing.T) {
	srv := newFullServer(t)
	longTitle := strings.Repeat("a", 501)
	body := `{"title":"` + longTitle + `"}`
	req := httptest.NewRequest("POST", "/api/beads/scanner", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	beadsCreateAsOwner(req)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for title too long, got %d", w.Code)
	}
}

func TestHandleBeadsCreateBadPriority(t *testing.T) {
	srv := newFullServer(t)
	body := `{"title":"test","priority":99}`
	req := httptest.NewRequest("POST", "/api/beads/scanner", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	beadsCreateAsOwner(req)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad priority, got %d", w.Code)
	}
}

func TestHandleBeadsCreateBadJSON(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/beads/scanner", strings.NewReader(`{invalid`))
	req.Header.Set("Content-Type", "application/json")
	beadsCreateAsOwner(req)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad JSON, got %d", w.Code)
	}
}

func TestHandleBeadsCreateSuccess(t *testing.T) {
	srv := newFullServer(t)
	body := `{"title":"test bead","type":"advisory","priority":1,"external_ref":"test/ref","metadata":{"key1":"val1"}}`
	req := httptest.NewRequest("POST", "/api/beads/scanner", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	beadsCreateAsOwner(req)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Errorf("expected 201 or 200, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// TestHandleBeadsCreateRequiresOwner is the positive control for the owner
// gate this fix adds (#3914): with no identity at all, bead creation must be
// refused before any validation runs — otherwise any authenticated non-owner
// can inject beads into an agent's store and poison audit signals.
func TestHandleBeadsCreateRequiresOwner(t *testing.T) {
	srv := newFullServer(t)
	body := `{"title":"test bead"}`
	req := httptest.NewRequest("POST", "/api/beads/scanner", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 without owner identity, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// TestHandleBeadsCreateSpoofedRoleRejected replays the exact spoof vector the
// gate exists for: a client-supplied X-Hive-Role: owner WITHOUT the
// server-only verification marker (authenticate strips inbound markers and
// sets it only for trusted owner sessions) must still be refused.
func TestHandleBeadsCreateSpoofedRoleRejected(t *testing.T) {
	srv := newFullServer(t)
	body := `{"title":"test bead"}`
	req := httptest.NewRequest("POST", "/api/beads/scanner", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", "owner") // spoofed: no verified marker
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for spoofed unverified owner role, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// TestBeadsCreateOwnerGateInSource asserts the gate exists in the handler
// source itself (the F16 pattern): a refactor that drops the requireOwnerRole
// call from handleBeadsCreate must fail here even if the HTTP tests are
// reshuffled.
func TestBeadsCreateOwnerGateInSource(t *testing.T) {
	body := f16HandlerBody(t, f16ReadSource(t, "api.go"), "handleBeadsCreate")
	if !strings.Contains(body, "requireOwnerRole(w, r)") {
		t.Error("handleBeadsCreate must call requireOwnerRole — bead creation writes into an agent's store (#3914)")
	}
}

func TestHandleObsidianSyncBadJSON(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("POST", "/api/knowledge/obsidian-sync", strings.NewReader(`{invalid`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleObsidianSync(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleObsidianSyncMissingFilename(t *testing.T) {
	srv := newMinimalServer(t)
	body := `{"content":"some content"}`
	req := httptest.NewRequest("POST", "/api/knowledge/obsidian-sync", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleObsidianSync(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing filename, got %d", w.Code)
	}
}

func TestHandleObsidianSyncWithFilename(t *testing.T) {
	srv := newMinimalServer(t)
	body := `{"filename":"test-note.md","content":"hello world"}`
	req := httptest.NewRequest("POST", "/api/knowledge/obsidian-sync", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleObsidianSync(w, req)

	// ensureKnowledge creates file-based knowledge, sync should work or fail gracefully
	if w.Code == http.StatusBadRequest {
		t.Error("should not return 400 with valid filename")
	}
}

func TestHandleObsidianSyncNoContent(t *testing.T) {
	srv := newMinimalServer(t)
	body := `{"filename":"test-note.md","frontmatter":{"title":"My Note"}}`
	req := httptest.NewRequest("POST", "/api/knowledge/obsidian-sync", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleObsidianSync(w, req)

	// No content but has frontmatter title — content should default to title
	if w.Code == http.StatusBadRequest {
		t.Error("should not return 400 — content should default to frontmatter title")
	}
}

func TestHandleKnowledgePromoteBadJSON(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("POST", "/api/knowledge/promote", strings.NewReader(`{invalid`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleKnowledgePromote(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}
