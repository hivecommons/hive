package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// ---------- helpers ----------

func isolateDashboardState(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	oldUserTokenPath := userTokenPath
	userTokenPath = filepath.Join(dir, "gh-user-token")
	t.Cleanup(func() { userTokenPath = oldUserTokenPath })
}

func testDeps(t *testing.T) *Dependencies {
	t.Helper()
	isolateDashboardState(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

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

	var refreshCalled atomic.Bool
	var persistCalled atomic.Bool

	_ = &refreshCalled
	_ = &persistCalled
	return &Dependencies{
		Config:   cfg,
		AgentMgr: mgr,
		Governor: gov,
		Logger:   logger,
		Ctx:      context.Background(),
		// Provider gateways: same real-provider adapters production wires in
		// cmd/hive, so handler tests exercise identical behavior (#5565).
		Watsonx:              testWatsonxGateway{},
		OpenRouter:           testOpenRouterGateway{},
		NewLinearAgent:       newTestLinearAgentFactory(logger, "", ""),
		LinearStoredViewerID: testLinearStoredViewerID,
		RefreshFunc:          func() { refreshCalled.Store(true) },
		PersistFunc:          func() { persistCalled.Store(true) },
	}
}

func apiServer(t *testing.T) (*Server, *Dependencies) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	deps := testDeps(t)
	s.RegisterAPI(deps)
	return s, deps
}

func doGet(s *Server, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	s.mux.ServeHTTP(rec, req)
	return rec
}

func doPost(s *Server, path string, body interface{}) *httptest.ResponseRecorder {
	var b bytes.Buffer
	json.NewEncoder(&b).Encode(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, &b)
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	return rec
}

func markOwnerRequest(req *http.Request) {
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set(ownerRoleVerifiedHeader, "true")
}

func doOwnerGet(s *Server, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	return rec
}

func doOwnerPost(s *Server, path string, body interface{}) *httptest.ResponseRecorder {
	var b bytes.Buffer
	json.NewEncoder(&b).Encode(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, &b)
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	return rec
}

func doPut(s *Server, path string, body interface{}) *httptest.ResponseRecorder {
	var b bytes.Buffer
	json.NewEncoder(&b).Encode(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, path, &b)
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	return rec
}

func doDelete(s *Server, path string, body interface{}) *httptest.ResponseRecorder {
	var b bytes.Buffer
	if body != nil {
		json.NewEncoder(&b).Encode(body)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, path, &b)
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var result map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v, body: %s", err, rec.Body.String())
	}
	return result
}

// ---------- tests ----------

func TestSetGitVersion(t *testing.T) {
	SetGitVersion("abc123full", "abc123f")
	if versionHash != "abc123full" {
		t.Errorf("versionHash = %q, want abc123full", versionHash)
	}
	if versionShort != "abc123f" {
		t.Errorf("versionShort = %q, want abc123f", versionShort)
	}
	// Reset
	SetGitVersion("unknown", "unknown")
}

func TestMaskSecret(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"ab", "••"},
		{"abcd", "••••"},
		{"abcde", "•abcde"[0:0] + "•" + "bcde"},
		{"secret-token-12345", "••••••••••••••2345"},
	}
	for _, tt := range tests {
		got := maskSecret(tt.input)
		if tt.input == "" && got != "" {
			t.Errorf("maskSecret(%q) = %q, want empty", tt.input, got)
		}
		if tt.input != "" && got == "" {
			t.Errorf("maskSecret(%q) = empty, want non-empty", tt.input)
		}
		// For inputs longer than 4 chars, last 4 chars should be visible
		if len(tt.input) > 4 {
			suffix := tt.input[len(tt.input)-4:]
			if !strings.HasSuffix(got, suffix) {
				t.Errorf("maskSecret(%q) = %q, expected suffix %q", tt.input, got, suffix)
			}
		}
	}
}

func TestHandleVersion(t *testing.T) {
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

func TestHandleConfig(t *testing.T) {
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

func TestHandleHistory(t *testing.T) {
	s, deps := apiServer(t)
	deps.Governor.Evaluate(5, 2, 0, 0)
	rec := doGet(s, "/api/history")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleTrends(t *testing.T) {
	s, deps := apiServer(t)
	deps.Governor.Evaluate(5, 2, 0, 0)
	tests := []struct {
		url string
	}{
		{"/api/trends"},
		{"/api/trends?range=day"},
		{"/api/trends?range=week"},
		{"/api/trends?hours=12"},
	}
	for _, tt := range tests {
		rec := doGet(s, tt.url)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", tt.url, rec.Code)
		}
	}
}

func TestHandleTimeline(t *testing.T) {
	s, deps := apiServer(t)
	deps.Governor.Evaluate(5, 2, 0, 0)
	deps.Governor.RecordKick("scanner")
	rec := doGet(s, "/api/timeline")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleWidget(t *testing.T) {
	s, deps := apiServer(t)
	deps.Governor.Evaluate(3, 1, 0, 0)
	rec := doGet(s, "/api/widget")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["issues"] == nil {
		t.Error("expected issues key in widget")
	}
}

func TestHandleTokens_NilCollector(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/tokens")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["status"] != "no_collector" {
		t.Errorf("expected no_collector status, got %v", result["status"])
	}
}

func TestHandleModelAdvisor(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/model-advisor")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["recommendation"] == nil {
		t.Error("expected recommendation in model-advisor")
	}
}

func TestHandleBudgetIgnore(t *testing.T) {
	s, _ := apiServer(t)

	// GET
	rec := doGet(s, "/api/budget-ignore")
	if rec.Code != http.StatusOK {
		t.Errorf("GET status = %d, want 200", rec.Code)
	}

	// POST
	rec = doPost(s, "/api/budget-ignore", map[string]interface{}{"ignored": []string{"scanner"}})
	if rec.Code != http.StatusOK {
		t.Errorf("POST status = %d, want 200", rec.Code)
	}
}

func TestHandleBudgetIgnoreSet_InvalidBody(t *testing.T) {
	s, _ := apiServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/budget-ignore", bytes.NewReader([]byte("not-json")))
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleGHAuth(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/gh-auth")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["type"] != "token" {
		t.Errorf("auth type = %v, want token", result["type"])
	}
}

func TestHandleGHAuth_AppType(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.GitHub.AppID = 12345
	rec := doGet(s, "/api/gh-auth")
	result := decodeJSON(t, rec)
	if result["type"] != "app" {
		t.Errorf("auth type = %v, want app", result["type"])
	}
}

func TestHandleSummaries_NilStatus(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/summaries")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleSummaries_WithStatus(t *testing.T) {
	s, _ := apiServer(t)
	s.UpdateStatus(&StatusPayload{
		Repos: []FrontendRepo{
			{Name: "repo1", ActionableIssues: []any{"i1"}, OpenPrs: []any{"p1"}},
		},
		Hold: FrontendHold{Items: []any{"h1"}},
	})
	rec := doGet(s, "/api/summaries")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleStatSources(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/config/stat-sources")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleBackends(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/config/backends")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// --- Sidebar ---
func TestHandleSidebar(t *testing.T) {
	s, _ := apiServer(t)

	rec := doGet(s, "/api/config/sidebar")
	if rec.Code != http.StatusOK {
		t.Errorf("GET sidebar status = %d", rec.Code)
	}

	rec = doPut(s, "/api/config/sidebar", map[string]interface{}{"panels": []string{"a"}})
	if rec.Code != http.StatusOK {
		t.Errorf("PUT sidebar status = %d", rec.Code)
	}
}

// --- Governor config ---
func TestHandleGovernorConfigGet(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["agents"] == nil {
		t.Error("expected agents key")
	}
}

func TestHandleGovernorSensing(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/governor/sensing", map[string]interface{}{"eval_interval_s": 60})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleGovernorThresholds(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/governor/thresholds", map[string]int{"quiet": 5})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleGovernorLabels(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/governor/labels", map[string]interface{}{"labels": []string{"hold"}})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleGovernorBudget(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/governor/budget", map[string]interface{}{"weekly_limit": 100000, "periodDays": 7, "criticalPct": 90})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleGovernorNotifications(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/governor/notifications",
		map[string]interface{}{"ntfyServer": "https://ntfy.sh", "ntfyTopic": "hive"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	// Verify ntfy config was set
	if s.deps.Config.Notifications.Ntfy == nil {
		t.Error("expected ntfy config to be set")
	}
}

func TestHandleGovernorNotifications_Discord(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/governor/notifications",
		map[string]interface{}{"discordWebhook": "https://discord.com/webhook"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleGovernorHealth(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/governor/health",
		map[string]interface{}{"healthcheckInterval": 120, "restartCooldown": 30, "modelLock": true})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleGovernorAddAgent(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/config/governor/agents",
		map[string]interface{}{"name": "newagent", "backend": "claude", "model": "haiku"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleGovernorAddAgent_Duplicate(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/config/governor/agents",
		map[string]interface{}{"name": "scanner", "backend": "claude"})
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
}

func TestHandleGovernorAddAgent_NoName(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/config/governor/agents", map[string]interface{}{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleGovernorAddAgent_DefaultBackend(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/config/governor/agents",
		map[string]interface{}{"name": "minimal"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleGovernorRemoveAgent(t *testing.T) {
	s, _ := apiServer(t)
	rec := doDelete(s, "/api/config/governor/agents/scanner", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleGovernorRemoveAgent_NotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doDelete(s, "/api/config/governor/agents/nonexistent", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleGovernorRepos(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/governor/repos",
		map[string]interface{}{"repos": []string{"repo1", "repo2"}, "primaryRepo": "repo1"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// --- Agent config ---

func TestHandleAgentConfigGet(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/config/agent/scanner")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["general"] == nil {
		t.Error("expected general key")
	}
}

func TestHandleAgentConfigGet_NotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/config/agent/nonexistent")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestHandleAgentConfigGet_MultilineDescription verifies that agents whose
// description contains literal newlines, tabs, and other control characters
// (as produced by YAML block scalars) still produce valid JSON in the config
// API response. Python's json.loads(strict=True) rejects unescaped control
// characters, so the JSON must contain \n / \t / \uXXXX escapes rather than
// raw bytes.
func TestHandleAgentConfigGet_MultilineDescription(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)

	multilineDesc := "Creates and auto-merges documentation PRs.\n" +
		"Reviews PRs on request.\n" +
		"\tIndented line with tab.\n" +
		"Line with carriage return.\r\n" +
		"Final line."

	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "myorg", Repos: []string{"repo1"}, AIAuthor: "bot"},
		GitHub:  config.GitHubConfig{Token: "tok"},
		Agents: map[string]config.AgentConfig{
			"scanner": {
				Backend:     "claude",
				Model:       "sonnet",
				Enabled:     true,
				Description: multilineDesc,
			},
		},
		Governor: config.GovernorConfig{
			EvalIntervalS: 300,
			Modes: map[string]config.ModeConfig{
				"busy": {Threshold: 10, Cadences: map[string]config.Cadence{"scanner": "5m"}},
			},
		},
	}

	gov := governor.New(cfg.Governor, cfg.Agents, logger)
	mgr := agent.NewManager(cfg.Agents, logger, agent.ProjectContext{})

	s.RegisterAPI(&Dependencies{
		Config:      cfg,
		AgentMgr:    mgr,
		Governor:    gov,
		Logger:      logger,
		Ctx:         context.Background(),
		RefreshFunc: func() {},
		PersistFunc: func() {},
	})

	rec := doGet(s, "/api/config/agent/scanner")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// The response body must be valid JSON — json.Decoder must not reject it.
	rawBody := rec.Body.Bytes()
	var parsed map[string]interface{}
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		t.Fatalf("response is not valid JSON: %v\nbody: %s", err, rawBody)
	}

	// Verify the description survived round-trip intact.
	general, ok := parsed["general"].(map[string]interface{})
	if !ok {
		t.Fatal("expected general section in response")
	}
	gotDesc, ok := general["description"].(string)
	if !ok {
		t.Fatal("expected description to be a string")
	}
	if gotDesc != multilineDesc {
		t.Errorf("description mismatch:\n  got:  %q\n  want: %q", gotDesc, multilineDesc)
	}

	// Additionally, verify no raw control characters (U+0000–U+001F) are
	// present in the JSON bytes except as part of valid escape sequences.
	// This catches the exact bug Python's json.loads(strict=True) rejects.
	inString := false
	escaped := false
	for i, b := range rawBody {
		if escaped {
			escaped = false
			continue
		}
		if b == '\\' && inString {
			escaped = true
			continue
		}
		if b == '"' {
			inString = !inString
			continue
		}
		if inString && b < 0x20 {
			t.Errorf("raw control character 0x%02x at byte offset %d inside JSON string", b, i)
		}
	}
}

func TestHandleAgentConfigGeneral(t *testing.T) {
	s, _ := apiServer(t)
	enabled := true
	clearOnKick := false
	rec := doPut(s, "/api/config/agent/scanner/general",
		map[string]interface{}{"enabled": &enabled, "clear_on_kick": &clearOnKick, "beads_dir": "/tmp/beads"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleAgentConfigGeneral_NotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/agent/nonexistent/general", map[string]interface{}{})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleAgentConfigModels(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/agent/scanner/models",
		map[string]interface{}{"backend": "copilot", "model": "gpt-4o"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleAgentConfigModels_NotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/agent/nonexistent/models",
		map[string]interface{}{"backend": "claude"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleAgentPrompt(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/config/agent/scanner/prompt")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleAgentPrompt_NotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/config/agent/nonexistent/prompt")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// --- Config stubs ---
// handleConfigStub tests removed — method was deleted as dead code in 95e94d3.

// --- Chat ---
func TestHandleChat(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/chat", map[string]interface{}{"query": "hello"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["status"] != "stub" {
		t.Errorf("status = %v, want stub", result["status"])
	}
}

func TestHandleChat_NoMessage(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/chat", map[string]interface{}{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// --- Nous ---
func TestHandleNous_NilState(t *testing.T) {
	s, _ := apiServer(t)

	endpoints := []string{"/api/nous/status", "/api/nous/ledger", "/api/nous/principles",
		"/api/nous/phase", "/api/nous/gate-pending", "/api/nous/gate-response", "/api/nous/config"}
	for _, ep := range endpoints {
		rec := doGet(s, ep)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", ep, rec.Code)
		}
	}
}

func TestHandleNous_WithState(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = &NousState{
		Mode:   "autonomous",
		Scope:  "repo",
		Phase:  "running",
		Status: map[string]interface{}{"active": true},
		Ledger: []map[string]interface{}{{"action": "test"}},
		Principles: []NousPrinciple{
			{ID: "p1", Text: "test principle", Confidence: 0.9, Source: "human"},
		},
		Config: map[string]interface{}{"goals": "test"},
	}

	rec := doGet(s, "/api/nous/status")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}

	rec = doGet(s, "/api/nous/phase")
	result := decodeJSON(t, rec)
	if result["phase"] != "running" {
		t.Errorf("phase = %v", result["phase"])
	}
}

func TestHandleNousApproveAbort(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/nous/approve", map[string]interface{}{})
	if rec.Code != http.StatusOK {
		t.Errorf("approve status = %d", rec.Code)
	}
	rec = doPost(s, "/api/nous/abort", map[string]interface{}{})
	if rec.Code != http.StatusOK {
		t.Errorf("abort status = %d", rec.Code)
	}
}

func TestHandleNousMode(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = &NousState{}
	rec := doPut(s, "/api/nous/mode", map[string]interface{}{"mode": "supervised"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	if deps.Nous.Mode != "supervised" {
		t.Errorf("mode = %q", deps.Nous.Mode)
	}
}

func TestHandleNousMode_Empty(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/nous/mode", map[string]interface{}{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleNousScope(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = &NousState{}
	rec := doPut(s, "/api/nous/scope", map[string]interface{}{"scope": "org"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleNousGateRespond(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = &NousState{}
	rec := doPost(s, "/api/nous/gate-respond", map[string]interface{}{"decision": "approve"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleNousConfigSection(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = &NousState{}
	rec := doPut(s, "/api/nous/config/goals", map[string]interface{}{"goal": "test"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleNousConfigSection_NilNous(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/nous/config/goals", map[string]interface{}{"goal": "test"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleNousDeletePrinciple(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = &NousState{
		Principles: []NousPrinciple{
			{ID: "p1", Text: "keep"},
			{ID: "p2", Text: "delete"},
		},
	}
	rec := doDelete(s, "/api/nous/principles/p2", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	if len(deps.Nous.Principles) != 1 {
		t.Errorf("principles len = %d, want 1", len(deps.Nous.Principles))
	}
}

func TestHandleNousDeletePrinciple_NilNous(t *testing.T) {
	s, _ := apiServer(t)
	rec := doDelete(s, "/api/nous/principles/p1", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// --- Knowledge ---
func TestHandleKnowledge_NilKnowledge(t *testing.T) {
	s, _ := apiServer(t)

	endpoints := []string{"/api/knowledge", "/api/knowledge/health", "/api/knowledge/stats",
		"/api/knowledge/subscriptions"}
	for _, ep := range endpoints {
		rec := doGet(s, ep)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", ep, rec.Code)
		}
	}
}

func TestHandleKnowledgeSearch_NilKnowledge(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/knowledge/search?q=test")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleKnowledgeFact_NilKnowledge(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/knowledge/project/test-slug")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleKnowledgeCreate_NilKnowledge(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/knowledge/create", map[string]interface{}{"title": "t", "body": "b"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleKnowledgeToggle(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Knowledge = config.KnowledgeConfig{Enabled: false}
	rec := doPut(s, "/api/knowledge/enabled", map[string]interface{}{"enabled": true})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleKnowledgeToggle_Disable(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Knowledge = config.KnowledgeConfig{Enabled: true}
	rec := doPut(s, "/api/knowledge/enabled", map[string]interface{}{"enabled": false})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// --- Helpers ---
func TestDecodeBody(t *testing.T) {
	var target struct{ Name string }
	body := bytes.NewReader([]byte(`{"name":"test"}`))
	req := httptest.NewRequest(http.MethodPost, "/", body)
	err := decodeBody(req, &target)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if target.Name != "test" {
		t.Errorf("name = %q", target.Name)
	}
}

func TestJsonResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	jsonResponse(rec, map[string]string{"key": "value"})
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestJsonError(t *testing.T) {
	rec := httptest.NewRecorder()
	jsonError(rec, "bad", http.StatusBadRequest)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestOkResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	okResponse(rec, map[string]string{"extra": "val"})
	var result map[string]interface{}
	json.NewDecoder(rec.Body).Decode(&result)
	if result["ok"] != true {
		t.Error("expected ok=true")
	}
	if result["extra"] != "val" {
		t.Error("expected extra=val")
	}
}

func TestRefreshAfterMutation(t *testing.T) {
	// Should not panic with nil deps
	s2 := &Server{}
	s2.refreshAfterMutation()
	s2.persistAfterMutation()
	s2.refreshAndPersist()
}

// --- Kick/Pause/Resume error paths ---
func TestHandleKick_NoMessage(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/kick/scanner", map[string]interface{}{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandlePause_NotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doOwnerPost(s, "/api/pause/nonexistent", map[string]interface{}{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandlePin_InvalidDimension(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/pin/scanner/invalid", map[string]interface{}{"value": "test"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleUnpin_InvalidDimension(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/unpin/scanner/invalid", map[string]interface{}{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleKnowledgeSearch_NoQuery(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/knowledge/search")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---------- Additional handler coverage ----------

func TestHandleSwitch(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/switch/scanner/gemini", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleSwitch_NotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/switch/nonexistent/gemini", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleModelSet(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/model/scanner/opus", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleModelSet_NotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/model/nonexistent/opus", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleResume(t *testing.T) {
	s, _ := apiServer(t)
	// Resume may fail because agent isn't actually running
	rec := doOwnerPost(s, "/api/resume/scanner", nil)
	// Accept OK or BadRequest (tmux not available)
	if rec.Code != http.StatusOK && rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleResume_NotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doOwnerPost(s, "/api/resume/nonexistent", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleRestart(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/restart/scanner", nil)
	// Will fail because tmux is not available, but tests the path
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleResetRestarts(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/reset-restarts/scanner", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleResetRestarts_NotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/reset-restarts/nonexistent", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandlePane(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/pane/scanner")
	// GetOutput may fail without tmux
	if rec.Code != http.StatusOK && rec.Code != http.StatusNotFound {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandlePane_WithLines(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/pane/scanner?lines=50")
	if rec.Code != http.StatusOK && rec.Code != http.StatusNotFound {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleAgentConfigCadences(t *testing.T) {
	s, _ := apiServer(t)
	body := map[string]int64{"idle": 900, "busy": 300}
	rec := doPut(s, "/api/config/agent/scanner/cadences", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleAgentConfigCadences_NotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/agent/nonexistent/cadences", map[string]int64{"idle": 900})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleAgentConfigCadences_Pause(t *testing.T) {
	s, _ := apiServer(t)
	body := map[string]int64{"idle": 0}
	rec := doPut(s, "/api/config/agent/scanner/cadences", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleAgentConfigCadences_StructuredTimesRoundTrip(t *testing.T) {
	s, _ := apiServer(t)
	body := map[string]any{"idle": map[string]any{
		"times": []string{"09:00", "17:00"},
		"days":  []string{"mon", "tue", "wed", "thu", "fri"},
		"tz":    "America/New_York",
	}}
	rec := doPut(s, "/api/config/agent/scanner/cadences", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = doGet(s, "/api/config/agent/scanner")
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	cadences := got["cadences"].(map[string]any)
	idle := cadences["idle"].(map[string]any)
	if idle["tz"] != "America/New_York" || len(idle["times"].([]any)) != 2 {
		t.Fatalf("idle cadence = %#v", idle)
	}
}

func TestHandleAgentConfigCadences_RejectsMutualExclusion(t *testing.T) {
	s, _ := apiServer(t)
	body := map[string]any{"idle": map[string]any{
		"interval": "5m",
		"times":    []string{"09:00"},
		"tz":       "UTC",
	}}
	rec := doPut(s, "/api/config/agent/scanner/cadences", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleAgentConfigPipeline(t *testing.T) {
	s, _ := apiServer(t)
	body := map[string]bool{"classify": true, "prime": false}
	rec := doPut(s, "/api/config/agent/scanner/pipeline", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleAgentConfigPipeline_NotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/agent/nonexistent/pipeline", map[string]bool{})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleAgentConfigHooks(t *testing.T) {
	s, _ := apiServer(t)
	body := map[string][]any{"pre_kick": {"echo hello"}}
	rec := doPut(s, "/api/config/agent/scanner/hooks", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleAgentConfigHooks_NotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/agent/nonexistent/hooks", map[string][]any{})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleAgentConfigRestrictions(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/agent/scanner/restrictions", map[string]any{
		"agent": []any{},
	})
	// May fail due to /data path on macOS
	if rec.Code != http.StatusOK && rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleAgentConfigRestrictions_NotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/agent/nonexistent/restrictions", map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleAgentConfigStats(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/agent/scanner/stats", map[string]any{
		"stats": []any{},
	})
	// May fail due to /data path on macOS
	if rec.Code != http.StatusOK && rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleAgentConfigStats_NotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/agent/nonexistent/stats", map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleKnowledgeDelete_NilKnowledge(t *testing.T) {
	s, deps := apiServer(t)
	deps.Knowledge = nil
	rec := doDelete(s, "/api/knowledge/project/test-slug", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleKnowledgePromote_NonexistentFact(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/knowledge/promote", map[string]string{
		"slug": "nonexistent", "from_layer": "project", "to_layer": "org",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleKnowledgePromote_MissingFields(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/knowledge/promote", map[string]string{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleKnowledgeImport_NilKnowledge(t *testing.T) {
	s, deps := apiServer(t)
	deps.Knowledge = nil
	rec := doPost(s, "/api/knowledge/import", map[string]string{"content": "test"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleKnowledgeSubsAdd_NilKnowledge(t *testing.T) {
	s, deps := apiServer(t)
	deps.Knowledge = nil
	rec := doPost(s, "/api/knowledge/subscriptions", map[string]string{"url": "http://example.com"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleKnowledgeSubsRemove_NilKnowledge(t *testing.T) {
	s, deps := apiServer(t)
	deps.Knowledge = nil
	rec := doDelete(s, "/api/knowledge/subscriptions", map[string]string{"name": "test-sub"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleKnowledgeUpdate_NilKnowledge(t *testing.T) {
	s, deps := apiServer(t)
	deps.Knowledge = nil
	rec := doPut(s, "/api/knowledge/project/test-slug", map[string]string{"body": "updated"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleKnowledgeLayer_AutoCreated(t *testing.T) {
	s, deps := apiServer(t)
	deps.Knowledge = nil
	rec := doGet(s, "/api/knowledge/project")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	result := decodeJSON(t, rec)
	if _, ok := result["facts"]; !ok {
		t.Error("expected facts key in response")
	}
}

func TestHandleNousGateDecision_NilNous(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = nil
	rec := doPost(s, "/api/nous/gate/decision", map[string]string{"decision": "approve"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleNousGateDecision_NoDecision(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = nil
	rec := doPost(s, "/api/nous/gate/decision", map[string]string{})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code) // nil nous => 404
	}
}

func TestHandleNousConfigRepos(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = nil
	rec := doPut(s, "/api/nous/config/repos", map[string]any{"value": []string{"repo1"}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleNousConfigOutput(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = nil
	rec := doPut(s, "/api/nous/config/output", map[string]any{"value": "json"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleNousConfigFastFail(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = nil
	rec := doPut(s, "/api/nous/config/fast_fail", map[string]any{"value": true})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleNousConfigSchedule(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = nil
	rec := doPut(s, "/api/nous/config/schedule", map[string]any{"value": "daily"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleNousConfigControllables(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = nil
	rec := doPut(s, "/api/nous/config/controllables", map[string]any{"value": []string{}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleNousConfigPrinciples(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = nil
	rec := doPut(s, "/api/nous/config/principles", map[string]any{"value": []string{}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleGovernorLabels_PUT(t *testing.T) {
	s, _ := apiServer(t)
	body := map[string]any{"exempt": []string{"hold", "wip"}}
	rec := doPut(s, "/api/config/governor/labels", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleGovernorBudget_PUT(t *testing.T) {
	s, _ := apiServer(t)
	body := map[string]any{"weekly_limit": 500000, "periodDays": 7, "criticalPct": 90}
	rec := doPut(s, "/api/config/governor/budget", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestFormatCadenceDuration(t *testing.T) {
	tests := []struct {
		seconds int64
		want    string
	}{
		{3600, "1h"},
		{7200, "2h"},
		{300, "5m"},
		{60, "1m"},
		{45, "45s"},
		{90, "90s"},
	}
	for _, tt := range tests {
		got := formatCadenceDuration(tt.seconds)
		if got != tt.want {
			t.Errorf("formatCadenceDuration(%d) = %q, want %q", tt.seconds, got, tt.want)
		}
	}
}

func TestSanitizeString(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain text unchanged",
			input: "my-agent",
			want:  "my-agent",
		},
		{
			name:  "script tag stripped",
			input: `<script>alert(1)</script>`,
			want:  "alert(1)",
		},
		{
			name:  "nested tags stripped",
			input: `<b><i>bold italic</i></b>`,
			want:  "bold italic",
		},
		{
			name:  "img tag with onerror stripped",
			input: `<img src=x onerror=alert(1)>`,
			want:  "",
		},
		{
			name:  "mixed content",
			input: `Agent <script>steal()</script> Name`,
			want:  "Agent steal() Name",
		},
		{
			name:  "whitespace trimmed",
			input: "  hello  ",
			want:  "hello",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "self-closing tag stripped",
			input: `test<br/>value`,
			want:  "testvalue",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeString(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeString(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
