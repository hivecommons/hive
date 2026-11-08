package dashboard

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/redmain"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/timeline"
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

// guidanceCountingScheduler is the real scheduler with a counter on the
// resident manual-kick builder, the call that resets the classifier budget and
// sweeps ClassifyAll / recordClassified / applyRunTriage over the cached queue.
type guidanceCountingScheduler struct {
	*scheduler.Scheduler
	residentKicks atomic.Int32
}

func (c *guidanceCountingScheduler) BuildAgentMessageFromLastActionable(agent string) string {
	c.residentKicks.Add(1)
	return c.Scheduler.BuildAgentMessageFromLastActionable(agent)
}

// residentOnlyRolePromptText must never reach a contributor: the work-list
// headings the resident kick renders (scheduler lane_messages.go and the
// shipped kick templates), the merge and dispatch instructions, the red-main
// notice, the closing instruction, and the queued items themselves.
var residentOnlyRolePromptText = []string{
	"MERGE-ELIGIBLE",
	"CI-FAILING",
	"ACTIONABLE ISSUES",
	"ACTIONABLE PRs",
	"hive-merge",
	"Begin now",
	"RED MAIN",
	"## Merge Sweep",
	"## Dispatching Fixes",
	"queued-elsewhere-issue",
	"queued-elsewhere-pr",
}

// installGuidanceCountingScheduler gives the hub a real scheduler whose cached
// queue holds other work (an issue, a PR, a red main) and a timeline store, so
// a resident kick built on assignment would show up three ways: the counter,
// classified journeys, and the queued titles in the prompt.
func installGuidanceCountingScheduler(t *testing.T, s *Server, agent, template string) (*guidanceCountingScheduler, *timeline.Store) {
	t.Helper()
	cfg := s.deps.Config
	a := cfg.Agents[agent]
	a.Role, a.Enabled, a.Mode, a.KickTemplate = agent, true, "ISSUES_AND_PRS", template
	cfg.Agents[agent] = a
	sched := scheduler.New(cfg, s.logger)
	store := timeline.NewStore()
	sched.SetLifecycleRecorder(store)
	sched.SetLastActionable(&ghpkg.ActionableResult{
		Issues: ghpkg.IssueResult{Count: 1, Items: []ghpkg.Issue{{Repo: "myorg/repo1", Number: 900, Title: "queued-elsewhere-issue"}}},
		PRs:    ghpkg.PRResult{Count: 1, Items: []ghpkg.PullRequest{{Repo: "myorg/repo1", Number: 901, Title: "queued-elsewhere-pr", Author: "someone"}}},
		RedMain: map[string]*redmain.Fact{"myorg/repo1": {
			Repo: "myorg/repo1", Branch: "main", Check: "unit", Since: time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC),
		}},
	})
	counting := &guidanceCountingScheduler{Scheduler: sched}
	s.deps.Scheduler = counting
	return counting, store
}

func assertGuidanceOnlyRolePrompt(t *testing.T, prompt, agent string, counting *guidanceCountingScheduler, store *timeline.Store) {
	t.Helper()
	if !strings.Contains(prompt, "[agent:"+agent+"]") || !strings.Contains(prompt, "HOUSE STYLE") {
		t.Errorf("prompt does not carry the %s role guidance", agent)
	}
	for _, banned := range residentOnlyRolePromptText {
		if strings.Contains(prompt, banned) {
			t.Errorf("role-delegated prompt carries resident-only text %q", banned)
		}
	}
	if n := counting.residentKicks.Load(); n != 0 {
		t.Errorf("assignment built %d resident manual kicks; want 0 (each one is a classification sweep)", n)
	}
	if n := len(store.Journeys(0)); n != 0 {
		t.Errorf("assignment classified %d queued issues; want none", n)
	}
	if t.Failed() {
		t.Logf("prompt:\n%s", prompt)
	}
}

// A role-delegated task carries the role's guidance and the one assigned task,
// framed so the task is the whole job — not the resident agent's kick with its
// work lists, merge sweep, dispatch steps, red-main notice and "Begin now".
// Building it makes no classification sweep.
func TestRoleDelegatedTaskCarriesGuidanceNotResidentWorkList(t *testing.T) {
	for _, tc := range []struct{ role, template string }{
		{role: "scanner", template: "scanner-automerge.md"},
		{role: "quality", template: "quality-full.md"},
	} {
		t.Run(tc.role, func(t *testing.T) {
			hub, s := roleTestHub(t)
			counting, store := installGuidanceCountingScheduler(t, s, tc.role, tc.template)
			setStatusIssues(s, map[string]any{
				"number": float64(7),
				"title":  "missing test coverage for the assigned-only API",
				"author": "bob",
				"labels": []any{"test-coverage"},
				"lane":   tc.role,
			})
			conn := &ContributorConnection{
				profile:  &ContributorProfile{GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "contributor"},
				role:     tc.role,
				lastPong: time.Now(),
			}
			msg := hub.selectTask(conn)
			if msg == nil || msg.Type != "task_assign" || msg.Number != 7 {
				t.Fatalf("expected task_assign for #7, got %+v", msg)
			}
			for _, want := range []string{
				"[clanker-agent-role:" + tc.role + "]",
				"Attribution remains the contributor's GitHub identity",
				"Your work for this session is ONLY the ASSIGNED CONTRIBUTOR TASK at the end",
				"ASSIGNED CONTRIBUTOR TASK (this is your whole job for this session):",
				"missing test coverage for the assigned-only API",
			} {
				if !strings.Contains(msg.Prompt, want) {
					t.Errorf("prompt is missing %q", want)
				}
			}
			if strings.Index(msg.Prompt, "ROLE GUIDANCE") > strings.Index(msg.Prompt, "ASSIGNED CONTRIBUTOR TASK") {
				t.Errorf("the assigned task must come after the role guidance")
			}
			assertGuidanceOnlyRolePrompt(t, msg.Prompt, tc.role, counting, store)
		})
	}
}

// Donated standby work carries the same guidance-only lane policy, after the
// task and framed so the task stays the whole job, and building it makes no
// classification sweep either.
func TestStandbyDispatchCarriesGuidanceNotResidentWorkList(t *testing.T) {
	cfg := standbyAutoDispatchConfig(true, "T2", 1)
	s, conn := standbyAutoDispatchHarness(t, cfg)
	counting, store := installGuidanceCountingScheduler(t, s, "quality", "quality-full.md")

	s.contributeHub.AutoDispatchStandby([]string{"quality"})
	msg := readMsg(t, conn)
	if msg.Type != "task_assign" {
		t.Fatalf("auto dispatch msg = %s: %s", msg.Type, msg.Reason)
	}
	const lanePolicy = " Lane policy (how the lane works; your work is still ONLY the task above"
	if at := strings.Index(msg.Prompt, lanePolicy); at < 0 {
		t.Errorf("standby prompt has no framed lane policy")
	} else if !strings.Contains(msg.Prompt[:at], "tiny standby item") {
		t.Errorf("the standby task must come before the lane policy")
	}
	assertGuidanceOnlyRolePrompt(t, msg.Prompt, "quality", counting, store)
}

// installTaskRepoKnowledge gives the hive a primary repo and the assigned
// task's repo, each checked out with its own AGENTS.md, and a knowledge vault
// with one fact about the assigned task, so a role prompt shows which repo's
// instructions it carries and whether knowledge was primed from the task.
func installTaskRepoKnowledge(t *testing.T, s *Server, sched *scheduler.Scheduler, taskRepo string) {
	t.Helper()
	checkouts := t.TempDir()
	for name, text := range map[string]string{
		"primary-repo":          "PRIMARY-REPO-INSTRUCTION: this is the role's primary repo.",
		filepath.Base(taskRepo): "TASK-REPO-INSTRUCTION: this is the assigned task's repo.",
	} {
		if err := os.MkdirAll(filepath.Join(checkouts, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(checkouts, name, "AGENTS.md"), []byte("# Agent notes\n\n"+text+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := s.deps.Config
	cfg.Project.PrimaryRepo = "primary-repo"
	cfg.Project.CheckoutsDir = checkouts

	vault := t.TempDir()
	fact := "---\ntitle: Standby coverage for the assigned-only API\ntype: gotcha\nlayer: project\n---\n" +
		"TASK-KNOWLEDGE: tiny standby items and assigned-only API coverage share one fixture.\n"
	if err := os.WriteFile(filepath.Join(vault, "task-fact.md"), []byte(fact), 0o644); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := knowledge.NewFileStore(vault, "vault", logger)
	if err != nil {
		t.Fatal(err)
	}
	primer := knowledge.NewPrimer(nil, knowledge.PrimerConfig{MaxFacts: 25}, logger)
	primer.AddFileStore("vault", store, knowledge.LayerProject)
	sched.SetPrimer(primer)
}

func assertTaskRepoRolePrompt(t *testing.T, prompt, agent string, sched *scheduler.Scheduler) {
	t.Helper()
	for _, want := range []string{"# Relevant Knowledge", "TASK-KNOWLEDGE:", "TASK-REPO-INSTRUCTION"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("role prompt is missing %q", want)
		}
	}
	if strings.Contains(prompt, "PRIMARY-REPO-INSTRUCTION") {
		t.Errorf("role prompt carries the role's primary repo AGENTS.md instead of the task repo's")
	}
	if _, ok := sched.KickSummary(agent); ok {
		t.Errorf("building the role prompt recorded a kick summary for the resident %s", agent)
	}
	if slugs := sched.KickPrimedSlugs(agent); slugs != nil {
		t.Errorf("building the role prompt recorded primed slugs %v for the resident %s", slugs, agent)
	}
}

// A role-delegated task's guidance carries the AGENTS.md of the repository the
// task is in, not the role's primary repo, and the "# Relevant Knowledge"
// primed from that one issue — still with no resident work lists, no
// classification sweep and no kick summary for the resident agent.
func TestRoleDelegatedTaskGuidanceCarriesTaskRepoAndIssueKnowledge(t *testing.T) {
	for _, tc := range []struct{ role, template string }{
		{role: "scanner", template: "scanner-automerge.md"},
		{role: "quality", template: "quality-full.md"},
	} {
		t.Run(tc.role, func(t *testing.T) {
			hub, s := roleTestHub(t)
			counting, store := installGuidanceCountingScheduler(t, s, tc.role, tc.template)
			installTaskRepoKnowledge(t, s, counting.Scheduler, "myorg/repo1")
			setStatusIssues(s, map[string]any{
				"number": float64(7),
				"title":  "missing test coverage for the assigned-only API",
				"author": "bob",
				"labels": []any{"test-coverage"},
				"lane":   tc.role,
			})
			conn := &ContributorConnection{
				profile:  &ContributorProfile{GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "contributor"},
				role:     tc.role,
				lastPong: time.Now(),
			}
			msg := hub.selectTask(conn)
			if msg == nil || msg.Type != "task_assign" || msg.Number != 7 || msg.Repo != "myorg/repo1" {
				t.Fatalf("expected task_assign for myorg/repo1#7, got %+v", msg)
			}
			assertTaskRepoRolePrompt(t, msg.Prompt, tc.role, counting.Scheduler)
			assertGuidanceOnlyRolePrompt(t, msg.Prompt, tc.role, counting, store)
		})
	}
}

// Donated standby work gets the same task-repo guidance.
func TestStandbyDispatchGuidanceCarriesTaskRepoAndIssueKnowledge(t *testing.T) {
	cfg := standbyAutoDispatchConfig(true, "T2", 1)
	s, conn := standbyAutoDispatchHarness(t, cfg)
	counting, store := installGuidanceCountingScheduler(t, s, "quality", "quality-full.md")
	installTaskRepoKnowledge(t, s, counting.Scheduler, "alice/repo")

	s.contributeHub.AutoDispatchStandby([]string{"quality"})
	msg := readMsg(t, conn)
	if msg.Type != "task_assign" || msg.Repo != "alice/repo" {
		t.Fatalf("auto dispatch msg = %s (%s): %s", msg.Type, msg.Repo, msg.Reason)
	}
	assertTaskRepoRolePrompt(t, msg.Prompt, "quality", counting.Scheduler)
	assertGuidanceOnlyRolePrompt(t, msg.Prompt, "quality", counting, store)
}
