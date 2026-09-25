package dashboard

import (
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestCampaignJamAgentInviteCreatesAttributedReplyAndSuggestion(t *testing.T) {
	s := jamTestServer(t)
	s.deps.Config.Agents["architect"] = config.AgentConfig{Model: "gpt-5.4", Enabled: true}
	thread := jamPostAs(t, s, "/api/campaigns/spec-agent/jam/threads", "read-write", "alice", map[string]any{
		"section": "Goals",
		"body":    "Can an agent draft the reliability goal?",
	}, false)
	if thread.Code != http.StatusOK {
		t.Fatalf("thread create = %d body=%s", thread.Code, thread.Body.String())
	}
	jam := decodeJam(t, thread)

	invite := jamPostAs(t, s, "/api/campaigns/spec-agent/jam/agents", "owner", "maintainer", map[string]any{
		"thread_id":     jam.Threads[0].ID,
		"agent":         "architect",
		"reply":         "Add a reliability target and owner.",
		"proposed_text": "Reliability target: 99.9% availability with an on-call owner.",
	}, true)
	if invite.Code != http.StatusOK {
		t.Fatalf("agent invite = %d body=%s", invite.Code, invite.Body.String())
	}
	jam = decodeJam(t, invite)
	if got := jam.Threads[0].Comments[len(jam.Threads[0].Comments)-1].Author; got.Type != "agent" || got.Name != "architect" || got.Model != "gpt-5.4" {
		t.Fatalf("agent comment attribution = %+v", got)
	}
	if len(jam.Suggestions) != 1 || jam.Suggestions[0].Status != jamSuggestionOpen || jam.Suggestions[0].Author.Agent != "architect" {
		t.Fatalf("agent suggestion = %+v", jam.Suggestions)
	}
	if len(jam.Revisions) != 0 {
		t.Fatalf("agent invite must not auto-apply revisions: %+v", jam.Revisions)
	}
}

func TestCampaignJamAgentInvitePermissionBoundaries(t *testing.T) {
	s := jamTestServer(t)
	thread := jamPostAs(t, s, "/api/campaigns/spec-agent-perms/jam/threads", "read-write", "alice", map[string]any{
		"section": "Scope",
		"body":    "Ask for help",
	}, false)
	if thread.Code != http.StatusOK {
		t.Fatalf("thread create = %d body=%s", thread.Code, thread.Body.String())
	}
	jam := decodeJam(t, thread)
	body := map[string]any{"thread_id": jam.Threads[0].ID, "agent": "spektacular"}
	notMaintainer := jamPostAs(t, s, "/api/campaigns/spec-agent-perms/jam/agents", "read-write", "alice", body, false)
	if notMaintainer.Code != http.StatusForbidden {
		t.Fatalf("read-write invite = %d body=%s", notMaintainer.Code, notMaintainer.Body.String())
	}
	unknown := jamPostAs(t, s, "/api/campaigns/spec-agent-perms/jam/agents", "owner", "maintainer", map[string]any{
		"thread_id": jam.Threads[0].ID,
		"agent":     "missing-agent",
	}, true)
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("unknown agent invite = %d body=%s", unknown.Code, unknown.Body.String())
	}
	allowed := jamPostAs(t, s, "/api/campaigns/spec-agent-perms/jam/agents", "owner", "maintainer", body, true)
	if allowed.Code != http.StatusOK {
		t.Fatalf("spektacular invite = %d body=%s", allowed.Code, allowed.Body.String())
	}
}
