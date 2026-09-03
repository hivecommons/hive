package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/scheduler"
)

func roleTestHub(t *testing.T) (*ContributeWSHub, *Server) {
	t.Helper()
	hub, s := covK2Hub(t)
	cfg := s.deps.Config
	cfg.Agents["quality"] = config.AgentConfig{Role: "quality", Enabled: true, LaneKeywords: []string{"test-coverage", "missing-tests"}}
	cfg.Agents["scanner"] = config.AgentConfig{Role: "scanner", Enabled: true}
	cfg.Agents["ci-maintainer"] = config.AgentConfig{Role: "ci-maintainer", Enabled: true}
	cfg.Hub.ContributeDelegatableRoles = nil
	s.deps.Scheduler = scheduler.New(cfg, s.logger)
	return hub, s
}

func TestAgentRoleClaimGatingMatrix(t *testing.T) {
	hub, _ := roleTestHub(t)
	cases := []struct {
		name   string
		tier   string
		role   string
		grants []string
		allow  []string
		want   bool
	}{
		{name: "absent role is current behavior", tier: "newcomer", want: true},
		{name: "newcomer cannot claim default role", tier: "newcomer", role: "scanner", want: false},
		{name: "contributor can claim safe default role", tier: "contributor", role: "scanner", want: true},
		{name: "trusted can claim safe default role", tier: "trusted", role: "quality", want: true},
		{name: "privileged role disabled by default", tier: "trusted", role: "ci-maintainer", grants: []string{"ci-maintainer"}, want: false},
		{name: "privileged role needs grant", tier: "trusted", role: "ci-maintainer", allow: []string{"ci-maintainer"}, want: false},
		{name: "privileged role needs trusted plus grant", tier: "contributor", role: "ci-maintainer", allow: []string{"ci-maintainer"}, grants: []string{"ci-maintainer"}, want: false},
		{name: "trusted plus grant can claim privileged role", tier: "trusted", role: "ci-maintainer", allow: []string{"ci-maintainer"}, grants: []string{"ci-maintainer"}, want: true},
		{name: "supervisor never delegatable", tier: "merger", role: "supervisor", allow: []string{"supervisor"}, grants: []string{"supervisor"}, want: false},
		{name: "disabled role cannot be claimed", tier: "trusted", role: "quality", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub.server.deps.Config.Hub.ContributeDelegatableRoles = tc.allow
			if tc.name == "disabled role cannot be claimed" {
				agent := hub.server.deps.Config.Agents["quality"]
				agent.Enabled = false
				hub.server.deps.Config.Agents["quality"] = agent
				defer func() {
					agent.Enabled = true
					hub.server.deps.Config.Agents["quality"] = agent
				}()
			}
			conn := &ContributorConnection{
				profile: &ContributorProfile{
					GitHubUsername:  "alice",
					ContributorID:   "c-alice",
					TrustTier:       tc.tier,
					AgentRoleGrants: tc.grants,
				},
				role: tc.role,
			}
			got, _ := hub.roleClaimAllowed(conn, tc.role)
			if got != tc.want {
				t.Fatalf("roleClaimAllowed(%s/%s grants=%v allow=%v) = %v, want %v",
					tc.tier, tc.role, tc.grants, tc.allow, got, tc.want)
			}
		})
	}
}

func TestAgentRoleTaskPayloadCarriesRolePromptAndFiltersLane(t *testing.T) {
	hub, s := roleTestHub(t)
	setStatusIssues(s,
		map[string]any{"number": float64(1), "title": "ordinary bug", "author": "bob"},
		map[string]any{
			"number": float64(2),
			"title":  "missing test coverage for API",
			"author": "bob",
			"labels": []any{"test-coverage"},
			"lane":   "quality",
		},
	)
	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "contributor"},
		role:     "quality",
		lastPong: time.Now(),
	}
	msg := hub.selectTask(conn)
	if msg == nil || msg.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %+v", msg)
	}
	if msg.Number != 2 {
		t.Fatalf("quality role should receive quality-shaped work (#2), got #%d", msg.Number)
	}
	if msg.Role != "quality" {
		t.Fatalf("task payload role = %q, want quality", msg.Role)
	}
	if !strings.Contains(msg.Prompt, "[clanker-agent-role:quality]") || !strings.Contains(msg.Prompt, "[agent:quality]") {
		t.Fatalf("prompt did not carry clanker role and quality kick prompt:\n%s", msg.Prompt)
	}
}

func TestAgentRoleDoubleClaimUsesContributorLease(t *testing.T) {
	hub, s := roleTestHub(t)
	setStatusIssues(s, map[string]any{
		"number": float64(7),
		"title":  "missing-tests regression",
		"author": "bob",
		"labels": []any{"missing-tests"},
	})
	roleConn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "role", ContributorID: "c-role", TrustTier: "contributor"},
		role:     "quality",
		lastPong: time.Now(),
	}
	plainConn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "plain", ContributorID: "c-plain", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	hub.mu.Lock()
	hub.connections["role"] = roleConn
	hub.connections["plain"] = plainConn
	hub.mu.Unlock()
	first := hub.selectTask(roleConn)
	if first == nil || first.Type != "task_assign" {
		t.Fatalf("expected role clanker assignment, got %+v", first)
	}
	second := hub.selectTask(plainConn)
	if second == nil || second.Type != "task_unavailable" || second.Reason != taskUnavailableNoMatchingWork {
		t.Fatalf("same issue should be hidden by the active lease, got %+v", second)
	}
}

func TestAgentRoleAuditEntryRecordsContributorAndRole(t *testing.T) {
	hub, _ := roleTestHub(t)
	hub.addActivity("alice", "picked up", "scanner", "copilot", "gpt", "", "contributor ran scanner task: issue myorg/repo#1")
	entries := hub.RecentActivity()
	if len(entries) == 0 {
		t.Fatal("expected activity entry")
	}
	got := entries[len(entries)-1]
	if got.Username != "alice" || got.Role != "scanner" || !strings.Contains(got.Task, "contributor ran scanner task") {
		t.Fatalf("audit entry = %+v, want contributor attribution plus scanner role", got)
	}
}
