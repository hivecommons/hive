package adminmcp

import (
	"context"
	"errors"
	"testing"
)

func TestCheckRequestPath(t *testing.T) {
	for _, path := range []string{
		"/api/status",
		"/api/status/summary",
		"/api/kick/scanner/status",
		"/api/agents/team%2Fscanner/log",
		"/api/model/scanner/kiro-api-key%2Fclaude-opus-5:high",
		"/api/runs/org%2Frepo%237/detail",
		"/api/overview/issues.json?repo=org%2Frepo&stale=true",
		"/api/logs/read?include=..&exclude=.",
		"/api/config/agent/.hidden/prompt",
		"/api/config/agent/a..b/prompt",
		"/api/config/agent/.../prompt",
		"/api/agents/%252e%252e/log",
	} {
		if err := CheckRequestPath(path); err != nil {
			t.Errorf("CheckRequestPath(%q) = %v, want it allowed", path, err)
		}
	}
	for _, path := range []string{
		"/api/kick/../status",
		"/api/kick/./status",
		"/api/model/../self-upgrade",
		"/api/agents/..",
		"/api/agents/.",
		"/api/config/agent/%2e%2e/prompt",
		"/api/config/agent/%2E./prompt",
		"/api/repos//repo/items/1/hold",
		"/api/pause/",
		"/api/plan/../approve?x=1",
		"api/status",
		"",
	} {
		err := CheckRequestPath(path)
		if !errors.Is(err, ErrRequestPath) {
			t.Errorf("CheckRequestPath(%q) = %v, want ErrRequestPath", path, err)
		}
	}
}

// A name of "." or ".." in a write's arguments was escaped into the request
// path as it was, and the path then named a different endpoint:
// agent.model with agent ".." and model "self-upgrade" asked for
// POST /api/model/../self-upgrade, which is POST /api/self-upgrade.
func TestWritePreviewRefusesDotNames(t *testing.T) {
	for _, tc := range []struct {
		op   string
		args map[string]any
	}{
		{WriteOpAgentModel, map[string]any{"agent": "..", "model": "self-upgrade"}},
		{WriteOpAgentBackend, map[string]any{"agent": "..", "backend": "backup"}},
		{WriteOpAgentEffort, map[string]any{"agent": "..", "effort": "release-channel"}},
		{WriteOpAgentModel, map[string]any{"agent": "scanner", "model": ".."}},
		{WriteOpAgentPause, map[string]any{"agent": ".."}},
		{WriteOpAgentResume, map[string]any{"agent": "."}},
		{WriteOpAgentNudge, map[string]any{"agent": "..", "prompt": "hello"}},
		{WriteOpAgentRestart, map[string]any{"agent": ".."}},
		{WriteOpAgentRemove, map[string]any{"agent": ".."}},
		{WriteOpAgentInteractionTier, map[string]any{"agent": "..", "tier": "ADVISORY"}},
		{WriteOpPlanApprove, map[string]any{"epic_id": ".."}},
		{WriteOpPlanReject, map[string]any{"epic_id": "."}},
		{WriteOpRepositoryItemHold, map[string]any{"owner": "..", "repo": "repo", "number": float64(1), "held": true}},
		{WriteOpRepositoryItemHold, map[string]any{"owner": "org", "repo": "..", "number": float64(1), "held": true}},
		{WriteOpContributorTrust, map[string]any{"contributor_id": "..", "tier": "trusted"}},
		{WriteOpContributorAgentRole, map[string]any{"contributor_id": "..", "agent_role": "architect"}},
		{WriteOpContributorRoleGrants, map[string]any{"contributor_id": "..", "agent_role_grants": []any{"architect"}}},
		{WriteOpContributorRevoke, map[string]any{"contributor_id": ".."}},
		{WriteOpContributorRequeue, map[string]any{"contributor_id": ".."}},
		{WriteOpContributorDelete, map[string]any{"contributor_id": ".."}},
	} {
		provider := &writeProvider{}
		store := NewMemoryPendingStore()
		h := NewHandler(provider, WithWritesEnabled(true), WithPendingStore(store), WithHiveID("hive-a"))
		_, err := h.PreviewWrite(context.Background(), map[string]any{"operation": tc.op, "args": tc.args})
		if !errors.Is(err, ErrRequestPath) {
			t.Errorf("%s %v: preview = %v, want ErrRequestPath", tc.op, tc.args, err)
		}
		if pending, _ := store.List(context.Background()); len(pending) != 0 {
			t.Errorf("%s %v: a confirmation was stored for a refused path", tc.op, tc.args)
		}
	}
}

// A confirmation that was stored before the check existed is not executed.
func TestWriteConfirmRefusesStoredDotPath(t *testing.T) {
	provider := &writeProvider{}
	store := NewMemoryPendingStore()
	h := NewHandler(provider, WithWritesEnabled(true), WithPendingStore(store), WithHiveID("hive-a"))
	args := map[string]any{"agent": "..", "model": "self-upgrade"}
	preview, err := agentModelOp{}.Preview(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	now := h.now()
	if err := store.Put(context.Background(), PendingConfirmation{ID: "old", Operation: WriteOpAgentModel, Args: args, Preview: preview, Hive: "hive-a", CreatedAt: now, ExpiresAt: now.Add(DefaultConfirmationTTL)}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": "old"}); !errors.Is(err, ErrRequestPath) {
		t.Fatalf("confirm = %v, want ErrRequestPath", err)
	}
	if len(provider.requests) != 0 {
		t.Fatalf("a refused path was executed: %+v", provider.requests)
	}
}

// Ordinary names, including ones that need escaping, are not affected.
func TestWritePreviewStillAcceptsOrdinaryNames(t *testing.T) {
	for _, tc := range []struct {
		op   string
		args map[string]any
		path string
	}{
		{WriteOpAgentModel, map[string]any{"agent": "team/scanner", "model": "kiro-api-key/claude-opus-5:high"}, "/api/model/team%2Fscanner/kiro-api-key%2Fclaude-opus-5:high"},
		{WriteOpAgentPause, map[string]any{"agent": ".hidden"}, "/api/pause/.hidden"},
		{WriteOpPlanApprove, map[string]any{"epic_id": "epic..1"}, "/api/plan/epic..1/approve"},
		{WriteOpRepositoryItemHold, map[string]any{"owner": "org", "repo": ".github", "number": float64(3), "held": true}, "/api/repos/org/.github/items/3/hold"},
	} {
		h := NewHandler(&writeProvider{}, WithWritesEnabled(true), WithHiveID("hive-a"))
		previewed, err := h.PreviewWrite(context.Background(), map[string]any{"operation": tc.op, "args": tc.args})
		if err != nil {
			t.Errorf("%s %v: %v", tc.op, tc.args, err)
			continue
		}
		if got := previewed.(map[string]any)["preview"].(WritePreview).Request.Path; got != tc.path {
			t.Errorf("%s: path = %s, want %s", tc.op, got, tc.path)
		}
	}
}
