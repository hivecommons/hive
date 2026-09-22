package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
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
	if env.Data.TaskContext.Assignment.TaskID != "task-1" || env.Data.TaskContext.Assignment.Repo != "owner/repo" {
		t.Fatalf("assignment = %#v", env.Data.TaskContext.Assignment)
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
	s.contributeHub.connections["alice"] = &ContributorConnection{
		currentTask: &WSTaskAssign{TaskID: "task-1", Kind: "issue", Repo: "owner/repo", Number: 42, Title: "do work"},
	}
	s.status = &StatusPayload{Timestamp: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), Repos: []FrontendRepo{{
		Name: "repo", Full: "owner/repo",
		OpenPrs: []any{map[string]any{"repo": "owner/repo", "number": 43, "failing_checks": []string{"build"}}},
	}}}
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
