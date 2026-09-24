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
