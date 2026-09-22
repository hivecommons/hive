package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/taskmcp"
)

func TestContributeMCPContextBundleUsesActiveAssignment(t *testing.T) {
	s := newTestServer()
	s.authToken = "secret"
	s.deps = &Dependencies{Config: &config.Config{Agents: map[string]config.AgentConfig{"lane": {Standby: &config.StandbyConfig{Enabled: true, DailyCapPerContributor: 2}}}}}
	s.contributeHub = NewContributeWSHub(s.logger, s)
	s.contributeHub.connections["alice"] = &ContributorConnection{
		currentTask:    &WSTaskAssign{TaskID: "task-1", Kind: "issue", Repo: "owner/repo", Number: 42, Title: "do work", Key: "owner/repo#42"},
		currentTaskGen: 9,
		currentLabels:  []string{"kind/feature"},
		taskAssignedAt: time.Now().Add(-time.Minute),
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, taskmcp.EndpointPath+"?token=secret", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"context_bundle","arguments":{"task_id":"task-1","repo":"owner/repo"}}}`))
	s.handleContributeMCP(rec, req)
	text := mcpResultText(t, rec.Body.Bytes())
	var env struct {
		Data taskmcp.BundleData `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.TaskContext == nil || env.Data.TaskContext.Assignment.TaskID != "task-1" || env.Data.TaskContext.Assignment.Repo != "owner/repo" {
		t.Fatalf("assignment = %#v", env.Data.TaskContext)
	}
	if got := env.Data.TaskContext.Policies.Standby; len(got) != 1 || got[0].Lane != "lane" || got[0].DailyCap != 2 {
		t.Fatalf("standby policy = %#v", got)
	}
}

func TestContributeMCPRejectsMissingTaskScope(t *testing.T) {
	s := newTestServer()
	s.authToken = "secret"
	s.contributeHub = NewContributeWSHub(s.logger, s)
	s.contributeHub.connections["alice"] = &ContributorConnection{
		currentTask: &WSTaskAssign{TaskID: "task-1", Repo: "owner/repo", Number: 42, Title: "do work"},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, taskmcp.EndpointPath+"?token=secret", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"task_context","arguments":{}}}`))
	s.handleContributeMCP(rec, req)
	var resp struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || resp.Error.Code != -32003 {
		t.Fatalf("error = %#v, want forbidden", resp.Error)
	}
}

func TestContributeMCPRequiresDashboardTokenWhenConfigured(t *testing.T) {
	s := newTestServer()
	s.authToken = "secret"
	s.contributeHub = NewContributeWSHub(s.logger, s)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, taskmcp.EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	s.handleContributeMCP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, taskmcp.EndpointPath+"?token=secret", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	s.handleContributeMCP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status with token = %d, want 200", rec.Code)
	}
}

func TestContributeMCPRelatedWorkFromStatusCache(t *testing.T) {
	s := newTestServer()
	s.authToken = "secret"
	s.deps = &Dependencies{Config: &config.Config{}}
	s.contributeHub = NewContributeWSHub(s.logger, s)
	s.contributeHub.connections["alice"] = &ContributorConnection{
		currentTask: &WSTaskAssign{TaskID: "task-1", Kind: "issue", Repo: "owner/repo", Number: 42, Title: "do work"},
	}
	s.status = &StatusPayload{Repos: []FrontendRepo{{
		Name: "repo", Full: "owner/repo",
		ActionableIssues: []any{map[string]any{"repo": "owner/repo", "number": 42, "title": "do work", "files": []string{"a.go"}}},
		OpenPrs:          []any{map[string]any{"repo": "owner/repo", "number": 43, "title": "Fixes #42", "author": "bob", "files": []string{"a.go"}}},
	}}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, taskmcp.EndpointPath+"?token=secret", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"related_work","arguments":{"task_id":"task-1","repo":"owner/repo"}}}`))
	s.handleContributeMCP(rec, req)
	text := mcpResultText(t, rec.Body.Bytes())
	var env struct {
		Data taskmcp.RelatedWorkData `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Items) != 1 || env.Data.Items[0].Number != 43 || len(env.Data.Items[0].Reasons) == 0 {
		t.Fatalf("related items = %#v", env.Data.Items)
	}
}

func TestContributeMCPCIHealthFromStatusCache(t *testing.T) {
	s := newTestServer()
	s.authToken = "secret"
	s.deps = &Dependencies{Config: &config.Config{AutoMerge: config.AutoMergeConfig{RequiredChecks: []string{"build", "test"}}}}
	s.contributeHub = NewContributeWSHub(s.logger, s)
	s.contributeHub.connections["alice"] = &ContributorConnection{currentTask: &WSTaskAssign{TaskID: "task-1", Kind: "issue", Repo: "owner/repo", Number: 42, Title: "do work"}}
	s.status = &StatusPayload{
		Timestamp: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		Repos: []FrontendRepo{{
			Name: "repo", Full: "owner/repo",
			OpenPrs: []any{map[string]any{"repo": "owner/repo", "number": 43, "failing_checks": []string{"build"}}},
		}},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, taskmcp.EndpointPath+"?token=secret", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ci_health","arguments":{"task_id":"task-1","repo":"owner/repo"}}}`))
	s.handleContributeMCP(rec, req)
	text := mcpResultText(t, rec.Body.Bytes())
	var env struct {
		Data taskmcp.CIHealthData `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Checks) != 2 || !env.Data.CacheStale {
		t.Fatalf("ci health = %#v", env.Data)
	}
}

func TestContributeMCPLeaseBearerScopesAndRefusesCrossTask(t *testing.T) {
	s := newTestServer()
	s.authToken = "secret"
	s.deps = &Dependencies{Config: &config.Config{TaskMCP: config.TaskMCPConfig{RemoteEnabled: true, LeaseRateLimitPerMinute: 10}, Dashboard: config.DashboardConfig{PublicURL: "https://hive.example"}}}
	s.contributeHub = NewContributeWSHub(s.logger, s)
	assign := &WSTaskAssign{TaskID: "task-lease", Kind: "issue", Repo: "owner/repo", Number: 42, Title: "leased work"}
	s.contributeHub.connections["alice"] = &ContributorConnection{
		profile:        &ContributorProfile{GitHubUsername: "alice", ContributorID: "alice-id"},
		currentTask:    assign,
		currentTaskGen: 2,
		taskAssignedAt: time.Now().Add(-time.Minute),
	}
	s.contributeHub.recordLeaseForKey("alice-id", assign.TaskID, assign.Repo, assign.Number, assign.identityKey(), "trusted", 2, time.Now())
	mcp := s.contributeHub.mintTaskMCPForAssignment("alice-id", assign, time.Now().Add(leaseTTL), "alice")
	if mcp == nil || mcp.Token == "" || mcp.URL != "https://hive.example"+taskmcp.EndpointPath {
		t.Fatalf("mcp = %#v", mcp)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, taskmcp.EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"task_context","arguments":{"task_id":"task-lease","repo":"owner/repo","number":42}}}`))
	req.Header.Set("Authorization", "Bearer "+mcp.Token)
	s.handleContributeMCP(rec, req)
	text := mcpResultText(t, rec.Body.Bytes())
	var okEnv struct {
		Data taskmcp.TaskContextData `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &okEnv); err != nil {
		t.Fatal(err)
	}
	if okEnv.Data.Assignment.TaskID != "task-lease" {
		t.Fatalf("assignment = %#v", okEnv.Data.Assignment)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, taskmcp.EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"related_work","arguments":{"task_id":"other","repo":"owner/other","number":7}}}`))
	req.Header.Set("Authorization", "Bearer "+mcp.Token)
	s.handleContributeMCP(rec, req)
	text = mcpResultText(t, rec.Body.Bytes())
	var refused struct {
		Data taskmcp.RefusalData `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &refused); err != nil {
		t.Fatal(err)
	}
	if refused.Data.Type != "refusal" || refused.Data.Code != "outside_lease_scope" || refused.Data.LeaseID == "" {
		t.Fatalf("refusal = %#v", refused.Data)
	}

	s.contributeHub.revokeLease("alice-id", assign.TaskID)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, taskmcp.EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+mcp.Token)
	s.handleContributeMCP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("revoked lease status = %d, want 403", rec.Code)
	}
}

func TestTaskAssignMCPRoundTrip(t *testing.T) {
	msg := WSMessage{Type: "task_assign", TaskID: "t1", Repo: "owner/repo", Number: 1, MCP: &WSTaskMCP{URL: "https://hive.example/api/contribute/mcp", Token: "tok", ExpiresAt: "2026-09-22T12:00:00Z"}}
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var got WSMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.MCP == nil || got.MCP.URL != msg.MCP.URL || got.MCP.Token != "tok" || got.MCP.ExpiresAt == "" {
		t.Fatalf("mcp round trip = %#v json=%s", got.MCP, b)
	}
}

func TestContributeMCPRepoConventionsFromKnowledge(t *testing.T) {
	s := mcpPhase3Server(t)
	writeKnowledgeFact(t, s, "repo-conventions.md", "Repository conventions", "Always run go test.", []string{"kind:conventions", "repo:owner/repo", "source:human"})
	rec := callMCP(t, s, taskmcp.ToolRepoConventions, `{"task_id":"task-1","repo":"owner/repo"}`)
	text := mcpResultText(t, rec.Body.Bytes())
	var env struct {
		Data taskmcp.RepoConventionsData `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Source != "human" || len(env.Data.Conventions) != 1 || env.Data.Conventions[0].Data.Body != "Always run go test." {
		t.Fatalf("repo conventions = %#v", env.Data)
	}
}

func TestContributeMCPRepoConventionsAbsent(t *testing.T) {
	s := mcpPhase3Server(t)
	rec := callMCP(t, s, taskmcp.ToolRepoConventions, `{"task_id":"task-1","repo":"owner/repo"}`)
	text := mcpResultText(t, rec.Body.Bytes())
	var env struct {
		Data taskmcp.RepoConventionsData `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Source != "none" || len(env.Data.Conventions) != 0 {
		t.Fatalf("repo conventions absent = %#v", env.Data)
	}
}

func TestContributeMCPDependenciesFromBeads(t *testing.T) {
	s := mcpPhase3Server(t)
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	epic, _ := store.Create("epic", beads.TypeEpic, beads.PriorityMedium, "architect", "gh-owner/repo#42")
	blocker, _ := store.Create("blocker", beads.TypeTask, beads.PriorityMedium, "architect", "")
	child, _ := store.Create("child", beads.TypeTask, beads.PriorityMedium, "architect", "")
	_ = store.AddDependency(epic.ID, blocker.ID)
	_ = store.Update(epic.ID, func(b *beads.Bead) { b.Metadata[planning.MetaPlanStatus] = planning.PlanStatusApproved })
	_ = store.Update(child.ID, func(b *beads.Bead) { b.Metadata[planning.MetaParentEpic] = epic.ID })
	s.deps.BeadStores = map[string]*beads.Store{"architect": store}
	rec := callMCP(t, s, taskmcp.ToolDependencies, `{"task_id":"task-1","repo":"owner/repo"}`)
	text := mcpResultText(t, rec.Body.Bytes())
	var env struct {
		Data taskmcp.DependenciesData `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.BlockedBy) != 1 || env.Data.BlockedBy[0].ID != blocker.ID || len(env.Data.SuggestedPlan) != 1 || env.Data.SuggestedPlan[0].ID != child.ID {
		t.Fatalf("dependencies = %#v", env.Data)
	}
}

func TestContributeMCPHistoryAndKnowledgeScopedAndPaginated(t *testing.T) {
	s := mcpPhase3Server(t)
	merged := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	s.status = &StatusPayload{Repos: []FrontendRepo{{
		Name:             "repo",
		Full:             "owner/repo",
		ActionableIssues: []any{map[string]any{"repo": "owner/repo", "number": 42, "files": []string{"a.go"}}},
		OpenPrs: []any{
			map[string]any{"repo": "owner/repo", "number": 10, "state": "closed", "merged_at": merged, "files": []string{"a.go"}, "title": "merged", "body": "details"},
			map[string]any{"repo": "other/repo", "number": 11, "state": "closed", "merged_at": merged, "files": []string{"a.go"}},
		},
	}}}
	writeKnowledgeFact(t, s, "owner.md", "Owner fact", "Scoped knowledge.", []string{"repo:owner/repo"})
	writeKnowledgeFact(t, s, "other.md", "Other fact", "Wrong repo.", []string{"repo:other/repo"})
	rec := callMCP(t, s, taskmcp.ToolHistory, `{"task_id":"task-1","repo":"owner/repo","limit":1}`)
	var hist struct {
		Data taskmcp.HistoryData `json:"data"`
		Page taskmcp.PageInfo    `json:"page"`
	}
	if err := json.Unmarshal([]byte(mcpResultText(t, rec.Body.Bytes())), &hist); err != nil {
		t.Fatal(err)
	}
	if len(hist.Data.Items) != 1 || hist.Data.Items[0].Number != 10 || hist.Data.Items[0].Data.Body != "details" {
		t.Fatalf("history = %#v", hist)
	}
	rec = callMCP(t, s, taskmcp.ToolKnowledge, `{"task_id":"task-1","repo":"owner/repo","query":"fact"}`)
	var know struct {
		Data taskmcp.KnowledgeData `json:"data"`
	}
	if err := json.Unmarshal([]byte(mcpResultText(t, rec.Body.Bytes())), &know); err != nil {
		t.Fatal(err)
	}
	if len(know.Data.Items) != 1 || know.Data.Items[0].Repo != "owner/repo" || know.Data.Items[0].Data.Body != "Scoped knowledge." {
		t.Fatalf("knowledge = %#v", know.Data)
	}
}

func TestContributeMCPKnowledgeRequiresExactRepoTagBeforeLimit(t *testing.T) {
	s := mcpPhase3Server(t)
	s.deps.Config.Hub.TaskMCPKnowledgeLimit = 1
	writeKnowledgeFact(t, s, "other.md", "Repository conventions", "Wrong repo.", []string{"kind:conventions", "repo:owner/repo2"})
	writeKnowledgeFact(t, s, "owner.md", "Repository conventions", "Right repo.", []string{"kind:conventions", "repo:owner/repo"})
	rec := callMCP(t, s, taskmcp.ToolRepoConventions, `{"task_id":"task-1","repo":"owner/repo"}`)
	var env struct {
		Data taskmcp.RepoConventionsData `json:"data"`
	}
	if err := json.Unmarshal([]byte(mcpResultText(t, rec.Body.Bytes())), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Conventions) != 1 || env.Data.Conventions[0].Data.Body != "Right repo." {
		t.Fatalf("repo conventions = %#v", env.Data)
	}
}

func TestContributeMCPRepoConventionsUseFullBody(t *testing.T) {
	s := mcpPhase3Server(t)
	longBody := strings.TrimSpace(strings.Repeat("convention ", 40))
	writeKnowledgeFact(t, s, "long.md", "Repository conventions", longBody, []string{"kind:conventions", "repo:owner/repo"})
	rec := callMCP(t, s, taskmcp.ToolRepoConventions, `{"task_id":"task-1","repo":"owner/repo"}`)
	var env struct {
		Data taskmcp.RepoConventionsData `json:"data"`
	}
	if err := json.Unmarshal([]byte(mcpResultText(t, rec.Body.Bytes())), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Conventions) != 1 || env.Data.Conventions[0].Data.Body != longBody {
		t.Fatalf("conventions body len = %d, want %d", len(env.Data.Conventions[0].Data.Body), len(longBody))
	}
}

func mcpPhase3Server(t *testing.T) *Server {
	t.Helper()
	s := newTestServer()
	s.authToken = "secret"
	s.deps = &Dependencies{Config: &config.Config{}, Knowledge: knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{Enabled: true, Engine: "file"}, s.logger)}
	vault := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(vault, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.deps.Knowledge.ConnectVault(vault, "test-vault"); err != nil {
		t.Fatal(err)
	}
	s.contributeHub = NewContributeWSHub(s.logger, s)
	s.contributeHub.connections["alice"] = &ContributorConnection{currentTask: &WSTaskAssign{TaskID: "task-1", Kind: "issue", Repo: "owner/repo", Number: 42, Title: "do work", Key: "owner/repo#42"}}
	return s
}

func writeKnowledgeFact(t *testing.T, s *Server, name, title, body string, tags []string) {
	t.Helper()
	vaults := s.deps.Knowledge.FileStores()
	if len(vaults) != 1 {
		t.Fatalf("vaults = %d", len(vaults))
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("title: " + title + "\n")
	if len(tags) > 0 {
		b.WriteString("tags: [")
		for i, tag := range tags {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(tag)
		}
		b.WriteString("]\n")
	}
	b.WriteString("---\n\n")
	b.WriteString(body)
	if err := os.WriteFile(filepath.Join(vaults[0].RootDir(), name), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	vaults[0].Reindex()
}

func callMCP(t *testing.T, s *Server, tool, args string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
	req := httptest.NewRequest(http.MethodPost, taskmcp.EndpointPath+"?token=secret", strings.NewReader(body))
	s.handleContributeMCP(rec, req)
	return rec
}

func mcpResultText(t *testing.T, body []byte) string {
	t.Helper()
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("rpc error: %#v body=%s", resp.Error, body)
	}
	if len(resp.Result.Content) != 1 {
		t.Fatalf("content = %#v", resp.Result.Content)
	}
	return resp.Result.Content[0].Text
}
