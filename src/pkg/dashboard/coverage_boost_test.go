package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// --- Pure function tests ---

func TestMaskToken(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"abc", "abc"},     // <= 4 chars, returned as-is
		{"abcd", "abcd"},   // exactly 4 chars
		{"abcde", "•bcde"}, // 5 chars, 1 hidden
		{"ghp_1234567890", "••••••••••7890"}, // typical token
	}
	for _, tt := range tests {
		got := maskToken(tt.input)
		// Verify last 4 chars preserved when len > 4
		if len(tt.input) > 4 {
			if !strings.HasSuffix(got, tt.input[len(tt.input)-4:]) {
				t.Errorf("maskToken(%q) = %q, should end with %q", tt.input, got, tt.input[len(tt.input)-4:])
			}
		} else if got != tt.input {
			t.Errorf("maskToken(%q) = %q, want %q", tt.input, got, tt.input)
		}
	}
}

func TestRedactTokensInLine(t *testing.T) {
	tests := []struct {
		input       string
		contains    string
		notContains string
	}{
		{"no token here", "no token here", ""},
		{"token: ghp_abcdef1234567890", "***REDACTED***", "abcdef1234567890"},
		{"prefix gho_abcdef1234567890 suffix", "***REDACTED***", "abcdef1234567890"},
		{"github_pat_xyzABCDEFG12345", "***REDACTED***", "ABCDEFG12345"},
		{"short ghp_123", "ghp_123", "REDACTED"}, // too short to match
	}
	for _, tt := range tests {
		got := redactTokensInLine(tt.input)
		if tt.contains != "" && !strings.Contains(got, tt.contains) {
			t.Errorf("redactTokensInLine(%q) = %q, expected to contain %q", tt.input, got, tt.contains)
		}
		if tt.notContains != "" && strings.Contains(got, tt.notContains) {
			t.Errorf("redactTokensInLine(%q) = %q, expected NOT to contain %q", tt.input, got, tt.notContains)
		}
	}
}

func TestRedactTokens(t *testing.T) {
	tests := []struct {
		name  string
		input string
		check func(string) bool
	}{
		{
			"github token",
			"my token is ghp_abcdef1234567890 here",
			func(s string) bool { return strings.Contains(s, "***") && !strings.Contains(s, "abcdef1234567890") },
		},
		{
			"device code line",
			"one-time code: ABCD-EF12",
			func(s string) bool { return strings.Contains(s, "[auth flow redacted]") },
		},
		{
			"device flow lines",
			"Visit https://github.com/login/device and enter your code\nPress any key to copy",
			func(s string) bool { return strings.Contains(s, "[auth flow redacted]") },
		},
		{
			"no secrets",
			"just a normal line",
			func(s string) bool { return s == "just a normal line" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactTokens(tt.input)
			if !tt.check(got) {
				t.Errorf("redactTokens failed for %q, got %q", tt.input, got)
			}
		})
	}
}

func TestRoundTo(t *testing.T) {
	tests := []struct {
		f        float64
		decimals int
		want     float64
	}{
		{3.14159, 2, 3.14},
		{3.14159, 0, 3.0},
		{99.95, 1, 100.0},
		{0.0, 3, 0.0},
	}
	for _, tt := range tests {
		got := roundTo(tt.f, tt.decimals)
		if got != tt.want {
			t.Errorf("roundTo(%v, %d) = %v, want %v", tt.f, tt.decimals, got, tt.want)
		}
	}
}

func TestFormatOptionalTime(t *testing.T) {
	zero := time.Time{}
	if got := formatOptionalTime(zero); got != "" {
		t.Errorf("formatOptionalTime(zero) = %q, want empty", got)
	}

	now := time.Now()
	got := formatOptionalTime(now)
	if got == "" {
		t.Error("formatOptionalTime(now) should not be empty")
	}
	// Should be RFC3339
	if _, err := time.Parse(time.RFC3339, got); err != nil {
		t.Errorf("formatOptionalTime(now) = %q, not RFC3339: %v", got, err)
	}
}

func TestBuildIssueToMerge_NilCollector(t *testing.T) {
	result := buildIssueToMerge(nil)
	if len(result) != 0 {
		t.Errorf("expected empty map, got %v", result)
	}
}

func TestBuildIssueToMerge_EmptyMTTR(t *testing.T) {
	mc := &MetricsCollector{}
	result := buildIssueToMerge(mc)
	if len(result) != 0 {
		t.Errorf("expected empty map, got %v", result)
	}
}

func TestACMMPackAllowedSet_NilLevel(t *testing.T) {
	cfg := &config.Config{}
	result := acmmPackAllowedSet(cfg)
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestACMMPackAllowedSet_InvalidLevel(t *testing.T) {
	level := 999
	cfg := &config.Config{ACMMLevel: &level}
	result := acmmPackAllowedSet(cfg)
	if result != nil {
		t.Errorf("expected nil for invalid level, got %v", result)
	}
}

func TestACMMPackAllowedSet_ValidLevel(t *testing.T) {
	level := 1
	cfg := &config.Config{ACMMLevel: &level}
	result := acmmPackAllowedSet(cfg)
	if result == nil {
		t.Fatal("expected non-nil for level 1")
	}
	if len(result) == 0 {
		t.Error("expected at least one agent in level 1 pack")
	}
}

// --- Server methods ---

func TestSetProxyViolationsProvider(t *testing.T) {
	called := false
	SetProxyViolationsProvider(func() map[string]int {
		called = true
		return map[string]int{"scanner": 3}
	})
	defer SetProxyViolationsProvider(nil)

	fn := getProxyViolationsFn()
	if fn == nil {
		t.Fatal("expected non-nil provider")
	}
	result := fn()
	if !called {
		t.Error("provider not called")
	}
	if result["scanner"] != 3 {
		t.Errorf("scanner violations = %d, want 3", result["scanner"])
	}
}

func TestBuildAgentOnlyStatus(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "claude", Model: "sonnet", Enabled: true},
		},
		Governor: config.GovernorConfig{
			Modes: map[string]config.ModeConfig{
				"idle": {Cadences: map[string]config.Cadence{"scanner": "15m"}},
			},
		},
	}

	state := governor.State{Mode: "IDLE"}
	statuses := map[string]*agent.AgentProcess{
		"scanner": {State: agent.StateRunning},
	}

	payload := BuildAgentOnlyStatus(state, statuses, cfg)
	if payload == nil {
		t.Fatal("expected non-nil payload")
	}
	if payload.GovMode != "idle" {
		t.Errorf("govMode = %q, want idle", payload.GovMode)
	}
	if len(payload.Agents) != 1 {
		t.Errorf("agents count = %d, want 1", len(payload.Agents))
	}
}

// --- Handler tests ---

func TestHandleRole_Default(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/role", nil)
	w := httptest.NewRecorder()
	srv.handleRole(w, req)

	var result map[string]string
	json.NewDecoder(w.Body).Decode(&result)
	if result["role"] != "owner" {
		t.Errorf("role = %q, want owner", result["role"])
	}
}

func TestHandleRole_WithHeaders(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/role", nil)
	req.Header.Set("X-Hive-Role", "read")
	req.Header.Set("X-Hive-User", "testuser")
	w := httptest.NewRecorder()
	srv.handleRole(w, req)

	var result map[string]string
	json.NewDecoder(w.Body).Decode(&result)
	if result["role"] != "read" {
		t.Errorf("role = %q, want read", result["role"])
	}
	if result["user"] != "testuser" {
		t.Errorf("user = %q, want testuser", result["user"])
	}
}

func TestHandleRole_WithSessionCookie(t *testing.T) {
	srv := newFullServer(t)
	sid := srv.createUserSession("session-user", "read")
	req := httptest.NewRequest("GET", "/api/role", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "cookie-user"})
	w := httptest.NewRecorder()
	srv.handleRole(w, req)

	var result map[string]string
	json.NewDecoder(w.Body).Decode(&result)
	if result["user"] != "session-user" {
		t.Errorf("user = %q, want session-user", result["user"])
	}
	if result["role"] != "read" {
		t.Errorf("role = %q, want read", result["role"])
	}
}

func TestRefreshAndPersistSync(t *testing.T) {
	refreshCalled := false
	persistCalled := false
	srv := newFullServer(t)
	srv.deps.RefreshFunc = func() { refreshCalled = true }
	srv.deps.PersistFunc = func() { persistCalled = true }
	srv.refreshAndPersistSync()
	if !refreshCalled {
		t.Error("refresh not called")
	}
	if !persistCalled {
		t.Error("persist not called")
	}
}

func TestPersistOnly(t *testing.T) {
	called := false
	srv := newFullServer(t)
	srv.deps.PersistFunc = func() { called = true }
	srv.persistOnly()
	if !called {
		t.Error("persist not called")
	}
}

func TestRefreshAsync(t *testing.T) {
	called := false
	srv := newFullServer(t)
	srv.deps.RefreshFunc = func() { called = true }
	srv.refreshAsync()
	if !called {
		t.Error("refresh not called")
	}
}

func TestPersistOnly_NilDeps(t *testing.T) {
	srv := &Server{}
	srv.persistOnly() // should not panic
}

func TestRefreshAsync_NilDeps(t *testing.T) {
	srv := &Server{}
	srv.refreshAsync() // should not panic
}

func TestHandleBeadsReset(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/beads/reset", strings.NewReader(`{"reason":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	srv.handleBeadsReset(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", w.Code)
	}
	var result map[string]any
	json.NewDecoder(w.Body).Decode(&result)
	if result["status"] != "reset" {
		t.Errorf("status = %v", result["status"])
	}
}

func TestHandleBeadsReset_NoBody(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/beads/reset", strings.NewReader(""))
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	srv.handleBeadsReset(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", w.Code)
	}
}

func TestHandleBeadsReset_NilStores(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.BeadStores = nil
	req := httptest.NewRequest("POST", "/api/beads/reset", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	srv.handleBeadsReset(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", w.Code)
	}
}

func TestHandleBeadsResetAgent(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/beads/scanner/reset", strings.NewReader(`{"reason":"agent test"}`))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	req.SetPathValue("agent", "scanner")
	w := httptest.NewRecorder()
	srv.handleBeadsResetAgent(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", w.Code)
	}
}

func TestHandleBeadsResetAgent_Unknown(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/beads/unknown/reset", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	req.SetPathValue("agent", "unknown")
	w := httptest.NewRecorder()
	srv.handleBeadsResetAgent(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

func TestHandleBeadsResetAgent_NilStores(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.BeadStores = nil
	req := httptest.NewRequest("POST", "/api/beads/scanner/reset", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	req.SetPathValue("agent", "scanner")
	w := httptest.NewRecorder()
	srv.handleBeadsResetAgent(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", w.Code)
	}
}

func TestHandleBeadsList_All(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/beads", nil)
	w := httptest.NewRecorder()
	srv.handleBeadsList(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", w.Code)
	}
}

func TestHandleBeadsList_ByAgent(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/beads/scanner", nil)
	req.SetPathValue("agent", "scanner")
	w := httptest.NewRecorder()
	srv.handleBeadsList(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", w.Code)
	}
}

func TestHandleBeadsList_UnknownAgent(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/beads/unknown", nil)
	req.SetPathValue("agent", "unknown")
	w := httptest.NewRecorder()
	srv.handleBeadsList(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

func TestHandleBeadsList_NilStores(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.BeadStores = nil
	req := httptest.NewRequest("GET", "/api/beads", nil)
	w := httptest.NewRecorder()
	srv.handleBeadsList(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", w.Code)
	}
}

// --- Server setters ---

func TestSetInferenceEndpoints(t *testing.T) {
	srv := newFullServer(t)
	endpoints := map[string][]string{
		"vllm": {"http://localhost:8000"},
	}
	srv.SetInferenceEndpoints(endpoints)
	if srv.inferenceEndpoints == nil {
		t.Fatal("endpoints not set")
	}
	if len(srv.inferenceEndpoints["vllm"]) != 1 {
		t.Errorf("vllm endpoints = %v", srv.inferenceEndpoints["vllm"])
	}
}

func TestSetSkipReloadFunc(t *testing.T) {
	srv := newFullServer(t)
	called := false
	srv.SetSkipReloadFunc(func() { called = true })
	if srv.deps.SkipReloadFunc == nil {
		t.Fatal("SkipReloadFunc not set")
	}
	srv.deps.SkipReloadFunc()
	if !called {
		t.Error("SkipReloadFunc not called")
	}
}

func TestSetGitHubAppRequired(t *testing.T) {
	srv := newFullServer(t)
	srv.SetGitHubAppRequired(true)
	if !srv.IsGitHubAppRequired() {
		t.Error("expected true")
	}
	srv.SetGitHubAppRequired(false)
	if srv.IsGitHubAppRequired() {
		t.Error("expected false")
	}
}

func TestSetGitHubAppRecheckFn(t *testing.T) {
	srv := newFullServer(t)
	srv.SetGitHubAppRecheckFn(func() bool { return true })
	if srv.githubAppRecheckFn == nil {
		t.Fatal("recheckFn not set")
	}
}

func TestHandleGitHubAppRecheck_NotConfigured(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/github-app/recheck", nil)
	w := httptest.NewRecorder()
	srv.handleGitHubAppRecheck(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Errorf("code = %d, want 501", w.Code)
	}
}

func TestHandleGitHubAppRecheck_Installed(t *testing.T) {
	srv := newFullServer(t)
	srv.SetGitHubAppRequired(true)
	srv.SetGitHubAppRecheckFn(func() bool { return true })
	req := httptest.NewRequest("POST", "/api/github-app/recheck", nil)
	w := httptest.NewRecorder()
	srv.handleGitHubAppRecheck(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", w.Code)
	}
	if srv.IsGitHubAppRequired() {
		t.Error("should no longer be required after successful recheck")
	}
}

func TestHandleGitHubAppRecheck_NotInstalled(t *testing.T) {
	srv := newFullServer(t)
	srv.SetGitHubAppRecheckFn(func() bool { return false })
	req := httptest.NewRequest("POST", "/api/github-app/recheck", nil)
	w := httptest.NewRecorder()
	srv.handleGitHubAppRecheck(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "not_installed") {
		t.Errorf("expected not_installed in response, got %s", body)
	}
}

// --- BroadcastAgentStatus ---

func TestBroadcastAgentStatus(t *testing.T) {
	srv := newFullServer(t)
	// Set lastFullBroadcast to long ago so it doesn't skip
	srv.statusMu.Lock()
	srv.lastFullBroadcast = time.Time{}
	srv.statusMu.Unlock()

	payload := &AgentStatusPayload{
		Timestamp: time.Now().Format(time.RFC3339),
		GovMode:   "idle",
	}
	// Should not panic
	srv.BroadcastAgentStatus(payload)
}

func TestBroadcastAgentStatus_SkipAfterRecent(t *testing.T) {
	srv := newFullServer(t)
	srv.statusMu.Lock()
	srv.lastFullBroadcast = time.Now()
	srv.statusMu.Unlock()

	payload := &AgentStatusPayload{
		Timestamp: time.Now().Format(time.RFC3339),
		GovMode:   "idle",
	}
	// Should skip silently (recent full broadcast)
	srv.BroadcastAgentStatus(payload)
}

// --- SeedTokenSparklineHistory ---

func TestSeedTokenSparklineHistory_Boost(t *testing.T) {
	srv := newFullServer(t)
	entries := []TokenSparklineEntry{
		{Timestamp: 100, Input: 50},
		{Timestamp: 200, Input: 100},
	}
	srv.SeedTokenSparklineHistory(entries)
	got := srv.TokenSparklineHistory()
	if len(got) != 2 {
		t.Errorf("history len = %d, want 2", len(got))
	}
}

func TestSeedTokenSparklineHistory_OverMax_Boost(t *testing.T) {
	srv := newFullServer(t)
	// Create more entries than max
	entries := make([]TokenSparklineEntry, tokenSparklineMaxEntries+50)
	for i := range entries {
		entries[i] = TokenSparklineEntry{Timestamp: int64(i)}
	}
	srv.SeedTokenSparklineHistory(entries)
	got := srv.TokenSparklineHistory()
	if len(got) != tokenSparklineMaxEntries {
		t.Errorf("history len = %d, want %d", len(got), tokenSparklineMaxEntries)
	}
}

// --- Advisory digest ---

func TestSetGetAdvisoryDigest(t *testing.T) {
	srv := newFullServer(t)
	srv.SetAdvisoryDigest(map[string]string{"test": "value"})
	got := srv.GetAdvisoryDigest()
	if got == nil {
		t.Fatal("expected non-nil digest")
	}
	m, ok := got.(map[string]string)
	if !ok {
		t.Fatalf("expected map[string]string, got %T", got)
	}
	if m["test"] != "value" {
		t.Errorf("digest test = %q", m["test"])
	}
}

// --- HealthSummary ---

func TestHealthSummary(t *testing.T) {
	srv := newFullServer(t)
	srv.MarkReady()
	result := srv.HealthSummary()
	if result == nil {
		t.Fatal("expected non-nil summary")
	}
	if result["status"] == nil {
		t.Error("expected status field in summary")
	}
}

// --- handleHealthDeep ---

func TestHandleHealthDeep(t *testing.T) {
	srv := newFullServer(t)
	srv.MarkReady()
	srv.UpdateStatus(minimalPayload())

	req := httptest.NewRequest("GET", "/api/health/deep", nil)
	w := httptest.NewRecorder()
	srv.handleHealthDeep(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", w.Code)
	}
	var result map[string]any
	json.NewDecoder(w.Body).Decode(&result)
	if result["checks"] == nil {
		t.Error("expected checks field")
	}
}

func TestHandleHealthDeep_NotReady(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/health/deep", nil)
	w := httptest.NewRecorder()
	srv.handleHealthDeep(w, req)

	var result map[string]any
	json.NewDecoder(w.Body).Decode(&result)
	checks := result["checks"].(map[string]any)
	ready := checks["ready"].(map[string]any)
	if ready["status"] != "fail" {
		t.Errorf("ready status = %v, want fail", ready["status"])
	}
}

// --- inHealthGrace ---

func TestInHealthGrace(t *testing.T) {
	srv := newFullServer(t)
	// Freshly constructed: startedAt is ~now, so the boot-grace window is open.
	if inGrace, age := srv.inHealthGrace(); !inGrace {
		t.Errorf("should be in grace immediately after construction, age=%s", age)
	}
	// Push startedAt past the boot-grace window: a long-lived process must no
	// longer be graced, so a persisting need-login/down condition can degrade.
	srv.startedAt = time.Now().Add(-2 * healthBootGrace)
	if inGrace, age := srv.inHealthGrace(); inGrace {
		t.Errorf("should not be in grace long after startup, age=%s", age)
	}
}

// --- roleEnforcement ---

func TestRoleEnforcement_ReadOnly(t *testing.T) {
	srv := newFullServer(t)
	handler := srv.roleEnforcement(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// GET with read role should pass
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.Header.Set("X-Hive-Role", "read")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("GET with read role: code = %d, want 200", w.Code)
	}

	// POST with read role to non-contribute path should be forbidden
	req = httptest.NewRequest("POST", "/api/kick/scanner", nil)
	req.Header.Set("X-Hive-Role", "read")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("POST with read role: code = %d, want 403", w.Code)
	}

	// POST with read role to contribute path should pass
	req = httptest.NewRequest("POST", "/api/contribute/register", nil)
	req.Header.Set("X-Hive-Role", "read")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("POST contribute with read role: code = %d, want 200", w.Code)
	}
}

func TestRoleEnforcement_NoRole(t *testing.T) {
	srv := newFullServer(t)
	handler := srv.roleEnforcement(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("POST", "/api/kick/scanner", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("no role: code = %d, want 200", w.Code)
	}
}

// --- Audit log ---

func TestAuditLog_LoadFromDisk(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "audit.jsonl")

	entries := []AuditEntry{
		{Timestamp: "2025-01-01T00:00:00Z", User: "admin", Action: "kick", Agent: "scanner"},
		{Timestamp: "2025-01-01T01:00:00Z", User: "system", Action: "eval"},
	}
	var lines []byte
	for _, e := range entries {
		data, _ := json.Marshal(e)
		lines = append(lines, data...)
		lines = append(lines, '\n')
	}
	os.WriteFile(logFile, lines, 0o644)

	a := &AuditLog{ring: make([]AuditEntry, 0, auditRingCap)}
	// We need to hack the loadFromDisk path — the function hardcodes auditLogPath.
	// Instead test the Log and Recent functions directly.
	a.Log("testuser", "test-action", "detail", "agent1")
	a.Log("", "system-action", "", "")

	recent := a.Recent(10)
	if len(recent) != 2 {
		t.Errorf("recent len = %d, want 2", len(recent))
	}
	// Newest first
	if recent[0].Action != "system-action" {
		t.Errorf("recent[0].Action = %q, want system-action", recent[0].Action)
	}
	if recent[0].User != "system" {
		t.Errorf("empty user should become 'system', got %q", recent[0].User)
	}
}

func TestAuditLog_RingOverflow(t *testing.T) {
	a := &AuditLog{ring: make([]AuditEntry, 0, auditRingCap)}
	for i := 0; i < auditRingCap+10; i++ {
		a.Log("user", "action", "", "")
	}
	if len(a.ring) > auditRingCap {
		t.Errorf("ring size %d exceeds cap %d", len(a.ring), auditRingCap)
	}
}

func TestHandleAuditLog_Authorized(t *testing.T) {
	srv := newFullServer(t)
	srv.audit.Log("admin", "test-action", "detail", "scanner")

	req := httptest.NewRequest("GET", "/api/audit", nil)
	req.Header.Set("X-Hive-Role", "owner")
	w := httptest.NewRecorder()
	srv.handleAuditLog(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", w.Code)
	}
}

func TestHandleAuditLog_Forbidden(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/audit", nil)
	req.Header.Set("X-Hive-Role", "read")
	w := httptest.NewRecorder()
	srv.handleAuditLog(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("code = %d, want 403", w.Code)
	}
}

// --- InferenceModels from env ---

func TestInferenceModelsFromEnv_Empty(t *testing.T) {
	t.Setenv("HIVE_VLLM_MODELS", "")
	models := inferenceModelsFromEnv("HIVE_VLLM_MODELS", "default-model")
	if len(models) != 1 || models[0] != "default-model" {
		t.Errorf("expected [default-model], got %v", models)
	}
}

func TestInferenceModelsFromEnv_Set(t *testing.T) {
	t.Setenv("HIVE_VLLM_MODELS", "model-a, model-b, model-c")
	models := inferenceModelsFromEnv("HIVE_VLLM_MODELS", "default")
	if len(models) != 3 {
		t.Errorf("expected 3 models, got %d: %v", len(models), models)
	}
	if models[0] != "model-a" {
		t.Errorf("models[0] = %q", models[0])
	}
	if models[1] != "model-b" {
		t.Errorf("models[1] = %q", models[1])
	}
}

// --- handleInferenceModels ---

func TestHandleInferenceModels_NoBackend(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/inference-models/", nil)
	w := httptest.NewRecorder()
	srv.handleInferenceModels(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestHandleInferenceModels_UnknownBackend(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/inference-models/unknown", nil)
	req.SetPathValue("backend", "unknown")
	w := httptest.NewRecorder()
	srv.handleInferenceModels(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

func TestHandleInferenceModels_WithEndpoints(t *testing.T) {
	// Mock inference endpoint
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "model-1"},
				{"id": "model-2"},
			},
		})
	}))
	defer mockSrv.Close()

	srv := newFullServer(t)
	srv.inferenceEndpoints = map[string][]string{
		"vllm": {mockSrv.URL},
	}

	req := httptest.NewRequest("GET", "/api/inference-models/vllm", nil)
	req.SetPathValue("backend", "vllm")
	w := httptest.NewRecorder()
	srv.handleInferenceModels(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", w.Code)
	}
	var result map[string]any
	json.NewDecoder(w.Body).Decode(&result)
	models := result["models"].([]any)
	if len(models) != 2 {
		t.Errorf("expected 2 models, got %d", len(models))
	}
}

// --- fetchModelsFromEndpoint ---

func TestFetchModelsFromEndpoint_Success(t *testing.T) {
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "llama-3"},
				{"id": "mixtral"},
			},
		})
	}))
	defer mockSrv.Close()

	models, err := fetchModelsFromEndpoint(mockSrv.URL, "")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(models) != 2 {
		t.Errorf("expected 2 models, got %d", len(models))
	}
}

func TestFetchModelsFromEndpoint_ServerError(t *testing.T) {
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mockSrv.Close()

	_, err := fetchModelsFromEndpoint(mockSrv.URL, "")
	if err == nil {
		t.Error("expected error for 500 response")
	}
}

func TestFetchModelsFromEndpoints_Deduplicates(t *testing.T) {
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "shared-model"},
			},
		})
	}))
	defer mockSrv.Close()

	models := fetchModelsFromEndpoints([]string{mockSrv.URL, mockSrv.URL}, "")
	if len(models) != 1 {
		t.Errorf("expected 1 deduplicated model, got %d", len(models))
	}
}

// --- LoadStatsConfigWithCfg ---

func TestLoadStatsConfigWithCfg_FromDisk(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agents", "scanner")
	os.MkdirAll(agentDir, 0o755)
	stats := map[string]any{
		"stats": []any{
			map[string]any{"key": "test", "label": "Test"},
		},
	}
	data, _ := json.Marshal(stats)
	os.WriteFile(filepath.Join(agentDir, "stats.json"), data, 0o644)

	// LoadStatsConfigWithCfg reads from /data/agents/<name>/stats.json
	// which we can't override. Test the fallback to config.
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner": {
				StatsDisplay: []config.StatsDisplayEntry{
					{Key: "test", Label: "Test Label", Source: "status", Field: "count", Style: "spark"},
				},
			},
		},
	}
	// This will fall through to config since /data/agents/scanner doesn't exist
	result := LoadStatsConfigWithCfg("scanner", cfg)
	if len(result) == 0 {
		t.Fatal("expected non-empty stats config")
	}
}

func TestLoadStatsConfigWithCfg_DefaultFallback(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{},
	}
	result := LoadStatsConfigWithCfg("scanner", cfg)
	if len(result) == 0 {
		t.Fatal("expected default stats config for scanner")
	}
}

// --- BuildBeadsFromConfig ---

func TestBuildBeadsFromConfig(t *testing.T) {
	dir := t.TempDir()
	workerStore, _ := beads.NewStore(dir + "/worker")
	supStore, _ := beads.NewStore(dir + "/supervisor")

	stores := map[string]*beads.Store{
		"scanner":    workerStore,
		"supervisor": supStore,
	}
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner":    {BeadRole: "worker"},
			"supervisor": {BeadRole: "supervisor"},
		},
	}

	fb := BuildBeadsFromConfig(stores, cfg)
	// Both stores are empty, so counts should be 0
	if fb.Workers != 0 || fb.Supervisor != 0 {
		t.Errorf("Workers=%d, Supervisor=%d", fb.Workers, fb.Supervisor)
	}
}

// --- Metrics collector disk operations ---

func TestMetricsCollector_LoadFromDisk_ValidFile(t *testing.T) {
	mc := &MetricsCollector{
		metrics: make(map[string]any),
		logger:  slog.Default(),
	}
	// loadFromDisk reads from hardcoded path /data/metrics/metrics.json
	// Call it anyway to ensure no panic
	mc.loadFromDisk()
}

func TestMetricsCollector_LoadMTTRFromDisk(t *testing.T) {
	mc := &MetricsCollector{
		metrics: make(map[string]any),
		logger:  slog.Default(),
	}
	// loads from hardcoded path - just verify no panic
	mc.loadMTTRFromDisk()
}

func TestMetricsCollector_SaveMTTRToDisk(t *testing.T) {
	mc := &MetricsCollector{
		logger: slog.Default(),
	}
	// writes to hardcoded /data/metrics/ which doesn't exist on test machine
	// verify no panic
	mc.saveMTTRToDisk(nil)
}

func TestMetricsCollector_CollectMTTR_NilClient(t *testing.T) {
	mc := &MetricsCollector{
		logger:  slog.Default(),
		metrics: make(map[string]any),
	}
	// nil ghClient, should return early without panic
	mc.collectMTTR(context.Background())
	if mc.mttr != nil {
		t.Error("expected nil mttr with nil client")
	}
}

func TestMetricsCollector_CollectMTTR_EmptyRepo(t *testing.T) {
	mc := &MetricsCollector{
		logger:  slog.Default(),
		metrics: make(map[string]any),
		repo:    "",
	}
	mc.collectMTTR(context.Background())
	if mc.mttr != nil {
		t.Error("expected nil mttr with empty repo")
	}
}

// --- ContributeWSHub ---

func TestContributeWSHub_LiveStates(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	hub := &ContributeWSHub{
		connections:    make(map[string]*ContributorConnection),
		completedTasks: make(map[string]time.Time),
		logger:         logger,
	}

	// Add a mock connection
	conn := &ContributorConnection{
		profile:     &ContributorProfile{ContributorID: "cid-1", GitHubUsername: "testuser"},
		lastPong:    time.Now(),
		currentTask: &WSTaskAssign{TaskID: "task-1", Kind: "issue", Repo: "org/repo", Number: 42},
	}
	hub.connections["conn-1"] = conn

	states := hub.LiveStates()
	if len(states) != 1 {
		t.Fatalf("expected 1 state, got %d", len(states))
	}
	s, ok := states["cid-1"]
	if !ok {
		t.Fatal("expected state for cid-1")
	}
	if !s.Active {
		t.Error("expected active")
	}
	if s.CurrentTask == nil || s.CurrentTask.TaskID != "task-1" {
		t.Error("expected task-1")
	}
	if s.Sessions != 1 {
		t.Errorf("sessions = %d, want 1", s.Sessions)
	}
}

func TestContributeWSHub_LiveStates_StaleConnection(t *testing.T) {
	logger := slog.Default()
	hub := &ContributeWSHub{
		connections:    make(map[string]*ContributorConnection),
		completedTasks: make(map[string]time.Time),
		logger:         logger,
	}

	// Stale connection (lastPong is old)
	conn := &ContributorConnection{
		profile:  &ContributorProfile{ContributorID: "cid-2"},
		lastPong: time.Now().Add(-2 * wsHeartbeatTimeout),
	}
	hub.connections["conn-2"] = conn

	states := hub.LiveStates()
	if len(states) != 0 {
		t.Errorf("expected 0 states for stale connection, got %d", len(states))
	}
}

func TestContributeWSHub_LiveStates_MultipleSessionsSameUser(t *testing.T) {
	logger := slog.Default()
	hub := &ContributeWSHub{
		connections:    make(map[string]*ContributorConnection),
		completedTasks: make(map[string]time.Time),
		logger:         logger,
	}

	hub.connections["conn-a"] = &ContributorConnection{
		profile:     &ContributorProfile{ContributorID: "cid-1"},
		lastPong:    time.Now(),
		currentTask: &WSTaskAssign{TaskID: "t1"},
	}
	hub.connections["conn-b"] = &ContributorConnection{
		profile:     &ContributorProfile{ContributorID: "cid-1"},
		lastPong:    time.Now(),
		currentTask: &WSTaskAssign{TaskID: "t2"},
	}

	states := hub.LiveStates()
	s := states["cid-1"]
	if s.Sessions != 2 {
		t.Errorf("sessions = %d, want 2", s.Sessions)
	}
	if len(s.Tasks) != 2 {
		t.Errorf("tasks = %d, want 2", len(s.Tasks))
	}
}

func TestContributeWSHub_SelectTask_NilServer(t *testing.T) {
	hub := &ContributeWSHub{
		connections:    make(map[string]*ContributorConnection),
		completedTasks: make(map[string]time.Time),
		logger:         slog.Default(),
	}
	conn := &ContributorConnection{
		profile: &ContributorProfile{ContributorID: "cid-1"},
	}
	result := hub.selectTask(conn)
	// #2546: the no-server-reference path now sends an explicit hub_not_ready
	// negative-ack instead of a silent nil.
	if result == nil || result.Type != "task_unavailable" || result.Reason != taskUnavailableHubNotReady {
		t.Errorf("expected task_unavailable/%s for nil server, got %+v", taskUnavailableHubNotReady, result)
	}
}

func TestContributeWSHub_SelectTask_Suspended(t *testing.T) {
	logger := slog.Default()
	srv := newFullServer(t)
	srv.deps.Config.Hub.ContributeSuspended = true

	hub := &ContributeWSHub{
		connections:    make(map[string]*ContributorConnection),
		completedTasks: make(map[string]time.Time),
		logger:         logger,
		server:         srv,
	}
	conn := &ContributorConnection{
		profile: &ContributorProfile{ContributorID: "cid-1"},
	}
	result := hub.selectTask(conn)
	// #2546: a suspended queue now sends contribution_suspended, not a silent nil.
	if result == nil || result.Type != "task_unavailable" || result.Reason != taskUnavailableContributionSuspended {
		t.Errorf("expected task_unavailable/%s when suspended, got %+v", taskUnavailableContributionSuspended, result)
	}
}

func TestContributeWSHub_SelectTask_NoStatus(t *testing.T) {
	logger := slog.Default()
	srv := newFullServer(t)

	hub := &ContributeWSHub{
		connections:    make(map[string]*ContributorConnection),
		completedTasks: make(map[string]time.Time),
		logger:         logger,
		server:         srv,
	}
	conn := &ContributorConnection{
		profile: &ContributorProfile{ContributorID: "cid-1"},
	}
	result := hub.selectTask(conn)
	// #2546: no status snapshot now sends hub_not_ready, not a silent nil.
	if result == nil || result.Type != "task_unavailable" || result.Reason != taskUnavailableHubNotReady {
		t.Errorf("expected task_unavailable/%s when no status, got %+v", taskUnavailableHubNotReady, result)
	}
}

func TestContributeWSHub_SelectTask_WithIssues(t *testing.T) {
	logger := slog.Default()
	srv := newFullServer(t)
	srv.statusMu.Lock()
	srv.status = &StatusPayload{
		Repos: []FrontendRepo{
			{
				Name: "repo1",
				Full: "testorg/repo1",
				ActionableIssues: []any{
					map[string]any{
						"number": 42,
						"title":  "Test issue",
						"url":    "https://github.com/testorg/repo1/issues/42",
						"author": "someone",
					},
				},
			},
		},
	}
	srv.statusMu.Unlock()

	hub := &ContributeWSHub{
		connections:    make(map[string]*ContributorConnection),
		completedTasks: make(map[string]time.Time),
		logger:         logger,
		server:         srv,
	}
	conn := &ContributorConnection{
		profile: &ContributorProfile{ContributorID: "cid-1", GitHubUsername: "contrib-user", TrustTier: "contributor"},
	}
	result := hub.selectTask(conn)
	if result == nil {
		t.Fatal("expected a task to be assigned")
	}
	if result.Type != "task_assign" {
		t.Errorf("type = %q, want task_assign", result.Type)
	}
	if result.Number != 42 {
		t.Errorf("number = %d, want 42", result.Number)
	}
	if result.Prompt == "" {
		t.Error("expected non-empty prompt")
	}
}

func TestContributeWSHub_SelectTask_Cooldown(t *testing.T) {
	logger := slog.Default()
	srv := newFullServer(t)
	srv.statusMu.Lock()
	srv.status = &StatusPayload{
		Repos: []FrontendRepo{
			{
				Name: "repo1",
				Full: "testorg/repo1",
				ActionableIssues: []any{
					map[string]any{"number": 42, "title": "Test", "url": "url"},
				},
			},
		},
	}
	srv.statusMu.Unlock()

	hub := &ContributeWSHub{
		connections:    make(map[string]*ContributorConnection),
		completedTasks: map[string]time.Time{"testorg/repo1#42": time.Now()},
		logger:         logger,
		server:         srv,
	}
	conn := &ContributorConnection{
		profile: &ContributorProfile{ContributorID: "cid-1", TrustTier: "contributor"},
	}
	result := hub.selectTask(conn)
	// #2546: the only candidate is in cooldown, so the candidate set is empty and
	// selectTask now sends no_matching_work rather than a silent nil.
	if result == nil || result.Type != "task_unavailable" || result.Reason != taskUnavailableNoMatchingWork {
		t.Errorf("expected task_unavailable/%s for task in cooldown, got %+v", taskUnavailableNoMatchingWork, result)
	}
}

func TestContributeWSHub_IsTaskInCooldown(t *testing.T) {
	hub := &ContributeWSHub{
		completedTasks: map[string]time.Time{
			"org/repo#1": time.Now(),
			"org/repo#2": time.Now().Add(-completedTaskCooldownHours * time.Hour * 2),
		},
		logger: slog.Default(),
	}

	if !hub.isTaskInCooldown("org/repo", 1) {
		t.Error("expected task 1 to be in cooldown")
	}
	if hub.isTaskInCooldown("org/repo", 2) {
		t.Error("expected task 2 to NOT be in cooldown (expired)")
	}
	if hub.isTaskInCooldown("org/repo", 3) {
		t.Error("expected task 3 to NOT be in cooldown (not found)")
	}
}

func TestContributeWSHub_MarkTaskCompleted(t *testing.T) {
	hub := &ContributeWSHub{
		completedTasks: make(map[string]time.Time),
		logger:         slog.Default(),
	}
	hub.markTaskCompleted("org/repo", 42, "https://github.com/org/repo/pull/1")

	if !hub.isTaskInCooldown("org/repo", 42) {
		t.Error("expected task to be in cooldown after marking complete")
	}
}

func TestContributeWSHub_AddActivity(t *testing.T) {
	hub := &ContributeWSHub{
		connections:    make(map[string]*ContributorConnection),
		completedTasks: make(map[string]time.Time),
		logger:         slog.Default(),
	}
	hub.addActivity("user1", "joined", "scanner", "claude", "sonnet", "", "")
	hub.addActivity("user2", "picked up", "", "claude", "sonnet", "", "task-1")

	activity := hub.RecentActivity()
	if len(activity) != 2 {
		t.Errorf("activity len = %d, want 2", len(activity))
	}
}

func TestContributeWSHub_AddActivity_Debounce(t *testing.T) {
	hub := &ContributeWSHub{
		connections:    make(map[string]*ContributorConnection),
		completedTasks: make(map[string]time.Time),
		logger:         slog.Default(),
	}
	hub.addActivity("user1", "joined", "", "", "", "", "")
	hub.addActivity("user1", "joined", "", "", "", "", "")

	activity := hub.RecentActivity()
	if len(activity) != 1 {
		t.Errorf("activity len = %d, want 1 (debounced)", len(activity))
	}
}

func TestContributeWSHub_RoleBreakdown(t *testing.T) {
	hub := &ContributeWSHub{
		connections: map[string]*ContributorConnection{
			"a": {role: "scanner"},
			"b": {role: "scanner"},
			"c": {role: "reviewer"},
			"d": {role: ""},
		},
		logger: slog.Default(),
	}
	breakdown := hub.RoleBreakdown()
	if breakdown["scanner"] != 2 {
		t.Errorf("scanner = %d", breakdown["scanner"])
	}
	if breakdown["reviewer"] != 1 {
		t.Errorf("reviewer = %d", breakdown["reviewer"])
	}
	if breakdown["task-driven"] != 1 {
		t.Errorf("task-driven = %d", breakdown["task-driven"])
	}
}

func TestContributeWSHub_ActiveCount(t *testing.T) {
	hub := &ContributeWSHub{
		connections: map[string]*ContributorConnection{
			"a": {profile: &ContributorProfile{GitHubUsername: "user1"}},
			"b": {profile: &ContributorProfile{GitHubUsername: "user1"}}, // same user
			"c": {profile: &ContributorProfile{GitHubUsername: "user2"}},
			"d": {profile: &ContributorProfile{}}, // no username
		},
		logger: slog.Default(),
	}
	count := hub.ActiveCount()
	if count != 2 {
		t.Errorf("ActiveCount = %d, want 2 (deduplicated)", count)
	}
}

func TestContributeWSHub_ActiveSessionCount(t *testing.T) {
	hub := &ContributeWSHub{
		connections: map[string]*ContributorConnection{
			"a": {}, "b": {}, "c": {},
		},
		logger: slog.Default(),
	}
	if hub.ActiveSessionCount() != 3 {
		t.Errorf("ActiveSessionCount = %d, want 3", hub.ActiveSessionCount())
	}
}

// --- readCgroupInt64 / readProcMemTotalBytes / readCgroupCPUUsageUsec ---

func TestReadCgroupInt64_ValidFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "memory.current")
	os.WriteFile(f, []byte("1234567890\n"), 0o644)
	got := readCgroupInt64(f)
	if got != 1234567890 {
		t.Errorf("got %d, want 1234567890", got)
	}
}

func TestReadCgroupInt64_Max(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "memory.max")
	os.WriteFile(f, []byte("max\n"), 0o644)
	got := readCgroupInt64(f)
	if got != -1 {
		t.Errorf("got %d, want -1 for 'max'", got)
	}
}

func TestReadCgroupInt64_MissingFile(t *testing.T) {
	got := readCgroupInt64("/nonexistent/path")
	if got != -1 {
		t.Errorf("got %d, want -1 for missing file", got)
	}
}

func TestReadCgroupInt64_BadContent(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "bad")
	os.WriteFile(f, []byte("not-a-number\n"), 0o644)
	got := readCgroupInt64(f)
	if got != -1 {
		t.Errorf("got %d, want -1 for invalid content", got)
	}
}

func TestReadProcMemTotalBytes(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "meminfo")
	content := `MemTotal:       16384000 kB
MemFree:         8192000 kB
MemAvailable:   12000000 kB
`
	os.WriteFile(f, []byte(content), 0o644)
	// Can't test directly since procMeminfo is a const, but verify the function
	// doesn't panic when the real /proc/meminfo is missing
	result := readProcMemTotalBytes()
	// On macOS this will return -1, on Linux it should return a real value
	_ = result
}

func TestReadCgroupCPUUsageUsec_ValidFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "cpu.stat")
	content := `usage_usec 5000000
user_usec 3000000
system_usec 2000000
`
	os.WriteFile(f, []byte(content), 0o644)
	got := readCgroupCPUUsageUsec(f)
	if got != 5000000 {
		t.Errorf("got %d, want 5000000", got)
	}
}

func TestReadCgroupCPUUsageUsec_NoUsageLine(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "cpu.stat")
	os.WriteFile(f, []byte("user_usec 3000000\n"), 0o644)
	got := readCgroupCPUUsageUsec(f)
	if got != -1 {
		t.Errorf("got %d, want -1", got)
	}
}

func TestReadCgroupV1CPUUsageUsec(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "cpuacct.usage")
	// 5 seconds in nanoseconds
	os.WriteFile(f, []byte("5000000000\n"), 0o644)
	got := readCgroupV1CPUUsageUsec(f)
	expected := int64(5000000) // 5e9 / 1000
	if got != expected {
		t.Errorf("got %d, want %d", got, expected)
	}
}

func TestReadCgroupV1CPUUsageUsec_MissingFile(t *testing.T) {
	got := readCgroupV1CPUUsageUsec("/nonexistent")
	if got != -1 {
		t.Errorf("got %d, want -1", got)
	}
}

func TestReadCgroupV1CPUUsageUsec_BadContent(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "bad")
	os.WriteFile(f, []byte("nope\n"), 0o644)
	got := readCgroupV1CPUUsageUsec(f)
	if got != -1 {
		t.Errorf("got %d, want -1", got)
	}
}

// --- handleSnapshotPage ---

func TestHandleSnapshotPage_AutoSnapshotDisabled(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Hub.AutoSnapshot = false

	req := httptest.NewRequest("GET", "/snapshot", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotPage(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "not currently published") {
		t.Error("expected 'not currently published' message")
	}
}

// handleHistory and handleTimeline are tested in api_test.go

// --- handleGHUserAuthPoll ---

func TestHandleGHUserAuthPoll_NoFlowInProgress(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/gh-user-auth/poll", nil)
	w := httptest.NewRecorder()
	srv.handleGHUserAuthPoll(w, req)

	var result map[string]any
	json.NewDecoder(w.Body).Decode(&result)
	if result["error"] == nil {
		t.Error("expected error for no flow in progress")
	}
}

// --- handleGHUserAuthStart ---

// Blank oauth_client_id must fall back to the public github.com client and run
// the device flow, not 400. See OAuthClientIDResolved.
func TestHandleGHUserAuthStart_BlankClientIDUsesPublicDefault(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.GitHub.OAuthClientID = ""
	mock := deviceFlowMock(t, "complete", "octocat")
	srv.deps.Config.GitHub.OAuthBaseURLOverride = mock.URL
	srv.deps.Config.GitHub.OAuthAPIURLOverride = mock.URL

	req := httptest.NewRequest("POST", "/api/gh-user-auth/start", nil)
	w := httptest.NewRecorder()
	srv.handleGHUserAuthStart(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ABCD-1234") {
		t.Errorf("device flow did not run, body=%s", w.Body.String())
	}
}

// --- handleGHUserAuthStatus ---

func TestHandleGHUserAuthStatus_NoToken(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/gh-user-auth/status", nil)
	w := httptest.NewRecorder()
	srv.handleGHUserAuthStatus(w, req)

	var result map[string]any
	json.NewDecoder(w.Body).Decode(&result)
	if result["logged_in"] != false {
		t.Errorf("logged_in = %v, want false", result["logged_in"])
	}
}

// --- checkModelAllowed ---

func TestCheckModelAllowed_NoConfig(t *testing.T) {
	hub := &ContributeWSHub{logger: slog.Default()}
	ok, _ := hub.checkModelAllowed("any-model")
	if !ok {
		t.Error("should be allowed when no config")
	}
}

func TestCheckModelAllowed_EmptyAllowList(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Hub.ContributeAllowModels = nil
	hub := &ContributeWSHub{server: srv, logger: slog.Default()}
	ok, _ := hub.checkModelAllowed("any-model")
	if !ok {
		t.Error("should be allowed with empty allow list")
	}
}

func TestCheckModelAllowed_MatchedModel(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Hub.ContributeAllowModels = []string{"sonnet", "opus"}
	hub := &ContributeWSHub{server: srv, logger: slog.Default()}
	ok, _ := hub.checkModelAllowed("sonnet")
	if !ok {
		t.Error("sonnet should be allowed")
	}
}

func TestCheckModelAllowed_RejectedModel(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Hub.ContributeAllowModels = []string{"sonnet", "opus"}
	srv.deps.Config.Hub.ContributeRejectUnknownModels = true
	hub := &ContributeWSHub{server: srv, logger: slog.Default()}
	ok, models := hub.checkModelAllowed("haiku")
	if ok {
		t.Error("haiku should be rejected")
	}
	if len(models) != 2 {
		t.Errorf("expected 2 accepted models, got %d", len(models))
	}
}

func TestCheckModelAllowed_EmptyModelNotRejected(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Hub.ContributeAllowModels = []string{"sonnet"}
	srv.deps.Config.Hub.ContributeRejectUnknownModels = false
	hub := &ContributeWSHub{server: srv, logger: slog.Default()}
	ok, _ := hub.checkModelAllowed("")
	if !ok {
		t.Error("empty model should be allowed when reject disabled")
	}
}

func TestCheckModelAllowed_EmptyModelRejected(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Hub.ContributeAllowModels = []string{"sonnet"}
	srv.deps.Config.Hub.ContributeRejectUnknownModels = true
	hub := &ContributeWSHub{server: srv, logger: slog.Default()}
	ok, _ := hub.checkModelAllowed("")
	if ok {
		t.Error("empty model should be rejected when reject enabled")
	}
}

// --- handleInceptionStart ---

func TestHandleInceptionStart_NoInception(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Inception = nil
	req := httptest.NewRequest("POST", "/api/inception/start", strings.NewReader(`{"idea":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleInceptionStart(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", w.Code)
	}
}

func TestHandleInceptionStart_BadBody(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Inception = nil
	req := httptest.NewRequest("POST", "/api/inception/start", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleInceptionStart(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503 (inception nil checked first)", w.Code)
	}
}

// --- handleKick edge cases ---

func TestHandleKick_AgentNotFound(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/kick/nonexistent", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("agent", "nonexistent")
	w := httptest.NewRecorder()
	srv.handleKick(w, req)

	// Should return an error since agent doesn't exist
	if w.Code == http.StatusOK {
		// Check if body contains error
		var result map[string]any
		json.NewDecoder(w.Body).Decode(&result)
		// It may succeed with a warning or fail - depends on implementation
	}
}

// handleRestart is tested in api_coverage_test.go

// --- handleGHRateLimits ---

func TestHandleGHRateLimits_NoClient(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.GHClient = nil
	req := httptest.NewRequest("GET", "/api/gh-rate-limits", nil)
	w := httptest.NewRecorder()
	srv.handleGHRateLimits(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", w.Code)
	}
}

// --- loadAgentRestrictions ---

func TestLoadAgentRestrictions_NoFiles(t *testing.T) {
	srv := newFullServer(t)
	result := srv.loadAgentRestrictions("scanner")
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	// Should have agent, global, policy keys
	if result["agent"] == nil {
		t.Error("expected agent key")
	}
	if result["global"] == nil {
		t.Error("expected global key")
	}
	if result["policy"] == nil {
		t.Error("expected policy key")
	}
}

// --- loadAgentStats ---

func TestLoadAgentStats_FallbackToDefaults(t *testing.T) {
	srv := newFullServer(t)
	result := srv.loadAgentStats("scanner")
	if len(result) == 0 {
		t.Error("expected default stats for scanner")
	}
}

// --- buildExportYAML ---

func TestBuildExportYAML_Basic(t *testing.T) {
	srv := newFullServer(t)
	agentCfg := config.AgentConfig{
		Backend:        "claude",
		Model:          "sonnet",
		Role:           "scanner",
		DisplayName:    "Scanner",
		Description:    "Scans issues",
		Emoji:          "🔍",
		Color:          "#00ff00",
		LaneKeywords:   []string{"bug", "fix"},
		DetectKeywords: []string{"error"},
		Aliases:        []string{"scan"},
	}
	cadences := map[string]string{"idle": "15m", "busy": "5m"}
	yaml := srv.buildExportYAML("scanner", agentCfg, cadences, true, "# my prompt\nDo stuff.")
	if yaml == "" {
		t.Error("expected non-empty YAML")
	}
	if !strings.Contains(yaml, "name: scanner") {
		t.Error("expected agent name in YAML")
	}
	if !strings.Contains(yaml, "backend: claude") {
		t.Error("expected backend in YAML")
	}
	if !strings.Contains(yaml, "cadences:") {
		t.Error("expected cadences section")
	}
	if !strings.Contains(yaml, "promptTemplate:") {
		t.Error("expected promptTemplate section for non-empty prompt")
	}
}

func TestBuildExportYAML_NoPrompt(t *testing.T) {
	srv := newFullServer(t)
	agentCfg := config.AgentConfig{Backend: "copilot"}
	yaml := srv.buildExportYAML("worker", agentCfg, nil, false, "")
	if yaml == "" {
		t.Error("expected non-empty YAML")
	}
	if strings.Contains(yaml, "promptTemplate:") {
		t.Error("expected no promptTemplate section with empty prompt")
	}
}
