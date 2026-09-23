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

// #8348: hub-launched agents authenticate to the task MCP endpoint with a
// per-launch token bound to their active launch, never with the dashboard
// bearer. The token is accepted only while the launch it names is active,
// scopes every tool call to that launch, and refuses other tasks with the
// same typed refusal remote leases get.

const launchScopeTestSecret = "dashboard-secret-8348"

func newLaunchScopeTestServer(t *testing.T, launches func() []taskmcp.LaunchScope) *Server {
	t.Helper()
	s := newTestServer()
	s.authToken = launchScopeTestSecret
	s.deps = &Dependencies{
		Config:                &config.Config{TaskMCP: config.TaskMCPConfig{LeaseRateLimitPerMinute: 10}},
		TaskMCPActiveLaunches: launches,
	}
	s.contributeHub = NewContributeWSHub(s.logger, s)
	return s
}

func mintLaunchToken(t *testing.T, launch taskmcp.LaunchScope, expiresAt time.Time) string {
	t.Helper()
	token, _, err := taskmcp.MintLeaseToken([]byte(launchScopeTestSecret), taskmcp.LeaseTokenClaims{
		TaskID: launch.TaskID, Identity: launch.Agent, Repo: launch.Repo, Number: launch.Number, ExpiresAt: expiresAt,
	}, time.Now())
	if err != nil {
		t.Fatalf("mint launch token: %v", err)
	}
	return token
}

func TestContributeMCPLaunchTokenScopesAndRefusesCrossTask(t *testing.T) {
	launch := taskmcp.LaunchScope{TaskID: "scanner:owner/repo#0:3", Repo: "owner/repo", Agent: "scanner", Generation: 3, StartedAt: time.Now().Add(-time.Minute)}
	active := []taskmcp.LaunchScope{launch}
	s := newLaunchScopeTestServer(t, func() []taskmcp.LaunchScope { return active })
	token := mintLaunchToken(t, launch, time.Now().Add(config.DefaultTaskMCPLaunchTokenTTL))
	if strings.Contains(token, launchScopeTestSecret) {
		t.Fatalf("launch token embeds the dashboard secret: %q", token)
	}

	// Own task via the URL query, the way a CLI --mcp-server URL carries it.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, taskmcp.EndpointPath+"?"+taskmcp.TokenQueryParam+"="+token, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"task_context","arguments":{}}}`))
	s.handleContributeMCP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("own task status = %d body=%s", rec.Code, rec.Body.String())
	}
	var okEnv struct {
		Data taskmcp.TaskContextData `json:"data"`
	}
	if err := json.Unmarshal([]byte(mcpResultText(t, rec.Body.Bytes())), &okEnv); err != nil {
		t.Fatal(err)
	}
	if okEnv.Data.Assignment.TaskID != launch.TaskID || okEnv.Data.Assignment.Repo != launch.Repo || okEnv.Data.Lease.Generation != launch.Generation {
		t.Fatalf("assignment = %#v, want the launch scope", okEnv.Data.Assignment)
	}

	// Another task: typed refusal, not cross-task content.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, taskmcp.EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"task_context","arguments":{"task_id":"reviewer:owner/repo#0:1","repo":"owner/repo"}}}`))
	req.Header.Set("Authorization", "Bearer "+token)
	s.handleContributeMCP(rec, req)
	var refused struct {
		Data taskmcp.RefusalData `json:"data"`
	}
	if err := json.Unmarshal([]byte(mcpResultText(t, rec.Body.Bytes())), &refused); err != nil {
		t.Fatal(err)
	}
	if refused.Data.Type != "refusal" || refused.Data.Code != "outside_lease_scope" || refused.Data.LeaseID == "" {
		t.Fatalf("refusal = %#v", refused.Data)
	}

	// Launch ends (or relaunches under a new generation): token stops working.
	active = []taskmcp.LaunchScope{{TaskID: "scanner:owner/repo#0:4", Repo: "owner/repo", Agent: "scanner", Generation: 4, StartedAt: time.Now()}}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, taskmcp.EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	s.handleContributeMCP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ended launch status = %d, want 403", rec.Code)
	}
}

func TestContributeMCPLaunchTokenRejectsForgedExpiredAndWrongAgent(t *testing.T) {
	launch := taskmcp.LaunchScope{TaskID: "scanner:owner/repo#0:3", Repo: "owner/repo", Agent: "scanner", Generation: 3, StartedAt: time.Now()}
	s := newLaunchScopeTestServer(t, func() []taskmcp.LaunchScope { return []taskmcp.LaunchScope{launch} })

	forged, _, err := taskmcp.MintLeaseToken([]byte("not-the-dashboard-secret"), taskmcp.LeaseTokenClaims{
		TaskID: launch.TaskID, Identity: launch.Agent, Repo: launch.Repo, ExpiresAt: time.Now().Add(time.Hour),
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	wrongAgent := taskmcp.LaunchScope{TaskID: launch.TaskID, Repo: launch.Repo, Agent: "reviewer"}
	cases := map[string]string{
		"forged signature": forged,
		"expired":          mintLaunchToken(t, launch, time.Now().Add(-time.Second)),
		"wrong agent":      mintLaunchToken(t, wrongAgent, time.Now().Add(time.Hour)),
	}
	for name, token := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, taskmcp.EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		s.handleContributeMCP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", name, rec.Code)
		}
	}

	// Positive control: the dashboard's own UI bearer still reaches the endpoint.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, taskmcp.EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+launchScopeTestSecret)
	s.handleContributeMCP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard bearer status = %d, want 200", rec.Code)
	}
}
