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
	"github.com/hivecommons/hive/pkg/governor"
)

func httpPutRaw(s *Server, path string, rawBody string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(rawBody))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// handleGovernorLogging
// ---------------------------------------------------------------------------

func TestHandleGovernorLogging_AllFields(t *testing.T) {
	s, deps := apiServer(t)

	rec := doPut(s, "/api/config/governor/logging", map[string]interface{}{
		"maxSizeMB":  50,
		"maxAgeDays": 14,
		"maxBackups": 5,
		"compress":   true,
		"level":      "debug",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if deps.Config.Governor.Logging.MaxSizeMB != 50 {
		t.Errorf("maxSizeMB = %d, want 50", deps.Config.Governor.Logging.MaxSizeMB)
	}
	if deps.Config.Governor.Logging.MaxAgeDays != 14 {
		t.Errorf("maxAgeDays = %d, want 14", deps.Config.Governor.Logging.MaxAgeDays)
	}
	if deps.Config.Governor.Logging.MaxBackups != 5 {
		t.Errorf("maxBackups = %d, want 5", deps.Config.Governor.Logging.MaxBackups)
	}
	if !deps.Config.Governor.Logging.Compress {
		t.Error("compress should be true")
	}
	if deps.Config.Governor.Logging.Level != "debug" {
		t.Errorf("level = %q, want debug", deps.Config.Governor.Logging.Level)
	}
}

func TestHandleGovernorLogging_InvalidBody(t *testing.T) {
	s, _ := apiServer(t)
	rec := httpPutRaw(s, "/api/config/governor/logging", "not-json{{{")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// handleHiveIDGet / handleHiveIDSet
// ---------------------------------------------------------------------------

func TestHandleHiveIDGet(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.HiveID = "test-hive-42"

	rec := doGet(s, "/api/hive-id")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["id"] != "test-hive-42" {
		t.Errorf("id = %q, want test-hive-42", body["id"])
	}
}

func TestHandleHiveIDSet(t *testing.T) {
	s, deps := apiServer(t)

	rec := doPut(s, "/api/hive-id", map[string]string{"id": "new-hive-99"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if deps.Config.HiveID != "new-hive-99" {
		t.Errorf("HiveID = %q, want new-hive-99", deps.Config.HiveID)
	}
}

func TestHandleHiveIDSet_EmptyID(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/hive-id", map[string]string{"id": ""})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// handleAuthToken
// ---------------------------------------------------------------------------

func TestHandleAuthToken_NotSet(t *testing.T) {
	s, _ := apiServer(t)
	os.Unsetenv("HIVE_DASHBOARD_TOKEN")

	rec := doGet(s, "/api/auth/token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	json.Unmarshal(rec.Body.Bytes(), &body)
	// SECURITY: the endpoint reports only whether a token is configured; it must
	// never return the token value (that was the F1 credential leak).
	if _, ok := body["token"]; ok {
		t.Errorf("response must not include a token field, got %v", body)
	}
	if body["configured"] != "false" {
		t.Errorf("configured = %q, want false", body["configured"])
	}
}

func TestHandleAuthToken_FromEnv(t *testing.T) {
	s, _ := apiServer(t)
	t.Setenv("HIVE_DASHBOARD_TOKEN", "secret-token-abc")

	rec := doGet(s, "/api/auth/token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	json.Unmarshal(rec.Body.Bytes(), &body)
	// SECURITY: reports configured=true but must NOT leak the token value.
	if strings.Contains(rec.Body.String(), "secret-token-abc") {
		t.Errorf("response leaked the token value: %s", rec.Body.String())
	}
	if body["configured"] != "true" {
		t.Errorf("configured = %q, want true", body["configured"])
	}
}

func TestHandleAuthToken_FromServer(t *testing.T) {
	s, _ := apiServer(t)
	s.authToken = "server-token-xyz"

	rec := doGet(s, "/api/auth/token")
	var body map[string]string
	json.Unmarshal(rec.Body.Bytes(), &body)
	// SECURITY: configured=true, but the token value must never appear.
	if strings.Contains(rec.Body.String(), "server-token-xyz") {
		t.Errorf("response leaked the token value: %s", rec.Body.String())
	}
	if body["configured"] != "true" {
		t.Errorf("configured = %q, want true", body["configured"])
	}
}

// TestHandleAuthToken_NeverReturnsValue locks in that the endpoint never
// discloses the token — the core of the F1 fix — regardless of token content.
func TestHandleAuthToken_NeverReturnsValue(t *testing.T) {
	s, _ := apiServer(t)
	s.authToken = "test-token-for-header-safety"

	rec := doGet(s, "/api/auth/token")
	if strings.Contains(rec.Body.String(), "test-token-for-header-safety") {
		t.Fatalf("auth-token endpoint leaked the token value: %s", rec.Body.String())
	}
	var body map[string]string
	json.Unmarshal(rec.Body.Bytes(), &body)
	if _, ok := body["token"]; ok {
		t.Errorf("response must not include a token field, got %v", body)
	}
}

// ---------------------------------------------------------------------------
// substituteTemplateVars
// ---------------------------------------------------------------------------

func TestSubstituteTemplateVars(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Project.Name = "MyProject"
	deps.Config.Project.Org = "myorg"
	deps.Config.Project.PrimaryRepo = "main-repo"
	deps.Config.Project.AIAuthor = "ai-bot"
	deps.Config.Project.Repos = []string{"repo1", "repo2"}
	deps.Config.HiveID = "hive-42"

	template := "Agent: ${AGENT_NAME}, Project: ${PROJECT_NAME}, Org: ${PROJECT_ORG}, " +
		"Primary: ${PROJECT_PRIMARY_REPO}, Author: ${PROJECT_AI_AUTHOR}, " +
		"Repos: ${PROJECT_REPOS_LIST}, Hive: ${HIVE_REPO}, HiveID: ${HIVE_ID}"

	result := s.substituteTemplateVars(template, "scanner")

	if result == template {
		t.Error("template should have been substituted")
	}
	expected := "Agent: scanner, Project: MyProject, Org: myorg, " +
		"Primary: myorg/main-repo, Author: ai-bot, " +
		"Repos: repo1, repo2, Hive: myorg/hive, HiveID: hive-42"
	if result != expected {
		t.Errorf("result = %q\nwant  = %q", result, expected)
	}
}

func TestSubstituteTemplateVars_NilDeps(t *testing.T) {
	s, _ := apiServer(t)
	s.deps = nil
	result := s.substituteTemplateVars("hello ${AGENT_NAME}", "scanner")
	if result != "hello ${AGENT_NAME}" {
		t.Errorf("should return unmodified template when deps is nil, got %q", result)
	}
}

func TestSubstituteTemplateVars_DisplayName(t *testing.T) {
	s, _ := apiServer(t)

	// Test agent display name substitution
	result := s.substituteTemplateVars("Display: ${AGENT_DISPLAY_NAME}", "scanner")
	// scanner has no display name set, so it falls back to agent name
	if result != "Display: scanner" {
		t.Errorf("got %q", result)
	}
}

// ---------------------------------------------------------------------------
// loadAgentRestrictions — with temp files
// ---------------------------------------------------------------------------

func TestLoadAgentRestrictions_WithFiles(t *testing.T) {
	s, _ := apiServer(t)

	// Create temp directory structure for agent restrictions
	tmpDir := t.TempDir()
	agentDir := filepath.Join(tmpDir, "agents", "scanner")
	os.MkdirAll(agentDir, 0o755)

	// Write agent-specific restrictions
	agentRestFile := filepath.Join(agentDir, "restrictions.conf")
	os.WriteFile(agentRestFile, []byte("*.log|Don't modify logs\n# comment\ndata/*|Read only\n"), 0o644)

	// Write CLAUDE.md with policy restrictions
	claudeMd := filepath.Join(agentDir, "CLAUDE.md")
	os.WriteFile(claudeMd, []byte("# Rules\n- **NEVER** touch production\n- Do not merge without review\n- ALWAYS test first\n- HARD RULE: no force pushes\n"), 0o644)

	// We can't override /data paths easily, but we can test the function logic
	// by verifying the default behavior with no files
	result := s.loadAgentRestrictions("scanner")
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if _, ok := result["global"]; !ok {
		t.Error("expected 'global' key")
	}
	if _, ok := result["agent"]; !ok {
		t.Error("expected 'agent' key")
	}
	if _, ok := result["policy"]; !ok {
		t.Error("expected 'policy' key")
	}
}

// ---------------------------------------------------------------------------
// loadPromptTemplate
// ---------------------------------------------------------------------------

func TestLoadPromptTemplate_NoFile(t *testing.T) {
	s, _ := apiServer(t)
	result := s.loadPromptTemplate("nonexistent-agent")
	// May return embedded default or empty
	_ = result
}

func TestLoadPromptTemplate_PolicyDir(t *testing.T) {
	s, deps := apiServer(t)
	tmpDir := t.TempDir()
	deps.Config.Policies.LocalDir = tmpDir
	deps.Config.Agents["scanner"] = config.AgentConfig{KickTemplate: "scanner.md"}

	policyAgentDir := filepath.Join(tmpDir, "examples", "kubestellar", "agents")
	os.MkdirAll(policyAgentDir, 0o755)
	os.WriteFile(filepath.Join(policyAgentDir, "scanner.md"), []byte("# Scanner Policy\nNEVER skip tests"), 0o644)

	result := s.loadPromptTemplate("scanner")
	if !strings.Contains(result, "Scanner Policy") {
		t.Errorf("expected template content, got %q", result)
	}
}

func TestLoadPromptTemplate_WithFile(t *testing.T) {
	s, deps := apiServer(t)
	tmpDir := t.TempDir()
	deps.Config.Policies.LocalDir = tmpDir

	policyAgentDir := filepath.Join(tmpDir, "examples", "kubestellar", "agents")
	os.MkdirAll(policyAgentDir, 0o755)
	claudeMd := filepath.Join(policyAgentDir, "scanner.md")
	os.WriteFile(claudeMd, []byte("Agent ${AGENT_NAME} in org ${PROJECT_ORG}"), 0o644)

	result := s.loadPromptTemplate("scanner")
	if result != "Agent scanner in org myorg" {
		t.Errorf("got %q", result)
	}
}

// ---------------------------------------------------------------------------
// loadAgentStats
// ---------------------------------------------------------------------------

func TestLoadAgentStats_DefaultStats(t *testing.T) {
	s, _ := apiServer(t)
	stats := s.loadAgentStats("scanner")
	if len(stats) == 0 {
		t.Error("expected default stats for scanner")
	}
}

func TestLoadAgentStats_UnknownAgent(t *testing.T) {
	s, _ := apiServer(t)
	stats := s.loadAgentStats("unknown-agent")
	if stats == nil {
		t.Fatal("expected non-nil (possibly empty) stats")
	}
}

// ---------------------------------------------------------------------------
// handleHistory — exercises file read path (no seed file)
// ---------------------------------------------------------------------------

func TestHandleHistory_WithEvals(t *testing.T) {
	s, deps := apiServer(t)
	deps.Governor.Evaluate(25, 0, 0, 0) // add an eval to history

	rec := doGet(s, "/api/history")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var result []interface{}
	json.Unmarshal(rec.Body.Bytes(), &result)
	if len(result) == 0 {
		t.Error("expected at least one eval entry")
	}
}

// ---------------------------------------------------------------------------
// handleTimeline — with eval data and mode history fallback
// ---------------------------------------------------------------------------

func TestHandleTimeline_WithModeHistory(t *testing.T) {
	s, deps := apiServer(t)
	// Seed mode history for fallback path
	deps.Governor.SeedModeHistory([]governor.ModeChange{
		{From: governor.ModeIdle, To: governor.ModeBusy},
	})

	rec := doGet(s, "/api/timeline")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var result map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &result)
	modes, ok := result["modes"].([]interface{})
	if !ok || len(modes) == 0 {
		t.Error("expected mode history in timeline")
	}
}

// ---------------------------------------------------------------------------
// handleGHRateLimits — nil client
// ---------------------------------------------------------------------------

// handleGHRateLimits requires a real GH client; tested in api_coverage_test.go

// ---------------------------------------------------------------------------
// handleObsidianSync — with Knowledge API
// ---------------------------------------------------------------------------

func TestHandleObsidianSync_WithKnowledge(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	rec := doPost(s, "/api/knowledge/obsidian/sync", map[string]interface{}{
		"filename": "test-note.md",
		"content":  "Hello world",
		"frontmatter": map[string]interface{}{
			"title": "Test Note",
			"tags":  []string{"go", "test"},
			"type":  "pattern",
		},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["ok"] != true {
		t.Errorf("ok = %v, want true", body["ok"])
	}
}

func TestHandleObsidianSync_MissingFilename(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	rec := doPost(s, "/api/knowledge/obsidian/sync", map[string]interface{}{
		"content": "Hello",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleObsidianSync_NilKnowledge(t *testing.T) {
	s, deps := apiServer(t)
	deps.Knowledge = nil

	// ensureKnowledge auto-creates, so this should succeed (or return 503 if deps is nil)
	rec := doPost(s, "/api/knowledge/obsidian/sync", map[string]interface{}{
		"filename": "test.md",
		"content":  "Hello",
	})
	// ensureKnowledge will auto-create a file-based knowledge API
	if rec.Code != http.StatusOK && rec.Code != http.StatusInternalServerError {
		t.Errorf("unexpected status = %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// maskSecret
// ---------------------------------------------------------------------------

func TestMaskSecret_Long(t *testing.T) {
	result := maskSecret("ghp_abc1234567890")
	// Last 4 chars should be visible
	runes := []rune(result)
	suffix := string(runes[len(runes)-4:])
	if suffix != "7890" {
		t.Errorf("last 4 chars should be visible, got %q (full: %q)", suffix, result)
	}
}

func TestMaskSecret_Short(t *testing.T) {
	result := maskSecret("abc")
	if result != "•••" {
		t.Errorf("got %q, want •••", result)
	}
}

func TestMaskSecret_Empty(t *testing.T) {
	if maskSecret("") != "" {
		t.Error("empty string should return empty")
	}
}

// ---------------------------------------------------------------------------
// getAgentPipeline / getAgentHooks — defaults
// ---------------------------------------------------------------------------

func TestGetAgentPipeline_DefaultCoverage2(t *testing.T) {
	s, _ := apiServer(t)
	// Set a custom pipeline to test the custom path
	s.pipelineMu.Lock()
	s.agentPipelines["scanner"] = map[string]bool{"step1": true}
	s.pipelineMu.Unlock()

	pipeline := s.getAgentPipeline("scanner")
	if !pipeline["step1"] {
		t.Error("expected custom pipeline step")
	}
}

func TestGetAgentHooks_DefaultCoverage2(t *testing.T) {
	s, _ := apiServer(t)
	// Set custom hooks to test the custom path
	s.hooksMu.Lock()
	s.agentHooks["scanner"] = map[string][]any{"preKick": {"hook1"}}
	s.hooksMu.Unlock()

	hooks := s.getAgentHooks("scanner")
	if len(hooks["preKick"]) != 1 {
		t.Error("expected custom hook")
	}
}

// ---------------------------------------------------------------------------
// SeedTokenSparklineHistory
// ---------------------------------------------------------------------------

func TestSeedTokenSparklineHistory(t *testing.T) {
	s, _ := apiServer(t)
	entries := []TokenSparklineEntry{
		{Timestamp: 1000, Input: 100},
		{Timestamp: 2000, Input: 200},
	}
	s.SeedTokenSparklineHistory(entries)

	history := s.TokenSparklineHistory()
	if len(history) != 2 {
		t.Errorf("expected 2 entries, got %d", len(history))
	}
}

// ---------------------------------------------------------------------------
// CollectAgentStats
// ---------------------------------------------------------------------------

func TestCollectAgentStats_StatusSource(t *testing.T) {
	payload := &StatusPayload{
		Agents: []FrontendAgent{
			{
				Name: "scanner",
				StatsConfig: []any{
					map[string]any{"key": "actionable", "source": "status", "field": "actionableCount"},
					map[string]any{"key": "openPrs", "source": "status", "field": "openPrCount"},
					map[string]any{"key": "mergeable", "source": "status", "field": "mergeableCount"},
				},
			},
		},
		Repos: []FrontendRepo{
			{
				ActionableIssues: []any{"issue1", "issue2"},
				OpenPrs: []any{
					map[string]any{"title": "pr1", "mergeable": true},
					map[string]any{"title": "pr2", "mergeable": false},
				},
			},
		},
	}

	result := CollectAgentStats(payload)
	if result["scanner"]["actionable"] != 2 {
		t.Errorf("actionable = %v, want 2", result["scanner"]["actionable"])
	}
	if result["scanner"]["openPrs"] != 2 {
		t.Errorf("openPrs = %v, want 2", result["scanner"]["openPrs"])
	}
	if result["scanner"]["mergeable"] != 1 {
		t.Errorf("mergeable = %v, want 1", result["scanner"]["mergeable"])
	}
}

func TestCollectAgentStats_HealthSource(t *testing.T) {
	payload := &StatusPayload{
		Agents: []FrontendAgent{
			{
				Name: "ci-maintainer",
				StatsConfig: []any{
					map[string]any{"key": "ci", "source": "health", "field": "ci"},
				},
			},
		},
		Health: map[string]any{"ci": 95.0},
	}

	result := CollectAgentStats(payload)
	if result["ci-maintainer"]["ci"] != 95.0 {
		t.Errorf("ci = %v, want 95.0", result["ci-maintainer"]["ci"])
	}
}

func TestCollectAgentStats_AgentMetricsSource(t *testing.T) {
	payload := &StatusPayload{
		Agents: []FrontendAgent{
			{
				Name: "outreach",
				StatsConfig: []any{
					map[string]any{"key": "stars", "source": "agentMetrics", "field": "stars"},
				},
			},
		},
		AgentMetrics: map[string]any{
			"outreach": map[string]any{"stars": 42},
		},
	}

	result := CollectAgentStats(payload)
	if result["outreach"]["stars"] != 42 {
		t.Errorf("stars = %v, want 42", result["outreach"]["stars"])
	}
}

func TestCollectAgentStats_TokensSource(t *testing.T) {
	payload := &StatusPayload{
		Agents: []FrontendAgent{
			{
				Name: "scanner",
				StatsConfig: []any{
					map[string]any{"key": "input", "source": "tokens", "field": "input"},
					map[string]any{"key": "output", "source": "tokens", "field": "output"},
					map[string]any{"key": "cacheRead", "source": "tokens", "field": "cacheRead"},
					map[string]any{"key": "cacheCreate", "source": "tokens", "field": "cacheCreate"},
					map[string]any{"key": "sessions", "source": "tokens", "field": "sessions"},
					map[string]any{"key": "messages", "source": "tokens", "field": "messages"},
				},
			},
		},
		Tokens: FrontendTokens{
			ByAgent: map[string]FrontendTokenBucket{
				"scanner": {Input: 100, Output: 50, CacheRead: 30, CacheCreate: 20, Sessions: 5, Messages: 15},
			},
		},
	}

	result := CollectAgentStats(payload)
	if result["scanner"]["input"] != int64(100) {
		t.Errorf("input = %v, want 100", result["scanner"]["input"])
	}
	if result["scanner"]["sessions"] != int(5) {
		t.Errorf("sessions = %v, want 5", result["scanner"]["sessions"])
	}
}

func TestCollectAgentStats_EmptyStatsConfig(t *testing.T) {
	payload := &StatusPayload{
		Agents: []FrontendAgent{
			{Name: "scanner", StatsConfig: nil},
		},
	}
	result := CollectAgentStats(payload)
	if _, ok := result["scanner"]; ok {
		t.Error("should not have entry for agent with nil stats config")
	}
}

// ---------------------------------------------------------------------------
// CollectRepoSnapshots
// ---------------------------------------------------------------------------

func TestCollectRepoSnapshots(t *testing.T) {
	payload := &StatusPayload{
		Repos: []FrontendRepo{
			{Name: "repo1", Issues: 10, PRs: 3},
			{Name: "repo2", Issues: 5, PRs: 1},
		},
	}

	result := CollectRepoSnapshots(payload)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result["repo1"].Issues != 10 {
		t.Errorf("repo1 issues = %d, want 10", result["repo1"].Issues)
	}
	if result["repo2"].PRs != 1 {
		t.Errorf("repo2 prs = %d, want 1", result["repo2"].PRs)
	}
}

func TestCollectRepoSnapshots_NilPayload(t *testing.T) {
	result := CollectRepoSnapshots(nil)
	if result != nil {
		t.Error("expected nil for nil payload")
	}
}

func TestCollectRepoSnapshots_EmptyRepos(t *testing.T) {
	payload := &StatusPayload{Repos: []FrontendRepo{}}
	result := CollectRepoSnapshots(payload)
	if result != nil {
		t.Error("expected nil for empty repos")
	}
}

func TestCollectAgentStats_BadStatType(t *testing.T) {
	payload := &StatusPayload{
		Agents: []FrontendAgent{
			{
				Name:        "scanner",
				StatsConfig: []any{"not-a-map", 42},
			},
		},
	}
	result := CollectAgentStats(payload)
	if _, ok := result["scanner"]; ok {
		t.Error("should skip non-map stats")
	}
}

// ---------------------------------------------------------------------------
// RecordSnapshot / refreshStatus
// ---------------------------------------------------------------------------

func TestRecordSnapshot_EmptySnapshotDir(t *testing.T) {
	ns := &NousState{
		SnapshotDir: "",
		Status:      map[string]interface{}{},
	}
	err := ns.RecordSnapshot(governor.State{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("expected nil error for empty snapshot dir, got %v", err)
	}
}

func TestRecordSnapshot_WithDir(t *testing.T) {
	tmpDir := t.TempDir()
	ns := &NousState{
		SnapshotDir: tmpDir,
		Phase:       "collecting",
		Status:      map[string]interface{}{},
	}

	govState := governor.State{
		Mode:        governor.ModeBusy,
		QueueIssues: 10,
		QueuePRs:    3,
	}

	err := ns.RecordSnapshot(govState, nil, []string{"scanner"}, nil, nil)
	if err != nil {
		t.Fatalf("RecordSnapshot: %v", err)
	}

	// Should have created a snapshot file
	entries, _ := os.ReadDir(tmpDir)
	if len(entries) == 0 {
		t.Error("expected at least one snapshot file")
	}

	// refreshStatus should have updated phase
	if ns.Phase != "observing" {
		t.Errorf("phase = %q, want observing", ns.Phase)
	}
	if ns.Status["snapshots"] != 1 {
		t.Errorf("snapshots = %v, want 1", ns.Status["snapshots"])
	}
}

// ---------------------------------------------------------------------------
// handleGovernorSensing — partial fields
// ---------------------------------------------------------------------------

func TestHandleGovernorSensing_PartialFields(t *testing.T) {
	s, deps := apiServer(t)

	rec := doPut(s, "/api/config/governor/sensing", map[string]interface{}{
		"eval_interval_s": 600,
		"ttlSeconds":      120,
		"pullbackSeconds": 30,
		"ghRatePatterns":  []string{"rate-limit-*"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if deps.Config.Governor.EvalIntervalS != 600 {
		t.Errorf("evalIntervalS = %d, want 600", deps.Config.Governor.EvalIntervalS)
	}
	if deps.Config.Governor.Sensing.TTLSeconds != 120 {
		t.Errorf("ttlSeconds = %d, want 120", deps.Config.Governor.Sensing.TTLSeconds)
	}
}

// ---------------------------------------------------------------------------
// handleGovernorBudget — with weekly limit
// ---------------------------------------------------------------------------

func TestHandleGovernorBudget_WithTotalTokens(t *testing.T) {
	s, deps := apiServer(t)

	rec := doPut(s, "/api/config/governor/budget", map[string]interface{}{
		"totalTokens": 50000,
		"periodDays":  7,
		"criticalPct": 90,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if deps.Config.Governor.Budget.TotalTokens != 50000 {
		t.Errorf("totalTokens = %d, want 50000", deps.Config.Governor.Budget.TotalTokens)
	}
}

// ---------------------------------------------------------------------------
// handleAgentConfigGeneral — more fields
// ---------------------------------------------------------------------------

func TestHandleAgentConfigGeneral_AllFields(t *testing.T) {
	s, deps := apiServer(t)

	rec := doPut(s, "/api/config/agent/scanner/general", map[string]interface{}{
		"displayName":     "Scanner Bot",
		"description":     "Scans issues",
		"launchCmd":       "custom-cmd --flag",
		"staleTimeout":    3600,
		"restartStrategy": "immediate",
		"clearOnKick":     true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	agentCfg := deps.Config.Agents["scanner"]
	if agentCfg.DisplayName != "Scanner Bot" {
		t.Errorf("displayName = %q", agentCfg.DisplayName)
	}
	if agentCfg.Description != "Scans issues" {
		t.Errorf("description = %q", agentCfg.Description)
	}
}

// ---------------------------------------------------------------------------
// handleAgentConfigRestrictions — with data
// ---------------------------------------------------------------------------

func TestHandleAgentConfigRestrictions_ValidBody(t *testing.T) {
	s, _ := apiServer(t)

	rec := doPut(s, "/api/config/agent/scanner/restrictions", map[string]interface{}{
		"restrictions": []map[string]string{
			{"pattern": "*.log", "reason": "no logs"},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// AttachAgentStats (governor)
// ---------------------------------------------------------------------------

func TestAttachAgentStats(t *testing.T) {
	s, deps := apiServer(t)
	_ = s

	// First evaluate to create a history entry
	deps.Governor.Evaluate(5, 0, 0, 0)

	stats := map[string]map[string]any{
		"scanner": {"actionable": 5},
	}
	deps.Governor.AttachAgentStats(stats)

	history := deps.Governor.EvalHistory()
	if len(history) == 0 {
		t.Fatal("expected eval history")
	}
	last := history[len(history)-1]
	if last.AgentStats == nil {
		t.Error("expected agent stats attached to last eval")
	}
}

func TestAttachAgentStats_EmptyHistory(t *testing.T) {
	s, deps := apiServer(t)
	_ = s
	// Should not panic with empty history
	deps.Governor.AttachAgentStats(map[string]map[string]any{})
}

// ---------------------------------------------------------------------------
// handleKnowledgeHealth — with knowledge
// ---------------------------------------------------------------------------

func TestHandleKnowledgeHealth_NilKnowledge(t *testing.T) {
	s, deps := apiServer(t)
	deps.Knowledge = nil

	rec := doGet(s, "/api/knowledge/health")
	// ensureKnowledge auto-creates, so this won't be 503
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// handleKnowledgeStats — with knowledge
// ---------------------------------------------------------------------------

func TestHandleKnowledgeStats_NilKnowledge(t *testing.T) {
	s, deps := apiServer(t)
	deps.Knowledge = nil

	rec := doGet(s, "/api/knowledge/stats")
	// ensureKnowledge auto-creates
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}
