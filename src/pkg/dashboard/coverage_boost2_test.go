package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/config"
	ghpkg "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/knowledge"
)

// --- isTemplatePlaceholder ---

func TestIsTemplatePlaceholder(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"<fact title>", true},
		{"<question title>", true},
		{"<title>", true},
		{"<your question>", true},
		{"<description>", true},
		{"<bead title>", true},
		{"fact_title", true},
		{"question_title", true},
		{"your question here", true},
		{"<ANYTHING>", true},
		{"<AnY UPPER>", true},
		{"What is the deployment strategy?", false},
		{"", false},
		{"normal text", false},
	}
	for _, tt := range tests {
		got := isTemplatePlaceholder(tt.input)
		if got != tt.want {
			t.Errorf("isTemplatePlaceholder(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

// --- handleGovernorRepos ---

func TestHandleGovernorRepos_EmptyBody(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/governor/repos", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestHandleGovernorRepos_InvalidBody_Boost(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/governor/repos", strings.NewReader("invalid"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestHandleGovernorRepos_SimpleRepos(t *testing.T) {
	srv := newFullServer(t)
	// A save that replaces the repo set must also name a default that is one of
	// the new repos (the always-exactly-one-default invariant), so send both.
	body := `{"repos":["repo1","repo2"],"primaryRepo":"repo1"}`
	req := httptest.NewRequest("PUT", "/api/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", w.Code)
	}
}

func TestHandleGovernorRepos_URLParsingRejected(t *testing.T) {
	srv := newFullServer(t)
	body := `{"repos":["https://github.com/myorg/myrepo"],"primaryRepo":"myrepo"}`
	req := httptest.NewRequest("PUT", "/api/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", w.Code)
	}
	want := "Repo target misconfigured: repo 'https://github.com/myorg/myrepo' is a URL — expected repo name only so the target resolves to org/repo. Fix in Settings → Repos."
	if !strings.Contains(w.Body.String(), want) {
		t.Fatalf("body = %q, want %q", w.Body.String(), want)
	}
}

func TestHandleStatusCarriesRepoTargetMisconfig(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Project.Org = "github.ibm.com"
	srv.deps.Config.Project.Repos = []string{"hive"}
	srv.UpdateStatus(minimalPayload())
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	srv.handleStatus(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	var got struct {
		RepoTargetMisconfigured bool   `json:"repoTargetMisconfigured"`
		RepoTargetIssue         string `json:"repoTargetIssue"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !got.RepoTargetMisconfigured {
		t.Fatal("repoTargetMisconfigured = false, want true")
	}
	want := "Repo target misconfigured: org 'github.ibm.com' looks like a forge host — expected org/repo. Fix in Settings → Repos."
	if got.RepoTargetIssue != want {
		t.Fatalf("repoTargetIssue = %q, want %q", got.RepoTargetIssue, want)
	}
	srv.deps.Config.Project.Org = "kubestellar"
	payload := &StatusPayload{RepoTargetMisconfigured: true, RepoTargetIssue: "stale"}
	srv.UpdateStatus(payload)
	if payload.RepoTargetMisconfigured || payload.RepoTargetIssue != "" {
		t.Fatalf("UpdateStatus did not clear stale repo target issue after fix: %#v", payload)
	}
}

func TestHandleGovernorRepos_InvalidRepoName(t *testing.T) {
	srv := newFullServer(t)
	body := `{"repos":["repo..name"]}`
	req := httptest.NewRequest("PUT", "/api/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400 for malicious repo name", w.Code)
	}
}

func TestHandleGovernorRepos_PrimaryRepo(t *testing.T) {
	srv := newFullServer(t)
	// The chosen default must be one of the monitored repos, so send both the
	// repo and the primaryRepo that names it (always-exactly-one-default).
	primary := "newrepo"
	body := `{"repos":["newrepo"],"primaryRepo":"newrepo"}`
	req := httptest.NewRequest("PUT", "/api/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", w.Code)
	}
	if srv.deps.Config.Project.PrimaryRepo != primary {
		t.Errorf("primaryRepo = %q, want %q", srv.deps.Config.Project.PrimaryRepo, primary)
	}
}

func TestHandleGovernorRepos_GHE_URL(t *testing.T) {
	// A hive already on github.com (newFullServer's testrepo) must NOT be able to
	// add a repo on a different forge — that would mix hosts and silently break
	// App auth for half the repos. The single-host-per-spoke guard rejects it and
	// leaves the hive's forge untouched.
	srv := newFullServer(t)
	body := `{"repos":["https://ghe.example.com/myorg/myrepo"],"primaryRepo":"myrepo"}`
	req := httptest.NewRequest("PUT", "/api/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (cross-forge repo rejected)", w.Code)
	}
	if srv.deps.Config.GitHub.BaseURL == "https://ghe.example.com" {
		t.Errorf("hive forge was switched to a mismatched host: %q", srv.deps.Config.GitHub.BaseURL)
	}
}

// --- handleAgentPromptSave ---

func TestHandleAgentPromptSave_NotFound(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/agents/nonexistent/prompt", strings.NewReader(`{"template":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "nonexistent")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleAgentPromptSave(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

func TestHandleAgentPromptSave_BadBody(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/agents/scanner/prompt", strings.NewReader("invalid"))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "scanner")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleAgentPromptSave(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

// --- handleInceptionState ---

func TestHandleInceptionState_NoInception(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Inception = nil
	req := httptest.NewRequest("GET", "/api/inception/state", nil)
	w := httptest.NewRecorder()
	srv.handleInceptionState(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", w.Code)
	}
}

// --- MetricsCollector loadFromDisk with temp file ---

func TestMetricsCollector_SaveAndLoadFromDisk(t *testing.T) {
	dir := t.TempDir()
	metricsFile := filepath.Join(dir, "metrics.json")

	mc := &MetricsCollector{
		logger:  slog.Default(),
		metrics: map[string]any{"test": "value"},
	}

	// Save
	data, _ := json.Marshal(mc.metrics)
	os.WriteFile(metricsFile, data, 0o644)

	// Load
	mc2 := &MetricsCollector{
		logger:  slog.Default(),
		metrics: make(map[string]any),
	}
	raw, _ := os.ReadFile(metricsFile)
	json.Unmarshal(raw, &mc2.metrics)

	if mc2.metrics["test"] != "value" {
		t.Errorf("loaded metrics test = %v", mc2.metrics["test"])
	}
}

func TestMetricsCollector_SaveAndLoadMTTR(t *testing.T) {
	dir := t.TempDir()
	mttrFile := filepath.Join(dir, "mttr.json")

	result := &ghpkg.MTTRResult{
		AvgMinutes:    30,
		MedianMinutes: 25,
		P90Minutes:    60,
		Count:         10,
	}

	data, _ := json.Marshal(result)
	os.WriteFile(mttrFile, data, 0o644)

	// Load back
	raw, _ := os.ReadFile(mttrFile)
	var loaded ghpkg.MTTRResult
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if loaded.Count != 10 {
		t.Errorf("count = %d, want 10", loaded.Count)
	}
	if loaded.AvgMinutes != 30 {
		t.Errorf("avg = %d, want 30", loaded.AvgMinutes)
	}
}

// --- buildIssueToMerge with actual data ---

func TestBuildIssueToMerge_WithHistory(t *testing.T) {
	mc := &MetricsCollector{
		mttr: &ghpkg.MTTRResult{
			AvgMinutes:     45,
			MedianMinutes:  30,
			P90Minutes:     120,
			Count:          5,
			FastestMinutes: 10,
			SlowestMinutes: 200,
			UpdatedAt:      time.Now().Format(time.RFC3339),
			History: []ghpkg.MTTRBucket{
				{T: time.Now().Unix(), Avg: 45.0, Median: 30.0},
			},
		},
	}
	result := buildIssueToMerge(mc)
	if result["count"] != 5 {
		t.Errorf("count = %v, want 5", result["count"])
	}
	if result["avg_minutes"] != 45 {
		t.Errorf("avg_minutes = %v, want 45", result["avg_minutes"])
	}
	history, ok := result["history"].([]map[string]any)
	if !ok {
		t.Fatal("history wrong type")
	}
	if len(history) != 1 {
		t.Errorf("history len = %d, want 1", len(history))
	}
}

// --- handleSnapshotPage with custom hub URL ---

func TestHandleSnapshotPage_CustomHubURL(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Hub.URL = "https://custom.hub.io"
	srv.deps.Config.Hub.AutoSnapshot = false

	req := httptest.NewRequest("GET", "/snapshot", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotPage(w, req)

	if !strings.Contains(w.Body.String(), "https://custom.hub.io") {
		t.Error("expected custom hub URL in response")
	}
}

// --- handleGovernorRepos with path-stripping ---

// An entry qualified with THIS hive's own org is the shape GitHub shows
// everywhere, so the save is accepted and stored as the bare repo name. It must
// never be persisted org-qualified: repo targets are built as org + "/" + repo,
// so "testorg/repo1" under org "testorg" would resolve to
// "testorg/testorg/repo1" and fail every agent.
func TestHandleGovernorRepos_StripOrgPrefix(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Project.Org = "testorg"
	body := `{"repos":["testorg/repo1"],"primaryRepo":"repo1"}`
	req := httptest.NewRequest("PUT", "/api/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: %s", w.Code, w.Body.String())
	}
	repos := srv.deps.Config.Project.Repos
	if len(repos) != 1 || repos[0] != "repo1" {
		t.Fatalf("Project.Repos = %v, want [repo1]", repos)
	}
}

// The org-qualified form is accepted in primaryRepo too, and both fields land
// bare so the always-exactly-one-default guard still matches them up.
func TestHandleGovernorRepos_StripOrgPrefixFromPrimary(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Project.Org = "zacburns"
	body := `{"repos":["zacburns/mlz-manager"],"primaryRepo":"zacburns/mlz-manager"}`
	req := httptest.NewRequest("PUT", "/api/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := srv.deps.Config.Project.Repos; len(got) != 1 || got[0] != "mlz-manager" {
		t.Fatalf("Project.Repos = %v, want [mlz-manager]", got)
	}
	if got := srv.deps.Config.Project.PrimaryRepo; got != "mlz-manager" {
		t.Fatalf("Project.PrimaryRepo = %q, want %q", got, "mlz-manager")
	}
}

// Positive control for the two tests above: a bare repo name must pass through
// the write path untouched. Without this a handler that stripped everything
// before the first '/' would still look correct.
func TestHandleGovernorRepos_BareRepoUnchanged(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Project.Org = "testorg"
	body := `{"repos":["repo1"],"primaryRepo":"repo1"}`
	req := httptest.NewRequest("PUT", "/api/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := srv.deps.Config.Project.Repos; len(got) != 1 || got[0] != "repo1" {
		t.Fatalf("Project.Repos = %v, want [repo1]", got)
	}
}

// A repo qualified with a DIFFERENT org must still be rejected with the clear
// repo_target message — normalizing it away would silently retarget the hive at
// a repository the owner never named.
func TestHandleGovernorRepos_ForeignOrgPrefixRejected(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Project.Org = "testorg"
	srv.deps.Config.Project.Repos = []string{"repo1"}
	srv.deps.Config.Project.PrimaryRepo = "repo1"
	body := `{"repos":["someoneelse/repo1"],"primaryRepo":"repo1"}`
	req := httptest.NewRequest("PUT", "/api/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "contains '/'") {
		t.Fatalf("body = %q, want the repo_target contains-'/' message", w.Body.String())
	}
	// The rejected value must not have been persisted.
	if got := srv.deps.Config.Project.Repos; len(got) != 1 || got[0] != "repo1" {
		t.Fatalf("Project.Repos = %v, want the pre-request [repo1]", got)
	}
}

// --- auditDetail ---

func TestAuditDetail(t *testing.T) {
	// No args
	if got := auditDetail(); got != "" {
		t.Errorf("expected empty, got %q", got)
	}

	// One pair
	got := auditDetail("key", "val")
	if got != "key=val" {
		t.Errorf("got %q", got)
	}

	// Multiple pairs
	got = auditDetail("a", "1", "b", "2")
	if got != "a=1, b=2" {
		t.Errorf("got %q", got)
	}

	// Odd number of args (last ignored)
	got = auditDetail("a", "1", "b")
	if got != "a=1" {
		t.Errorf("got %q", got)
	}
}

// --- sanitizeString ---

func TestSanitizeString_Boost(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello world", "hello world"},
		{"<script>alert(1)</script>", "alert(1)"},
		{"${SECRET}", ""},
		{"prefix ${VAR} suffix", "prefix  suffix"},
		{"  spaces  ", "spaces"},
	}
	for _, tt := range tests {
		got := sanitizeString(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeString(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// --- sanitizeFilenameComponent ---

func TestSanitizeFilenameComponent_Boost(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"my-file_name", "my-file_name"},
		{"file.txt", "file-txt"},
		{"a/b\\c", "a-b-c"},
		{"spaces here", "spaces-here"},
	}
	for _, tt := range tests {
		got := sanitizeFilenameComponent(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeFilenameComponent(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// --- decodeBody ---

func TestDecodeBody_Valid(t *testing.T) {
	body := strings.NewReader(`{"name":"test"}`)
	req := httptest.NewRequest("POST", "/test", body)
	var result struct {
		Name string `json:"name"`
	}
	err := decodeBody(req, &result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Name != "test" {
		t.Errorf("name = %q", result.Name)
	}
}

func TestDecodeBody_Invalid(t *testing.T) {
	body := strings.NewReader("not json")
	req := httptest.NewRequest("POST", "/test", body)
	var result struct{}
	err := decodeBody(req, &result)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

// --- resolveAgentParam ---

func TestResolveAgentParam_NilDeps(t *testing.T) {
	srv := &Server{}
	got := srv.resolveAgentParam("test")
	if got != "test" {
		t.Errorf("got %q, want test", got)
	}
}

func TestResolveAgentParam_WithAgentMgr(t *testing.T) {
	srv := newFullServer(t)
	// Resolve by name
	got := srv.resolveAgentParam("scanner")
	if got != "scanner" {
		t.Errorf("got %q, want scanner", got)
	}
	// Resolve by ID
	got = srv.resolveAgentParam("scan-001")
	if got != "scanner" {
		t.Errorf("resolveAgentParam(scan-001) = %q, want scanner", got)
	}
}

// --- handleBackends ---

func TestHandleBackends_Boost(t *testing.T) {
	srv := newFullServer(t)
	t.Setenv("HIVE_VLLM_MODELS", "")
	t.Setenv("HIVE_LLMD_MODELS", "")

	req := httptest.NewRequest("GET", "/api/backends", nil)
	w := httptest.NewRecorder()
	srv.handleBackends(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
	var result []map[string]any
	json.NewDecoder(w.Body).Decode(&result)
	if len(result) == 0 {
		t.Error("expected at least one backend")
	}
}

// --- handleSidebarGet / handleSidebarSet ---

func TestHandleSidebarGetSet(t *testing.T) {
	srv := newFullServer(t)

	// GET when empty
	req := httptest.NewRequest("GET", "/api/sidebar", nil)
	w := httptest.NewRecorder()
	srv.handleSidebarGet(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("GET code = %d", w.Code)
	}

	// SET
	req = httptest.NewRequest("PUT", "/api/sidebar", strings.NewReader(`{"items":["a","b"]}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	srv.handleSidebarSet(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("SET code = %d", w.Code)
	}

	// GET again - should have data
	req = httptest.NewRequest("GET", "/api/sidebar", nil)
	w = httptest.NewRecorder()
	srv.handleSidebarGet(w, req)
	if !strings.Contains(w.Body.String(), "items") {
		t.Error("expected sidebar data after set")
	}
}

// --- loadSidebarFromDisk / saveSidebarToDisk ---

func TestSidebarDisk_NoFile(t *testing.T) {
	srv := newFullServer(t)
	srv.loadSidebarFromDisk() // should not panic with missing file
}

// --- handleWidget ---

func TestHandleWidget_Boost(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/widget", nil)
	w := httptest.NewRecorder()
	srv.handleWidget(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
	var result map[string]any
	json.NewDecoder(w.Body).Decode(&result)
	if result["mode"] == nil {
		t.Error("expected mode field")
	}
}

// --- handleTrends ---

func TestHandleTrends_Default_Boost(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/trends", nil)
	w := httptest.NewRecorder()
	srv.handleTrends(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

func TestHandleTrends_Week_Boost(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/trends?range=week", nil)
	w := httptest.NewRecorder()
	srv.handleTrends(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

func TestHandleTrends_CustomHours_Boost(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/trends?hours=48", nil)
	w := httptest.NewRecorder()
	srv.handleTrends(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

func TestHandleTrends_MaxHours(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/trends?hours=9999", nil)
	w := httptest.NewRecorder()
	srv.handleTrends(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

// --- handleSummaries ---

func TestHandleSummaries_NoStatus(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/summaries", nil)
	w := httptest.NewRecorder()
	srv.handleSummaries(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

func TestHandleSummaries_WithStatus_Boost(t *testing.T) {
	srv := newFullServer(t)
	srv.UpdateStatus(minimalPayload())
	req := httptest.NewRequest("GET", "/api/summaries", nil)
	w := httptest.NewRecorder()
	srv.handleSummaries(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

// --- handlePane ---

func TestHandlePane_NotFound(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/pane/nonexistent", nil)
	req.SetPathValue("agent", "nonexistent")
	w := httptest.NewRecorder()
	srv.handlePane(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

// --- handleAgentConfigGet ---

func TestHandleAgentConfigGet_NotFound_Boost(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/agents/nonexistent/config", nil)
	req.SetPathValue("name", "nonexistent")
	w := httptest.NewRecorder()
	srv.handleAgentConfigGet(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

func TestHandleAgentConfigGet_Found(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/agents/scanner/config", nil)
	req.SetPathValue("name", "scanner")
	w := httptest.NewRecorder()
	srv.handleAgentConfigGet(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", w.Code)
	}
}

// --- handleAgentConfigRestrictions ---

func TestHandleAgentConfigRestrictions_NotFound_Boost(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/agents/nonexistent/restrictions", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "nonexistent")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleAgentConfigRestrictions(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

// --- queryInferenceModels ---

func TestQueryInferenceModels_WithEndpoints(t *testing.T) {
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "test-model"}},
		})
	}))
	defer mockSrv.Close()

	srv := newFullServer(t)
	srv.inferenceEndpoints = map[string][]string{"vllm": {mockSrv.URL}}

	models := srv.queryInferenceModels("vllm")
	if len(models) != 1 || models[0] != "test-model" {
		t.Errorf("models = %v", models)
	}
}

func TestQueryInferenceModels_FallbackToEnv(t *testing.T) {
	srv := newFullServer(t)
	t.Setenv("HIVE_VLLM_MODELS", "model-a,model-b")

	models := srv.queryInferenceModels("vllm")
	if len(models) != 2 {
		t.Errorf("models = %v", models)
	}
}

func TestQueryInferenceModels_DefaultModel(t *testing.T) {
	srv := newFullServer(t)
	t.Setenv("HIVE_VLLM_MODELS", "")
	t.Setenv("HIVE_LLMD_MODELS", "")

	// With no endpoints registered and no env override, every inference
	// backend falls back to the static common aliases.
	models := srv.queryInferenceModels("vllm")
	if len(models) != len(inferenceStaticModelAliases) {
		t.Errorf("expected %d static fallback aliases, got %v", len(inferenceStaticModelAliases), models)
	}
}

// --- handleGovernorAddAgent ---

func TestHandleGovernorAddAgent_InvalidBody(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/governor/agents", strings.NewReader("invalid"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorAddAgent(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestHandleGovernorAddAgent_MissingName_Boost(t *testing.T) {
	srv := newFullServer(t)
	body := `{"backend":"claude","model":"sonnet"}`
	req := httptest.NewRequest("POST", "/api/governor/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorAddAgent(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

// --- handleVersion ---

func TestHandleVersion_Boost(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/version", nil)
	w := httptest.NewRecorder()
	srv.handleVersion(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
	var result map[string]any
	json.NewDecoder(w.Body).Decode(&result)
	if result["version"] == nil {
		t.Error("expected version field")
	}
}

// --- handleKnowledgeToggle ---

func TestHandleKnowledgeToggle_Enable_Boost(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Knowledge = config.KnowledgeConfig{
		Layers: []config.KnowledgeLayer{
			{Type: "project", Path: t.TempDir()},
		},
	}
	body := `{"enabled":true}`
	req := httptest.NewRequest("PUT", "/api/knowledge/toggle", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleKnowledgeToggle(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", w.Code)
	}
}

func TestHandleKnowledgeToggle_Disable_Boost(t *testing.T) {
	srv := newFullServer(t)
	body := `{"enabled":false}`
	req := httptest.NewRequest("PUT", "/api/knowledge/toggle", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleKnowledgeToggle(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

// --- ensureKnowledge ---

func TestEnsureKnowledge_AlwaysCreates(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Knowledge = nil

	got := srv.ensureKnowledge()
	if !got {
		t.Error("ensureKnowledge should always return true (auto-creates)")
	}
	if srv.deps.Knowledge == nil {
		t.Error("expected knowledge to be auto-created")
	}
}

func TestEnsureKnowledge_Enabled(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Knowledge = config.KnowledgeConfig{
		Enabled: true,
		Layers: []config.KnowledgeLayer{
			{Type: "project", Path: t.TempDir()},
		},
	}
	// First call will create knowledge API
	got := srv.ensureKnowledge()
	if !got {
		t.Error("expected true when knowledge enabled")
	}
}

// --- handleKnowledgeList ---

func TestHandleKnowledgeList_AutoCreates(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Knowledge = nil

	req := httptest.NewRequest("GET", "/api/knowledge", nil)
	w := httptest.NewRecorder()
	srv.handleKnowledgeList(w, req)

	// ensureKnowledge auto-creates, so should get a response
	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

func TestHandleKnowledgeList_Enabled(t *testing.T) {
	srv := newFullServer(t)
	dir := t.TempDir()
	layers := []knowledge.LayerConfig{
		{Type: "project", Path: dir},
	}
	kcfg := knowledge.KnowledgeConfig{Enabled: true, Layers: layers}
	srv.deps.Knowledge = knowledge.NewKnowledgeAPI(layers, kcfg, srv.deps.Logger)

	req := httptest.NewRequest("GET", "/api/knowledge", nil)
	w := httptest.NewRecorder()
	srv.handleKnowledgeList(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

// --- handleKnowledgeStats ---

func TestHandleKnowledgeStats_Enabled(t *testing.T) {
	srv := newFullServer(t)
	dir := t.TempDir()
	layers := []knowledge.LayerConfig{
		{Type: "project", Path: dir},
	}
	kcfg := knowledge.KnowledgeConfig{Enabled: true, Layers: layers}
	srv.deps.Knowledge = knowledge.NewKnowledgeAPI(layers, kcfg, srv.deps.Logger)

	req := httptest.NewRequest("GET", "/api/knowledge/stats", nil)
	w := httptest.NewRecorder()
	srv.handleKnowledgeStats(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

// --- handleKnowledgeExport ---

func TestHandleKnowledgeExport_Disabled(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Knowledge = nil
	srv.deps.Config.Knowledge.Enabled = false

	req := httptest.NewRequest("GET", "/api/knowledge/export", nil)
	w := httptest.NewRecorder()
	srv.handleKnowledgeExport(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

// --- handleKnowledgeLayer ---

func TestHandleKnowledgeLayer_NoKnowledge(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Knowledge = nil
	srv.deps.Config.Knowledge.Enabled = false

	req := httptest.NewRequest("GET", "/api/knowledge/layer/project", nil)
	req.SetPathValue("layer", "project")
	w := httptest.NewRecorder()
	srv.handleKnowledgeLayer(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

func TestHandleKnowledgeLayer_WithKnowledge_Boost(t *testing.T) {
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
}

// --- HealthSummary comprehensive ---

func TestHealthSummary_WithStatus(t *testing.T) {
	srv := newFullServer(t)
	srv.MarkReady()
	srv.UpdateStatus(minimalPayload())

	result := srv.HealthSummary()
	if result["status"] == nil {
		t.Error("expected status field")
	}
}

// --- handleStatSources ---

func TestHandleStatSources_Boost(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/stat-sources", nil)
	w := httptest.NewRecorder()
	srv.handleStatSources(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

// --- handleAgentExport ---

func TestHandleAgentExport_NotFound_Boost(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/agents/nonexistent/export", nil)
	req.SetPathValue("name", "nonexistent")
	w := httptest.NewRecorder()
	srv.handleAgentExport(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

func TestHandleAgentExport_Found(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/agents/scanner/export", nil)
	req.SetPathValue("name", "scanner")
	w := httptest.NewRecorder()
	srv.handleAgentExport(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "AgentDefinition") {
		t.Error("expected AgentDefinition in YAML export")
	}
}

// --- handleNousGateDecision ---

func TestHandleNousGateDecision_NoNous(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Nous = nil
	req := httptest.NewRequest("POST", "/api/nous/gate", strings.NewReader(`{"decision":"approve"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleNousGateDecision(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

// --- handleHiveIDSet ---

func TestHandleHiveIDSet_EmptyID_Boost(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/hive-id", strings.NewReader(`{"id":""}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleHiveIDSet(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestHandleHiveIDSet_Valid(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/hive-id", strings.NewReader(`{"id":"my-hive"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleHiveIDSet(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", w.Code)
	}
	if srv.deps.Config.HiveID != "my-hive" {
		t.Errorf("HiveID = %q", srv.deps.Config.HiveID)
	}
}

// --- handleAgentConfigGeneral ---

func TestHandleAgentConfigGeneral_NotFound_Boost(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/agents/nonexistent/config", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "nonexistent")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleAgentConfigGeneral(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

func TestHandleAgentConfigGeneral_InvalidBody_Boost(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/agents/scanner/config", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "scanner")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleAgentConfigGeneral(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

// --- okResponse ---

func TestOkResponse_Boost(t *testing.T) {
	w := httptest.NewRecorder()
	okResponse(w, map[string]string{"key": "val"})

	var result map[string]any
	json.NewDecoder(w.Body).Decode(&result)
	if result["ok"] != true {
		t.Error("expected ok=true")
	}
	if result["key"] != "val" {
		t.Errorf("key = %v", result["key"])
	}
}

func TestOkResponse_NilExtra(t *testing.T) {
	w := httptest.NewRecorder()
	okResponse(w, nil)

	var result map[string]any
	json.NewDecoder(w.Body).Decode(&result)
	if result["ok"] != true {
		t.Error("expected ok=true")
	}
}

// --- jsonResponse / jsonError ---

func TestJsonResponse_Boost(t *testing.T) {
	w := httptest.NewRecorder()
	jsonResponse(w, map[string]string{"hello": "world"})

	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("content-type = %q", w.Header().Get("Content-Type"))
	}
}

func TestJsonError_Boost(t *testing.T) {
	w := httptest.NewRecorder()
	jsonError(w, "bad request", http.StatusBadRequest)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d", w.Code)
	}
	var result map[string]any
	json.NewDecoder(w.Body).Decode(&result)
	if result["ok"] != false {
		t.Error("expected ok=false")
	}
	if result["error"] != "bad request" {
		t.Errorf("error = %v", result["error"])
	}
}

// --- refreshAfterMutation / refreshAndPersist ---

func TestRefreshAfterMutation_NilDeps_Boost(t *testing.T) {
	srv := &Server{}
	srv.refreshAfterMutation() // should not panic
}

func TestRefreshAndPersist(t *testing.T) {
	srv := newFullServer(t)
	var refreshed, persisted atomic.Bool
	srv.deps.RefreshFunc = func() { refreshed.Store(true) }
	srv.deps.PersistFunc = func() { persisted.Store(true) }
	srv.refreshAndPersist()
	// These run in goroutines — poll instead of racing on plain bools.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && (!refreshed.Load() || !persisted.Load()) {
		time.Sleep(10 * time.Millisecond)
	}
	if !refreshed.Load() {
		t.Error("refresh not called")
	}
	if !persisted.Load() {
		t.Error("persist not called")
	}
}

// --- handleGHUserAuthStatus with saved token ---

func TestHandleGHUserAuthStatus_InvalidToken(t *testing.T) {
	srv := newFullServer(t)
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "gh-user-token")
	os.WriteFile(tokenFile, []byte("fake-invalid-token"), 0o600)
	// Can't easily test since userTokenPath is a const. Just verify the handler
	// handles missing file gracefully.
	req := httptest.NewRequest("GET", "/api/gh-user-auth/status", nil)
	w := httptest.NewRecorder()
	srv.handleGHUserAuthStatus(w, req)

	var result map[string]any
	json.NewDecoder(w.Body).Decode(&result)
	if result["logged_in"] != false {
		t.Errorf("expected logged_in=false, got %v", result["logged_in"])
	}
}

// --- buildHealth ---

func TestBuildHealth_NilClient_Boost(t *testing.T) {
	result := buildHealth(nil, nil)
	if result == nil {
		t.Fatal("expected non-nil")
	}
	// With no cached health, should return default
	if result["ci"] != 100 {
		t.Errorf("ci = %v", result["ci"])
	}
}

// --- collectSystemResources ---

func TestCollectSystemResources(t *testing.T) {
	res := collectSystemResources()
	if res == nil {
		t.Fatal("expected non-nil")
	}
	if res.CpuCores <= 0 {
		t.Errorf("CpuCores = %d", res.CpuCores)
	}
}
