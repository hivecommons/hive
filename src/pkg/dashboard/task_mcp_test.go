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
