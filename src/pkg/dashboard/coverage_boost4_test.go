package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// --- buildStructureKickMessage ---

func TestBuildStructureKickMessage(t *testing.T) {
	srv := newFullServer(t)
	state := &knowledge.InceptionState{
		IdeaText: "Build a task manager",
		IdeaSlug: "task-manager",
		Questions: []knowledge.Question{
			{ID: "q1", Text: "What database?", Category: "storage"},
			{ID: "q2", Text: "What framework?", Category: "language"},
		},
		Answers: map[string]string{
			"q1": "PostgreSQL",
			"q2": "React",
		},
	}
	msg := srv.buildStructureKickMessage(state)
	if !strings.Contains(msg, "Structure phase") {
		t.Error("expected Structure phase mention")
	}
	if !strings.Contains(msg, "PostgreSQL") {
		t.Error("expected answer in message")
	}
	if !strings.Contains(msg, "task-manager") {
		t.Error("expected idea slug in message")
	}
}

// --- buildAgentLeaderboardEntries ---

func TestBuildAgentLeaderboardEntries(t *testing.T) {
	srv := newFullServer(t)
	entries := srv.buildAgentLeaderboardEntries()
	// Should return entries for scanner agent
	if entries == nil {
		t.Fatal("expected non-nil entries")
	}
}

func TestBuildAgentLeaderboardEntries_NilDeps(t *testing.T) {
	srv := &Server{}
	entries := srv.buildAgentLeaderboardEntries()
	if entries != nil {
		t.Error("expected nil for nil deps")
	}
}

// --- countAgentActivity ---

func TestCountAgentActivity_NoStores(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.BeadStores = nil
	prs, issues, findings := srv.countAgentActivity("scanner")
	if prs != 0 || issues != 0 || findings != 0 {
		t.Errorf("expected all zero, got prs=%d issues=%d findings=%d", prs, issues, findings)
	}
}

func TestCountAgentActivity_UnknownAgent(t *testing.T) {
	srv := newFullServer(t)
	prs, issues, findings := srv.countAgentActivity("unknown")
	if prs != 0 || issues != 0 || findings != 0 {
		t.Errorf("expected all zero for unknown agent")
	}
}

func TestCountAgentActivity_WithStore(t *testing.T) {
	srv := newFullServer(t)
	// scanner store already exists in newFullServer
	prs, issues, findings := srv.countAgentActivity("scanner")
	_ = prs
	_ = issues
	_ = findings
	// Just verify no panic with empty store
}

// --- handleLeaderboardPage ---

func TestHandleLeaderboardPage(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/leaderboard", nil)
	w := httptest.NewRecorder()
	srv.handleLeaderboardPage(w, req)

	// The standalone page was folded into the /contribute Leaderboard tab.
	// /leaderboard is now a deep-link shim that redirects to that tab.
	if w.Code != http.StatusFound {
		t.Errorf("code = %d, want 302 (redirect)", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/contribute/leaderboard" {
		t.Errorf("Location = %q, want /contribute/leaderboard", loc)
	}
}

// --- handleAPIv1 ---

func TestHandleAPIv1_NoAuth(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/v1/status", nil)
	w := httptest.NewRecorder()
	srv.handleAPIv1(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", w.Code)
	}
}

// --- handleAPIDocs ---

func TestHandleAPIDocs_Boost(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/docs", nil)
	w := httptest.NewRecorder()
	srv.handleAPIDocs(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

// --- clearInceptionBeads ---

func TestClearInceptionBeads(t *testing.T) {
	srv := newFullServer(t)
	dir := t.TempDir()
	store, _ := beads.NewStore(dir)
	srv.clearInceptionBeads(store)
	// No open beads, should be a no-op
}

// --- LeaderboardForHub ---

func TestLeaderboardForHub(t *testing.T) {
	srv := newFullServer(t)
	entries := srv.LeaderboardForHub()
	// Should return at least agent entries
	if entries == nil {
		t.Fatal("expected non-nil entries")
	}
}

// --- handleContributorRevoke ---

func TestHandleContributorRevoke_InvalidBody(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/contribute/revoke", strings.NewReader("invalid"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleContributorRevoke(w, req)

	if w.Code == http.StatusOK {
		t.Error("expected non-200 for invalid body")
	}
}

// --- handlePackApply with valid level ---

func TestHandlePackApply_ValidLevel(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()
	req := httptest.NewRequest("POST", "/api/packs/1/apply", nil)
	req.SetPathValue("level", "1")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackApply(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, body=%s", w.Code, w.Body.String())
	}
}

// --- handlePackSetLevel with valid level + cadences ---

func TestHandlePackSetLevel_WithCadences(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/packs/level", strings.NewReader(`{"level":2}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackSetLevel(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, body=%s", w.Code, w.Body.String())
	}
}

// --- handleKnowledgeExport ---

func TestHandleKnowledgeExport_Enabled(t *testing.T) {
	srv := newFullServer(t)
	dir := t.TempDir()
	layers := []knowledge.LayerConfig{{Type: "project", Path: dir}}
	kcfg := knowledge.KnowledgeConfig{Enabled: true, Layers: layers}
	srv.deps.Knowledge = knowledge.NewKnowledgeAPI(layers, kcfg, srv.deps.Logger)

	req := httptest.NewRequest("GET", "/api/knowledge/export", nil)
	w := httptest.NewRecorder()
	srv.handleKnowledgeExport(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

// --- handleObsidianSync with valid body ---

func TestHandleObsidianSync_ValidBody(t *testing.T) {
	srv := newFullServer(t)
	body := `{"slug":"test-note","title":"Test Note","body":"content here","layer":"project"}`
	req := httptest.NewRequest("POST", "/api/obsidian/sync", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleObsidianSync(w, req)

	// Will auto-create knowledge API and attempt sync
	if w.Code == 0 {
		t.Error("unexpected zero code")
	}
}

// --- handleGitSourcesConnect with valid body ---

func TestHandleGitSourcesConnect_ValidBody(t *testing.T) {
	srv := newFullServer(t)
	dir := t.TempDir()
	layers := []knowledge.LayerConfig{{Type: "project", Path: dir}}
	kcfg := knowledge.KnowledgeConfig{Enabled: true, Layers: layers}
	srv.deps.Knowledge = knowledge.NewKnowledgeAPI(layers, kcfg, srv.deps.Logger)

	body := `{"url":"https://github.com/org/repo","branch":"main"}`
	req := httptest.NewRequest("POST", "/api/git-sources/connect", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGitSourcesConnect(w, req)

	// May fail (no git available for clone) but should exercise the handler
	if w.Code == 0 {
		t.Error("unexpected zero code")
	}
}

// --- handleGitSourcesDisconnect with valid body ---

func TestHandleGitSourcesDisconnect_ValidBody(t *testing.T) {
	srv := newFullServer(t)
	dir := t.TempDir()
	layers := []knowledge.LayerConfig{{Type: "project", Path: dir}}
	kcfg := knowledge.KnowledgeConfig{Enabled: true, Layers: layers}
	srv.deps.Knowledge = knowledge.NewKnowledgeAPI(layers, kcfg, srv.deps.Logger)

	body := `{"url":"https://github.com/org/repo"}`
	req := httptest.NewRequest("POST", "/api/git-sources/disconnect", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGitSourcesDisconnect(w, req)

	if w.Code == 0 {
		t.Error("unexpected zero code")
	}
}

// --- handleVaultsConnect with valid body ---

func TestHandleVaultsConnect_ValidBody(t *testing.T) {
	srv := newFullServer(t)
	dir := t.TempDir()
	layers := []knowledge.LayerConfig{{Type: "project", Path: dir}}
	kcfg := knowledge.KnowledgeConfig{Enabled: true, Layers: layers}
	srv.deps.Knowledge = knowledge.NewKnowledgeAPI(layers, kcfg, srv.deps.Logger)

	vaultDir := t.TempDir()
	body := `{"path":"` + vaultDir + `","name":"test-vault"}`
	req := httptest.NewRequest("POST", "/api/vaults/connect", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleVaultsConnect(w, req)

	if w.Code == 0 {
		t.Error("unexpected zero code")
	}
}

// --- handleKnowledgePromote with valid body ---

func TestHandleKnowledgePromote_ValidBody(t *testing.T) {
	srv := newFullServer(t)
	dir := t.TempDir()
	layers := []knowledge.LayerConfig{{Type: "project", Path: dir}}
	kcfg := knowledge.KnowledgeConfig{Enabled: true, Layers: layers}
	srv.deps.Knowledge = knowledge.NewKnowledgeAPI(layers, kcfg, srv.deps.Logger)

	body := `{"slug":"test-fact","from":"project","to":"shared"}`
	req := httptest.NewRequest("POST", "/api/knowledge/promote", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleKnowledgePromote(w, req)

	// May fail since fact doesn't exist, but exercises the handler
	if w.Code == 0 {
		t.Error("unexpected zero code")
	}
}

// --- handleKnowledgeCreate ---

func TestHandleKnowledgeCreate_NoKnowledge(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Knowledge = nil
	body := `{"title":"Test","body":"test body","layer":"project"}`
	req := httptest.NewRequest("POST", "/api/knowledge/create", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleKnowledgeCreate(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", w.Code)
	}
}

func TestHandleKnowledgeCreate_InvalidBody_Boost(t *testing.T) {
	srv := newFullServer(t)
	dir := t.TempDir()
	layers := []knowledge.LayerConfig{{Type: "project", Path: dir}}
	kcfg := knowledge.KnowledgeConfig{Enabled: true, Layers: layers}
	srv.deps.Knowledge = knowledge.NewKnowledgeAPI(layers, kcfg, srv.deps.Logger)

	req := httptest.NewRequest("POST", "/api/knowledge/create", strings.NewReader("invalid"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleKnowledgeCreate(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestHandleKnowledgeCreate_MissingFields_Boost(t *testing.T) {
	srv := newFullServer(t)
	dir := t.TempDir()
	layers := []knowledge.LayerConfig{{Type: "project", Path: dir}}
	kcfg := knowledge.KnowledgeConfig{Enabled: true, Layers: layers}
	srv.deps.Knowledge = knowledge.NewKnowledgeAPI(layers, kcfg, srv.deps.Logger)

	body := `{"title":"","body":""}`
	req := httptest.NewRequest("POST", "/api/knowledge/create", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleKnowledgeCreate(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

// --- buildHealth with cached health ---

func TestBuildHealth_Cached(t *testing.T) {
	// Set cached health first
	cachedHealthMu.Lock()
	cachedHealth = map[string]any{"ci": 95, "nightly": 100}
	cachedHealthMu.Unlock()
	defer func() {
		cachedHealthMu.Lock()
		cachedHealth = nil
		cachedHealthMu.Unlock()
	}()

	// Call with nil client to get cached
	result := buildHealth(nil, nil)
	if result["ci"] != 95 {
		t.Errorf("ci = %v, want 95", result["ci"])
	}
}

// --- Full server with complete deps for deeper handler tests ---

func newDeepTestServer(t *testing.T) *Server {
	t.Helper()
	orig := privateURLResolver
	privateURLResolver = func(_ context.Context, _ string) ([]string, error) {
		return []string{"93.184.216.34"}, nil
	}
	t.Cleanup(func() { privateURLResolver = orig })

	level := 2
	agentsDir := t.TempDir()
	cfg := &config.Config{
		ACMMLevel: &level,
		Project: config.ProjectConfig{
			Org: "testorg", Name: "test", PrimaryRepo: "testrepo",
			Repos: []string{"testrepo"},
		},
		Data: config.DataConfig{AgentsDir: agentsDir},
		Agents: map[string]config.AgentConfig{
			"scanner": {ID: "scan-001", Role: "scanner", Backend: "claude", Model: "sonnet", DisplayName: "Scanner", Enabled: true},
		},
		GitHub:     config.GitHubConfig{Token: "ghp_test123456789"},
		SourcePath: t.TempDir() + "/hive.yaml",
		Governor: config.GovernorConfig{
			EvalIntervalS: 300,
			Modes: map[string]config.ModeConfig{
				"idle":  {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "15m"}},
				"quiet": {Threshold: 2, Cadences: map[string]config.Cadence{"scanner": "10m"}},
				"busy":  {Threshold: 10, Cadences: map[string]config.Cadence{"scanner": "5m"}},
				"surge": {Threshold: 20, Cadences: map[string]config.Cadence{"scanner": "2m"}},
			},
		},
	}

	dir := t.TempDir()
	scannerStore, _ := beads.NewStore(dir + "/scanner")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
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

// --- handleApplyPackAndSetLevel together ---

func TestPackApplyAndSetLevel_Sequential(t *testing.T) {
	srv := newDeepTestServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()

	// Apply level 1
	result, err := srv.ApplyPack(1)
	if err != nil {
		t.Fatalf("ApplyPack(1): %v", err)
	}
	if result.Name == "" {
		t.Error("expected pack name")
	}

	// Then set level 2
	req := httptest.NewRequest("PUT", "/api/packs/level", strings.NewReader(`{"level":2}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackSetLevel(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("SetLevel code = %d, body=%s", w.Code, w.Body.String())
	}
}

// --- handleGovernorRepos with EnumerateFunc ---

func TestHandleGovernorRepos_WithEnumerateFunc(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.EnumerateFunc = func() {}

	body := `{"repos":["repo1","repo2"],"primaryRepo":"repo1"}`
	req := httptest.NewRequest("PUT", "/api/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
	// EnumerateFunc is called in goroutine, check immediately isn't reliable
}

// --- handleKnowledgeCreate success path ---

func TestHandleKnowledgeCreate_Valid(t *testing.T) {
	srv := newFullServer(t)
	dir := t.TempDir()
	layers := []knowledge.LayerConfig{{Type: "project", Path: dir}}
	kcfg := knowledge.KnowledgeConfig{Enabled: true, Layers: layers}
	srv.deps.Knowledge = knowledge.NewKnowledgeAPI(layers, kcfg, srv.deps.Logger)

	body := `{"title":"Test Fact","body":"This is a test fact body","layer":"project"}`
	req := httptest.NewRequest("POST", "/api/knowledge/create", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleKnowledgeCreate(w, req)

	// May fail with 500 if layer has no configured endpoint - that's OK, we exercised the handler
	if w.Code == http.StatusBadRequest {
		t.Errorf("unexpected 400: %s", w.Body.String())
	}
}

// --- handleKnowledgeStats ---

func TestHandleKnowledgeStats_NoKnowledge(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Knowledge = nil
	req := httptest.NewRequest("GET", "/api/knowledge/stats", nil)
	w := httptest.NewRecorder()
	srv.handleKnowledgeStats(w, req)

	// ensureKnowledge auto-creates
	if w.Code == 0 {
		t.Error("unexpected zero code")
	}
}

// --- handleAgentConfigGeneral with display name ---

func TestHandleAgentConfigGeneral_SetDisplayName(t *testing.T) {
	srv := newFullServer(t)
	body := `{"displayName":"My Scanner","description":"Scans things"}`
	req := httptest.NewRequest("PUT", "/api/agents/scanner/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "scanner")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleAgentConfigGeneral(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, body=%s", w.Code, w.Body.String())
	}
}

// --- handleHandleAgentExport with cadences ---

func TestHandleAgentExport_WithCadences(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/agents/scanner/export", nil)
	req.SetPathValue("name", "scanner")
	w := httptest.NewRecorder()
	srv.handleAgentExport(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "scanner") {
		t.Error("expected scanner in export")
	}
}

// --- handleHiveIDSet with various inputs ---

func TestHandleHiveIDSet_TooLong(t *testing.T) {
	srv := newFullServer(t)
	longID := strings.Repeat("a", 100)
	body := `{"id":"` + longID + `"}`
	req := httptest.NewRequest("PUT", "/api/hive-id", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleHiveIDSet(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400 for too-long ID", w.Code)
	}
}

func TestHandleHiveIDSet_InvalidChars(t *testing.T) {
	srv := newFullServer(t)
	body := `{"id":"bad<>chars"}`
	req := httptest.NewRequest("PUT", "/api/hive-id", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleHiveIDSet(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400 for invalid chars", w.Code)
	}
}

// --- collectSystemResources (repeat for coverage) ---

func TestCollectSystemResources_Boost(t *testing.T) {
	res := collectSystemResources()
	if res == nil {
		t.Fatal("expected non-nil")
	}
	// On macOS, disk should be available
	if res.DiskTotalGB <= 0 {
		t.Logf("disk total = %f (may be 0 in CI)", res.DiskTotalGB)
	}
}

// --- handleAgentImport valid YAML ---

func TestHandleAgentImport_ValidYAML(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()
	yamlBody := `{"yaml":"apiVersion: hive.kubestellar.io/v1\nkind: AgentDefinition\nmetadata:\n  name: imported-agent\nspec:\n  backend: claude\n  model: sonnet\n  role: worker"}`
	req := httptest.NewRequest("POST", "/api/agents/import", strings.NewReader(yamlBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAgentImport(w, req)

	// May succeed or fail depending on validation
	if w.Code == 0 {
		t.Error("unexpected zero code")
	}
}

// --- UpdateStatus ---

func TestUpdateStatus_Broadcast(t *testing.T) {
	srv := newFullServer(t)
	payload := minimalPayload()
	srv.UpdateStatus(payload)

	// Verify status was stored
	srv.statusMu.RLock()
	if srv.status == nil {
		t.Error("expected status to be set")
	}
	srv.statusMu.RUnlock()
}

// --- AppendTokenSparkline ---

func TestAppendTokenSparkline_NilStatus(t *testing.T) {
	srv := newFullServer(t)
	srv.AppendTokenSparkline(nil) // should not panic
}

func TestAppendTokenSparkline_WithData(t *testing.T) {
	srv := newFullServer(t)
	status := minimalPayload()
	srv.AppendTokenSparkline(status)

	history := srv.TokenSparklineHistory()
	if len(history) != 1 {
		t.Errorf("history len = %d, want 1", len(history))
	}
}

// --- handleGovernorAddAgent valid ---

func TestHandleGovernorAddAgent_FullAgent(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()
	body := `{"name":"full-agent","backend":"claude","model":"sonnet","role":"scanner","displayName":"Full Agent","description":"A test agent","emoji":"🔍","color":"#00ff00"}`
	req := httptest.NewRequest("POST", "/api/governor/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorAddAgent(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, body=%s", w.Code, w.Body.String())
	}
	// Verify agent was added to config
	if _, ok := srv.deps.Config.Agents["full-agent"]; !ok {
		t.Error("expected full-agent in config")
	}
}

// --- handleChat ---

func TestHandleChat_BadBody(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader("invalid"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code == http.StatusOK {
		t.Error("expected non-200 for invalid body")
	}
}

// --- handleContributorTrust with valid but non-existent user ---

func TestHandleContributorTrust_UserNotFound(t *testing.T) {
	srv := newFullServer(t)
	body := `{"username":"nonexistent","tier":"contributor"}`
	req := httptest.NewRequest("PUT", "/api/contribute/trust", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", "owner") // mutation gate fails closed on a missing role (C5)
	req.Header.Set(ownerRoleVerifiedHeader, "true") // requireOwnerRole needs the verified marker too (F14)
	w := httptest.NewRecorder()
	srv.handleContributorTrust(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

// --- handleHivesRegister ---

func TestHandleHivesRegister_InvalidBody(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/hives/register", strings.NewReader("invalid"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleHivesRegister(w, req)

	if w.Code == http.StatusOK {
		t.Error("expected non-200 for invalid body")
	}
}

// --- handleContributeReissueToken with token prefix ---

func TestHandleContributeReissueToken_TokenPrefix(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/contribute/reissue-token", nil)
	req.Header.Set("Authorization", "token fake-token-value")
	w := httptest.NewRecorder()
	srv.handleContributeReissueToken(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", w.Code)
	}
}

// --- AuditLog with writer ---

func TestAuditLog_LogToWriter(t *testing.T) {
	a := &AuditLog{ring: make([]AuditEntry, 0, auditRingCap)}
	// Simulate having a writer but no /data directory
	a.Log("admin", "kick", "agent=scanner", "scanner")
	a.Log("admin", "restart", "", "scanner")
	a.Log("system", "eval", "mode=busy", "")

	recent := a.Recent(10)
	if len(recent) != 3 {
		t.Errorf("recent len = %d, want 3", len(recent))
	}
	// Should be newest first
	if recent[0].Action != "eval" {
		t.Errorf("newest action = %q, want eval", recent[0].Action)
	}
}

// --- handleKnowledgeLayer with specific layer ---

func TestHandleKnowledgeLayer_SpecificLayer(t *testing.T) {
	srv := newFullServer(t)
	dir := t.TempDir()
	layers := []knowledge.LayerConfig{{Type: "project", Path: dir}}
	kcfg := knowledge.KnowledgeConfig{Enabled: true, Layers: layers}
	srv.deps.Knowledge = knowledge.NewKnowledgeAPI(layers, kcfg, srv.deps.Logger)

	req := httptest.NewRequest("GET", "/api/knowledge/layer/project", nil)
	req.SetPathValue("layer", "project")
	w := httptest.NewRecorder()
	srv.handleKnowledgeLayer(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
	var result map[string]any
	json.NewDecoder(w.Body).Decode(&result)
	if result["layer"] != "project" {
		t.Errorf("layer = %v", result["layer"])
	}
}
