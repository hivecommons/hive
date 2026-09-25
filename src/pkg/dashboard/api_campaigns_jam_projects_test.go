package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
