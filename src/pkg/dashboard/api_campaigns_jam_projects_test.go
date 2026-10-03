package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestCampaignJamProjectSyncInitialPublish(t *testing.T) {
	s := jamTestServer(t)
	var captured campaignProjectSyncPayload
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Variables struct {
				Input campaignProjectSyncPayload `json:"input"`
			} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode project sync request: %v", err)
		}
		captured = body.Variables.Input
		_, _ = w.Write([]byte(`{"data":{"hiveJamProjectSync":{"ok":true}}}`))
	}))
	t.Cleanup(api.Close)
	t.Setenv(jamProjectSyncEndpointEnv, api.URL)

	seed := jamPostAs(t, s, "/api/campaigns/spec-project/jam", "read-write", "alice", map[string]any{"spec_content": "## Goals\nShip together"}, false)
	if seed.Code != http.StatusOK {
		t.Fatalf("seed = %d body=%s", seed.Code, seed.Body.String())
	}
	suggestion := jamPostAs(t, s, "/api/campaigns/spec-project/jam/suggestions", "read-write", "alice", map[string]any{
		"section":       "Goals",
		"proposed_text": "Add Projects sync",
	}, false)
	if suggestion.Code != http.StatusOK {
		t.Fatalf("suggestion = %d body=%s", suggestion.Code, suggestion.Body.String())
	}
	enable := jamPostAs(t, s, "/api/campaigns/spec-project/jam/project-sync", "owner", "maintainer", map[string]any{
		"action":      "enable",
		"project_url": "https://github.com/orgs/hivecommons/projects/7",
	}, true)
	if enable.Code != http.StatusOK {
		t.Fatalf("enable sync = %d body=%s", enable.Code, enable.Body.String())
	}
	sync := jamPostAs(t, s, "/api/campaigns/spec-project/jam/project-sync", "owner", "maintainer", map[string]any{"action": "sync"}, true)
	if sync.Code != http.StatusOK {
		t.Fatalf("sync = %d body=%s", sync.Code, sync.Body.String())
	}
	jam := decodeJam(t, sync)
	if jam.ProjectSync == nil || jam.ProjectSync.LastStatus != "synced" || len(jam.ProjectSync.PublishedItems) < 2 {
		t.Fatalf("project sync state = %+v", jam.ProjectSync)
	}
	if captured.ProjectURL == "" || captured.Spec == "" || len(captured.Items) < 2 {
		t.Fatalf("captured payload = %+v", captured)
	}
}

func TestCampaignJamProjectSyncInboundStatusPreservesDecisions(t *testing.T) {
	s := jamTestServer(t)
	create := jamPostAs(t, s, "/api/campaigns/spec-project-status/jam/polls", "read-write", "alice", map[string]any{
		"section": "Scope", "question": "Track status?", "options": []string{"yes", "no"},
	}, false)
	if create.Code != http.StatusOK {
		t.Fatalf("poll create = %d body=%s", create.Code, create.Body.String())
	}
	jam := decodeJam(t, create)
	decide := jamPostAs(t, s, "/api/campaigns/spec-project-status/jam/polls", "owner", "maintainer", map[string]any{
		"action": "decide", "poll_id": jam.Polls[0].ID, "outcome": "yes", "rationale": "Need visibility",
	}, true)
	if decide.Code != http.StatusOK {
		t.Fatalf("poll decide = %d body=%s", decide.Code, decide.Body.String())
	}
	enable := jamPostAs(t, s, "/api/campaigns/spec-project-status/jam/project-sync", "owner", "maintainer", map[string]any{
		"action": "enable", "project_id": "PVT_kwDOB",
	}, true)
	if enable.Code != http.StatusOK {
		t.Fatalf("enable = %d body=%s", enable.Code, enable.Body.String())
	}
	inbound := jamPostAs(t, s, "/api/campaigns/spec-project-status/jam/project-sync", "owner", "maintainer", map[string]any{
		"action": "inbound_status", "item_type": "derived_issue", "external_id": "item-1", "status": "Done",
	}, true)
	if inbound.Code != http.StatusOK {
		t.Fatalf("inbound = %d body=%s", inbound.Code, inbound.Body.String())
	}
	jam = decodeJam(t, inbound)
	if jam.ProjectSync.LastStatus != "Done" || len(jam.Revisions) != 1 || len(jam.Revisions[0].Decisions) != 1 {
		t.Fatalf("inbound status lost decision data: sync=%+v revisions=%+v", jam.ProjectSync, jam.Revisions)
	}
}

func TestCampaignJamProjectSyncDisabledAndFailureHandling(t *testing.T) {
	s := jamTestServer(t)
	called := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	t.Cleanup(api.Close)
	t.Setenv(jamProjectSyncEndpointEnv, api.URL)

	disabled := jamPostAs(t, s, "/api/campaigns/spec-project-fail/jam/project-sync", "owner", "maintainer", map[string]any{"action": "sync"}, true)
	if disabled.Code != http.StatusBadRequest || called {
		t.Fatalf("disabled sync code=%d called=%v body=%s", disabled.Code, called, disabled.Body.String())
	}
	enable := jamPostAs(t, s, "/api/campaigns/spec-project-fail/jam/project-sync", "owner", "maintainer", map[string]any{"action": "enable", "project_id": "PVT_fail"}, true)
	if enable.Code != http.StatusOK {
		t.Fatalf("enable = %d body=%s", enable.Code, enable.Body.String())
	}
	failed := jamPostAs(t, s, "/api/campaigns/spec-project-fail/jam/project-sync", "owner", "maintainer", map[string]any{"action": "sync"}, true)
	if failed.Code != http.StatusBadGateway {
		t.Fatalf("failed sync = %d body=%s", failed.Code, failed.Body.String())
	}
	get := doOwnerGet(s, "/api/campaigns/spec-project-fail/jam")
	jam := decodeJam(t, get)
	if jam.ProjectSync.LastStatus != "failed" || jam.ProjectSync.LastError == "" || jam.ProjectSync.RetryAdvice == "" {
		t.Fatalf("failure state not visible: %+v", jam.ProjectSync)
	}
}

func TestJamProjectSyncAuthRestrictsGitHubToken(t *testing.T) {
	const githubToken = "gh-secret"
	const customToken = "custom-secret"
	t.Setenv("GITHUB_TOKEN", githubToken)
	t.Setenv(jamProjectSyncTokenEnv, customToken)

	cases := []struct {
		name      string
		endpoint  string
		wantToken string
		wantErr   bool
	}{
		{name: "default github endpoint", endpoint: jamProjectSyncDefaultURL, wantToken: githubToken},
		{name: "custom https endpoint gets custom token", endpoint: "https://sync.example.com/graphql", wantToken: customToken},
		{name: "github host over http is not trusted", endpoint: "http://api.github.com/graphql", wantErr: true},
		{name: "plain http to remote host rejected", endpoint: "http://sync.example.com/graphql", wantErr: true},
		{name: "loopback http allowed", endpoint: "http://127.0.0.1:8080/graphql", wantToken: customToken},
		{name: "localhost http allowed", endpoint: "http://localhost:8080/graphql", wantToken: customToken},
		{name: "unsupported scheme rejected", endpoint: "ftp://sync.example.com/graphql", wantErr: true},
		{name: "invalid url rejected", endpoint: "not a url", wantErr: true},
		{name: "github host on a non-default port gets custom token, not GITHUB_TOKEN", endpoint: "https://api.github.com:8443/other", wantToken: customToken},
		{name: "github host with userinfo gets custom token, not GITHUB_TOKEN", endpoint: "https://user@api.github.com/graphql", wantToken: customToken},
		{name: "github host with a different path gets custom token, not GITHUB_TOKEN", endpoint: "https://api.github.com/other", wantToken: customToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, token, err := jamProjectSyncAuth(tc.endpoint)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("jamProjectSyncAuth(%q) err = nil, want error", tc.endpoint)
				}
				return
			}
			if err != nil {
				t.Fatalf("jamProjectSyncAuth(%q) err = %v", tc.endpoint, err)
			}
			if token != tc.wantToken {
				t.Fatalf("jamProjectSyncAuth(%q) token = %q, want %q", tc.endpoint, token, tc.wantToken)
			}
		})
	}
}

func TestJamProjectSyncAuthRequiresGitHubTokenForGitHub(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	if _, _, err := jamProjectSyncAuth(jamProjectSyncDefaultURL); err == nil {
		t.Fatal("jamProjectSyncAuth without GITHUB_TOKEN err = nil, want error")
	}
}

func TestCampaignJamProjectSyncDoesNotForwardGitHubTokenToCustomEndpoint(t *testing.T) {
	s := jamTestServer(t)
	authHeader := make(chan string, 1)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader <- r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":{"hiveJamProjectSync":{"ok":true}}}`))
	}))
	t.Cleanup(api.Close)
	t.Setenv(jamProjectSyncEndpointEnv, api.URL)
	t.Setenv("GITHUB_TOKEN", "gh-secret")
	t.Setenv(jamProjectSyncTokenEnv, "")

	enable := jamPostAs(t, s, "/api/campaigns/spec-project-token/jam/project-sync", "owner", "maintainer", map[string]any{"action": "enable", "project_id": "PVT_token"}, true)
	if enable.Code != http.StatusOK {
		t.Fatalf("enable = %d body=%s", enable.Code, enable.Body.String())
	}
	sync := jamPostAs(t, s, "/api/campaigns/spec-project-token/jam/project-sync", "owner", "maintainer", map[string]any{"action": "sync"}, true)
	if sync.Code != http.StatusOK {
		t.Fatalf("sync = %d body=%s", sync.Code, sync.Body.String())
	}
	if got := <-authHeader; got != "" {
		t.Fatalf("custom endpoint received Authorization %q, want none", got)
	}
}

func TestPostGitHubProjectDraftsUsesProjectsV2Mutation(t *testing.T) {
	var queries []string
	var inputs []map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string `json:"query"`
			Variables struct {
				Input map[string]any `json:"input"`
			} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		queries = append(queries, body.Query)
		inputs = append(inputs, body.Variables.Input)
		_, _ = w.Write([]byte(`{"data":{"addProjectV2DraftIssue":{"projectItem":{"id":"PVTI_1"}}}}`))
	}))
	t.Cleanup(api.Close)

	payload := campaignProjectSyncPayload{ProjectID: "PVT_x", Items: []CampaignProjectItem{
		{Type: "spec", Title: "Campaign spec c", Body: "body"},
		{Type: "decision", Title: "Q?"},
	}}
	if err := postGitHubProjectDrafts(api.URL, "tok", payload); err != nil {
		t.Fatalf("postGitHubProjectDrafts: %v", err)
	}
	if len(queries) != 2 || !strings.Contains(queries[0], "addProjectV2DraftIssue") || strings.Contains(queries[0], "hiveJamProjectSync") {
		t.Fatalf("queries = %v, want two addProjectV2DraftIssue mutations", queries)
	}
	if inputs[0]["projectId"] != "PVT_x" || inputs[0]["title"] != "Campaign spec c" || inputs[0]["body"] != "body" {
		t.Fatalf("input[0] = %v", inputs[0])
	}
	if err := postGitHubProjectDrafts(api.URL, "tok", campaignProjectSyncPayload{}); err == nil {
		t.Fatal("missing project_id err = nil, want error")
	}
}

// TestPostGitHubProjectDraftsRejectsNonNodeIDProjectID covers
// hivecommons/hive#10086: a GitHub Projects v2 node id is always "PVT_…"; a
// project number or a project URL pasted into project_id by mistake must be
// rejected locally with an actionable message instead of reaching the real
// API only to come back as an opaque GraphQL error.
func TestPostGitHubProjectDraftsRejectsNonNodeIDProjectID(t *testing.T) {
	var calls int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"data":{"addProjectV2DraftIssue":{"projectItem":{"id":"PVTI_1"}}}}`))
	}))
	t.Cleanup(api.Close)

	cases := []string{"7", "https://github.com/orgs/hivecommons/projects/7", "PVT"}
	for _, projectID := range cases {
		payload := campaignProjectSyncPayload{ProjectID: projectID, Items: []CampaignProjectItem{{Type: "spec", Title: "t"}}}
		if err := postGitHubProjectDrafts(api.URL, "tok", payload); err == nil {
			t.Fatalf("project_id %q err = nil, want rejection", projectID)
		}
	}
	if calls != 0 {
		t.Fatalf("rejected project_id still reached the API %d times", calls)
	}
}

// TestCampaignJamProjectSyncStoreFailuresReport5xx covers hivecommons/hive#10083:
// the project-sync GET and POST handlers loaded/mutated the shared Jam store
// the same way the thread and agent-invite handlers do, but still reported
// every store error (corrupt campaign-jam.json) as a client-error 400 instead
// of deferring to campaignJamStatus like api_campaigns_jam_agents.go was
// fixed to do in #10268. A corrupt store is a server-side fault, not a bad
// request, for every Jam endpoint — including project sync.
func TestCampaignJamProjectSyncStoreFailuresReport5xx(t *testing.T) {
	s := jamTestServer(t)
	seed := jamPostAs(t, s, "/api/campaigns/spec-sync-store/jam", "read-write", "alice", map[string]any{
		"spec_content": "## Goals\nShip together",
	}, false)
	if seed.Code != http.StatusOK {
		t.Fatalf("seed = %d body=%s", seed.Code, seed.Body.String())
	}
	enable := jamPostAs(t, s, "/api/campaigns/spec-sync-store/jam/project-sync", "owner", "maintainer", map[string]any{
		"action":      "enable",
		"project_url": "https://github.com/orgs/hivecommons/projects/7",
	}, true)
	if enable.Code != http.StatusOK {
		t.Fatalf("enable sync = %d body=%s", enable.Code, enable.Body.String())
	}

	path, err := s.campaignJamPath()
	if err != nil {
		t.Fatalf("jam path: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"campaigns": {"spec`), 0o600); err != nil {
		t.Fatalf("truncate jam store: %v", err)
	}

	get := doOwnerGet(s, "/api/campaigns/spec-sync-store/jam/project-sync")
	if get.Code != http.StatusInternalServerError {
		t.Fatalf("project-sync get on corrupt store = %d body=%s, want 500", get.Code, get.Body.String())
	}

	disablePost := jamPostAs(t, s, "/api/campaigns/spec-sync-store/jam/project-sync", "owner", "maintainer", map[string]any{
		"action": "disable",
	}, true)
	if disablePost.Code != http.StatusInternalServerError {
		t.Fatalf("project-sync disable on corrupt store = %d body=%s, want 500", disablePost.Code, disablePost.Body.String())
	}

	syncPost := jamPostAs(t, s, "/api/campaigns/spec-sync-store/jam/project-sync", "owner", "maintainer", map[string]any{
		"action": "sync",
	}, true)
	if syncPost.Code != http.StatusInternalServerError {
		t.Fatalf("project-sync sync on corrupt store = %d body=%s, want 500", syncPost.Code, syncPost.Body.String())
	}
}

// TestJamProjectSyncUsesProjectsV2ForEnterpriseEndpoints covers the rest of
// hivecommons/hive#10086: a GitHub Enterprise Server GraphQL endpoint
// (https://<host>/api/graphql) is a real GitHub API with the Projects v2
// schema and no hiveJamProjectSync field, so an override pointing at one must
// publish draft issues the same way the canonical endpoint does instead of
// posting the hive's own mutation. Endpoints that are not GitHub GraphQL
// APIs keep the previous behaviour.
func TestJamProjectSyncUsesProjectsV2ForEnterpriseEndpoints(t *testing.T) {
	projectsV2 := []string{
		"https://api.github.com/graphql",
		"https://ghe.example.com/api/graphql",
		"https://github.example.com/api/graphql",
	}
	for _, raw := range projectsV2 {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", raw, err)
		}
		if !jamProjectSyncUsesProjectsV2(u) {
			t.Fatalf("%s treated as a non-GitHub endpoint, want the Projects v2 mutation", raw)
		}
	}
	custom := []string{
		"http://127.0.0.1:8080",
		"https://relay.example.com/jam-sync",
		"https://user:pass@ghe.example.com/api/graphql",
		"https://ghe.example.com/graphql",
	}
	for _, raw := range custom {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", raw, err)
		}
		if jamProjectSyncUsesProjectsV2(u) {
			t.Fatalf("%s treated as a GitHub GraphQL endpoint", raw)
		}
	}
}

// TestJamProjectSyncEnterpriseEndpointKeepsItsOwnToken pins the token binding
// that #10086's routing change must not disturb: an enterprise GraphQL
// override now gets the Projects v2 mutation, but still only ever receives
// HIVE_JAM_PROJECT_SYNC_TOKEN, never GITHUB_TOKEN (#8811).
func TestJamProjectSyncEnterpriseEndpointKeepsItsOwnToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "gh-secret")
	t.Setenv(jamProjectSyncTokenEnv, "custom-token")
	endpoint, token, err := jamProjectSyncAuth("https://ghe.example.com/api/graphql")
	if err != nil {
		t.Fatalf("jamProjectSyncAuth: %v", err)
	}
	if endpoint != "https://ghe.example.com/api/graphql" {
		t.Fatalf("endpoint = %q", endpoint)
	}
	if token != "custom-token" {
		t.Fatalf("token = %q, want the custom endpoint token", token)
	}
}
