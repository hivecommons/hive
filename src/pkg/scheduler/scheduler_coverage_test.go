package scheduler

import (
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// ---------------------------------------------------------------------------
// formatIssueList
// ---------------------------------------------------------------------------

func TestFormatIssueList_Empty(t *testing.T) {
	s := newScheduler()
	result := s.formatIssueList(nil)
	if result != "(none)" {
		t.Errorf("got %q, want (none)", result)
	}
}

func TestFormatIssueList_SingleIssue(t *testing.T) {
	s := newScheduler()
	issues := []github.Issue{
		{Repo: "repo1", Number: 42, Title: "fix bug", AgeMinutes: 15, Labels: []string{"kind/bug"}},
	}
	result := s.formatIssueList(issues)
	if !strings.Contains(result, "15m") {
		t.Errorf("expected age in output: %s", result)
	}
	if !strings.Contains(result, "repo1#42") {
		t.Errorf("expected repo#number in output: %s", result)
	}
	if !strings.Contains(result, "kind/bug") {
		t.Errorf("expected label in output: %s", result)
	}
}

func TestFormatIssueList_TruncatesTitle(t *testing.T) {
	s := newScheduler()
	longTitle := strings.Repeat("x", 80) // longer than maxTitleLen=60
	issues := []github.Issue{
		{Repo: "repo1", Number: 1, Title: longTitle, AgeMinutes: 5, Labels: []string{"test"}},
	}
	result := s.formatIssueList(issues)
	// The truncated title should be exactly 60 chars
	if strings.Contains(result, longTitle) {
		t.Error("expected title to be truncated")
	}
}

func TestFormatIssueList_MaxIssues(t *testing.T) {
	s := newScheduler()
	issueCount := maxIssuesPerKick + 10
	issues := make([]github.Issue, issueCount)
	for i := range issues {
		issues[i] = github.Issue{Repo: "r", Number: i + 1, Title: "issue", Labels: []string{}}
	}
	result := s.formatIssueList(issues)
	lines := strings.Split(strings.TrimSpace(result), "\n")
	if len(lines) > maxIssuesPerKick {
		t.Errorf("expected at most %d lines, got %d", maxIssuesPerKick, len(lines))
	}
}

// ---------------------------------------------------------------------------
// formatPRList
// ---------------------------------------------------------------------------

func TestFormatPRList_Empty(t *testing.T) {
	s := newScheduler()
	actionable := &github.ActionableResult{}
	result := s.formatPRList(actionable)
	if result != "(none)" {
		t.Errorf("got %q, want (none)", result)
	}
}

func TestFormatPRList_SinglePR(t *testing.T) {
	s := newScheduler()
	actionable := &github.ActionableResult{
		PRs: github.PRResult{
			Count: 1,
			Items: []github.PullRequest{
				{Repo: "repo1", Number: 99, Title: "feat: new thing", Author: "user1"},
			},
		},
	}
	result := s.formatPRList(actionable)
	if !strings.Contains(result, "repo1#99") {
		t.Errorf("expected repo#number in output: %s", result)
	}
	if !strings.Contains(result, "@user1") {
		t.Errorf("expected author in output: %s", result)
	}
}

func TestFormatPRList_TruncatesTitle(t *testing.T) {
	s := newScheduler()
	longTitle := strings.Repeat("y", 80)
	actionable := &github.ActionableResult{
		PRs: github.PRResult{
			Count: 1,
			Items: []github.PullRequest{
				{Repo: "repo1", Number: 1, Title: longTitle, Author: "a"},
			},
		},
	}
	result := s.formatPRList(actionable)
	if strings.Contains(result, longTitle) {
		t.Error("expected title to be truncated at 70 chars")
	}
}

func TestFormatMergeEligibleDataShowsQueuedMarker(t *testing.T) {
	result := formatMergeEligibleData([]byte(`{"merge_eligible":[{"number":7,"repo":"acme/widget","title":"fix it","queued":true}]}`))
	if !strings.Contains(result, "#7 acme/widget [queued for auto-merge]") {
		t.Fatalf("queued marker missing from merge-eligible output: %s", result)
	}
}

// ---------------------------------------------------------------------------
// substituteTemplate
// ---------------------------------------------------------------------------

func TestSubstituteTemplate_BasicVars(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{
			Org:         "myorg",
			Name:        "MyProject",
			PrimaryRepo: "main-repo",
			AIAuthor:    "ai-bot",
			Repos:       []string{"repo1", "repo2"},
		},
	}
	s := New(cfg, testLogger())

	actionable := &github.ActionableResult{
		Issues: github.IssueResult{Count: 5, SLAViolations: 2},
		PRs:    github.PRResult{Count: 3},
		Hold:   github.HoldResult{Total: 1},
	}

	template := "Agent: ${AGENT_NAME}, Issues: ${QUEUE_ISSUES}, PRs: ${QUEUE_PRS}, Hold: ${QUEUE_HOLD}, SLA: ${SLA_VIOLATIONS}"
	result := s.substituteTemplate(template, actionable, "scanner", nil)

	if !strings.Contains(result, "Agent: scanner") {
		t.Errorf("expected agent name substitution: %s", result)
	}
	if !strings.Contains(result, "Issues: 5") {
		t.Errorf("expected issue count: %s", result)
	}
	if !strings.Contains(result, "PRs: 3") {
		t.Errorf("expected PR count: %s", result)
	}
	if !strings.Contains(result, "Hold: 1") {
		t.Errorf("expected hold count: %s", result)
	}
	if !strings.Contains(result, "SLA: 2") {
		t.Errorf("expected SLA violations: %s", result)
	}
}

func TestSubstituteTemplate_ProjectVars(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{
			Org:         "testorg",
			Name:        "TestProj",
			PrimaryRepo: "core",
			AIAuthor:    "ai-author",
			Repos:       []string{"core", "docs"},
		},
	}
	s := New(cfg, testLogger())

	actionable := &github.ActionableResult{}
	template := "Org: ${PROJECT_ORG}, Name: ${PROJECT_NAME}, Repo: ${PROJECT_PRIMARY_REPO}, Author: ${PROJECT_AI_AUTHOR}, Repos: ${PROJECT_REPOS_LIST}"
	result := s.substituteTemplate(template, actionable, "test", nil)

	if !strings.Contains(result, "Org: testorg") {
		t.Errorf("expected org: %s", result)
	}
	if !strings.Contains(result, "Name: TestProj") {
		t.Errorf("expected project name: %s", result)
	}
	if !strings.Contains(result, "Repo: testorg/core") {
		t.Errorf("expected primary repo: %s", result)
	}
	if !strings.Contains(result, "Author: ai-author") {
		t.Errorf("expected AI author: %s", result)
	}
	if !strings.Contains(result, "Repos: core, docs") {
		t.Errorf("expected repos list: %s", result)
	}
}

func TestSubstituteTemplate_IssueAndPRLists(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{
			Org:   "org",
			Repos: []string{"r"},
		},
	}
	s := New(cfg, testLogger())

	actionable := &github.ActionableResult{
		PRs: github.PRResult{
			Count: 1,
			Items: []github.PullRequest{{Repo: "r", Number: 5, Title: "pr title", Author: "u"}},
		},
	}
	issues := []github.Issue{
		{Repo: "r", Number: 1, Title: "issue title", Labels: []string{"bug"}, AgeMinutes: 10, Lane: "scanner"},
	}

	template := "Issues:\n${ISSUE_LIST}\nPRs:\n${PR_LIST}"
	result := s.substituteTemplate(template, actionable, "scanner", issues)

	if !strings.Contains(result, "r#1") {
		t.Errorf("expected issue in output: %s", result)
	}
	if !strings.Contains(result, "r#5") {
		t.Errorf("expected PR in output: %s", result)
	}
}

func TestSubstituteTemplate_SpecialRepoVars(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{
			Org:         "myorg",
			PrimaryRepo: "main",
			Repos:       []string{"main"},
		},
	}
	s := New(cfg, testLogger())

	actionable := &github.ActionableResult{}
	template := "Homebrew: ${PROJECT_HOMEBREW_REPO}, Hive: ${HIVE_REPO}"
	result := s.substituteTemplate(template, actionable, "test", nil)

	if !strings.Contains(result, "Homebrew: myorg/homebrew-tap") {
		t.Errorf("expected homebrew repo: %s", result)
	}
	if !strings.Contains(result, "Hive: myorg/hive") {
		t.Errorf("expected hive repo: %s", result)
	}
}

func TestSubstituteTemplate_AuthAndRepos(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{
			Org:   "org",
			Repos: []string{"r1"},
		},
	}
	s := New(cfg, testLogger())

	actionable := &github.ActionableResult{}
	template := "${GH_AUTH}${AUTHORIZED_REPOS}"
	result := s.substituteTemplate(template, actionable, "test", nil)

	if !strings.Contains(result, "GH_TOKEN") {
		t.Errorf("expected GH auth instructions: %s", result)
	}
	if !strings.Contains(result, "AUTHORIZED REPOS") {
		t.Errorf("expected authorized repos: %s", result)
	}
}

func TestSubstituteTemplate_TimestampPresent(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{
			Org:   "org",
			Repos: []string{"r"},
		},
	}
	s := New(cfg, testLogger())

	actionable := &github.ActionableResult{}
	template := "Time: ${TIMESTAMP}"
	result := s.substituteTemplate(template, actionable, "test", nil)

	// Should contain the timestamp, not the placeholder
	if strings.Contains(result, "${TIMESTAMP}") {
		t.Error("expected TIMESTAMP to be substituted")
	}
	if !strings.Contains(result, "Time: ") {
		t.Error("expected Time prefix")
	}
}

// ---------------------------------------------------------------------------
// loadPromptTemplate — test with temp file
// ---------------------------------------------------------------------------

// redirectPolicySeams points every /data policy seam (agentHomeDir,
// userSavedPolicyDir, clonedPoliciesDir) at fresh empty temp dirs so the
// template loaders can never read live production state on a hive host
// (#4585). Restores the originals via t.Cleanup.
func redirectPolicySeams(t *testing.T) {
	t.Helper()
	prevAgentHome := agentHomeDir
	prevUserSaved := userSavedPolicyDir
	prevCloned := clonedPoliciesDir
	agentHomeDir = t.TempDir()
	userSavedPolicyDir = t.TempDir()
	clonedPoliciesDir = t.TempDir()
	t.Cleanup(func() {
		agentHomeDir = prevAgentHome
		userSavedPolicyDir = prevUserSaved
		clonedPoliciesDir = prevCloned
	})
}

func TestLoadPromptTemplate_FromPoliciesDir(t *testing.T) {
	redirectPolicySeams(t)
	tmpDir := t.TempDir()
	// Use an agent name that cannot exist in the live /data/agents or
	// /data/policies paths, which loadPromptTemplate consults first; a real
	// deployed name like "scanner" would be shadowed on a hive host.
	const agentName = "hermetic-test-agent-xyz"
	// Create the template file
	templatePath := tmpDir + "/examples/kubestellar/agents/" + agentName + ".md"
	os.MkdirAll(tmpDir+"/examples/kubestellar/agents", 0o755)
	os.WriteFile(templatePath, []byte("Hello ${AGENT_NAME}"), 0o644)

	cfg := &config.Config{
		Policies: config.PoliciesConfig{
			LocalDir: tmpDir,
		},
		Project: config.ProjectConfig{Org: "org", Repos: []string{"r"}},
	}
	s := New(cfg, testLogger())
	result := s.loadPromptTemplate(agentName)
	if result != "Hello ${AGENT_NAME}" {
		t.Errorf("got %q, want template content", result)
	}
}

func TestLoadPromptTemplate_NotFound(t *testing.T) {
	redirectPolicySeams(t)
	cfg := &config.Config{
		Policies: config.PoliciesConfig{
			LocalDir: "/nonexistent",
		},
	}
	s := New(cfg, testLogger())
	result := s.loadPromptTemplate("nonexistent-agent-xyz")
	if result != "" {
		t.Errorf("expected empty string for missing template, got %q", result)
	}
}

// ---------------------------------------------------------------------------
// buildAgentMessage with template
// ---------------------------------------------------------------------------

func TestBuildAgentMessage_UsesTemplate(t *testing.T) {
	redirectPolicySeams(t)
	tmpDir := t.TempDir()
	templatePath := tmpDir + "/examples/kubestellar/agents/custom-agent.md"
	os.MkdirAll(tmpDir+"/examples/kubestellar/agents", 0o755)
	os.WriteFile(templatePath, []byte("Custom: ${AGENT_NAME} issues=${QUEUE_ISSUES}"), 0o644)

	cfg := &config.Config{
		Policies: config.PoliciesConfig{LocalDir: tmpDir},
		Project:  config.ProjectConfig{Org: "org", Repos: []string{"r"}},
	}
	s := New(cfg, testLogger())

	actionable := &github.ActionableResult{
		Issues: github.IssueResult{Count: 7},
	}
	result := s.BuildAgentMessage("custom-agent", nil, actionable)
	if !strings.Contains(result, "[agent:custom-agent]") {
		t.Errorf("expected kick header: %s", result)
	}
	if !strings.Contains(result, "Custom: custom-agent issues=7") {
		t.Errorf("expected template substitution: %s", result)
	}
}

// ---------------------------------------------------------------------------
// buildQualityMessage — additional branch coverage
// ---------------------------------------------------------------------------

func TestBuildQualityMessage_NoIssues(t *testing.T) {
	s := newScheduler()
	actionable := &github.ActionableResult{}
	msg := s.buildQualityMessage(nil, actionable)

	if !strings.Contains(msg, "[agent:quality]") {
		t.Error("expected quality header")
	}
	if strings.Contains(msg, "TEST-RELATED ISSUES") {
		t.Error("should not have issues section when none exist")
	}
	if !strings.Contains(msg, "COVERAGE TARGET: 91%") {
		t.Error("expected coverage target")
	}
}

func TestBuildQualityMessage_WithIssues(t *testing.T) {
	s := newScheduler()
	issues := []github.Issue{
		{Repo: "r", Number: 1, Title: "test gap", Lane: "quality", Labels: []string{"test"}},
	}
	actionable := &github.ActionableResult{
		Issues: github.IssueResult{Count: 1, Items: issues},
	}
	msg := s.buildQualityMessage(issues, actionable)
	if !strings.Contains(msg, "TEST-RELATED ISSUES (1)") {
		t.Error("expected issues section")
	}
}

func TestBuildQualityMessage_TruncatesTitle(t *testing.T) {
	s := newScheduler()
	longTitle := strings.Repeat("z", 80)
	issues := []github.Issue{
		{Repo: "r", Number: 1, Title: longTitle, Lane: "quality", Labels: []string{}},
	}
	actionable := &github.ActionableResult{
		Issues: github.IssueResult{Count: 1, Items: issues},
	}
	msg := s.buildQualityMessage(issues, actionable)
	if strings.Contains(msg, longTitle) {
		t.Error("expected title to be truncated")
	}
}

// ---------------------------------------------------------------------------
// buildArchitectMessage — additional branch coverage
// ---------------------------------------------------------------------------

func TestBuildArchitectMessage_WithIssues(t *testing.T) {
	s := newScheduler()
	issues := []github.Issue{
		{Repo: "r", Number: 1, Title: "refactor", Lane: "architect", Labels: []string{"arch"}},
	}
	actionable := &github.ActionableResult{
		Issues: github.IssueResult{Count: 1, Items: issues},
		PRs:    github.PRResult{Count: 2},
		Hold:   github.HoldResult{Total: 0},
	}
	msg := s.buildArchitectMessage(issues, actionable)
	if !strings.Contains(msg, "ARCHITECTURE-RELATED ISSUES (1)") {
		t.Error("expected architecture issues section")
	}
	if !strings.Contains(msg, "r#1") {
		t.Error("expected issue reference")
	}
}

func TestBuildArchitectMessage_TruncatesTitle(t *testing.T) {
	s := newScheduler()
	longTitle := strings.Repeat("a", 80)
	issues := []github.Issue{
		{Repo: "r", Number: 1, Title: longTitle, Lane: "architect", Labels: []string{}},
	}
	actionable := &github.ActionableResult{
		Issues: github.IssueResult{Count: 1, Items: issues},
	}
	msg := s.buildArchitectMessage(issues, actionable)
	if strings.Contains(msg, longTitle) {
		t.Error("expected title truncation")
	}
}

// ---------------------------------------------------------------------------
// buildGenericMessage — branch for knowledge priming
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// buildAgentListAndRoles
// ---------------------------------------------------------------------------

func TestBuildAgentListAndRoles_WithAgents(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner":    {Backend: "claude", Model: "sonnet", DisplayName: "Scanner Agent", Enabled: true},
			"supervisor": {Backend: "claude", Model: "opus", Enabled: true},
		},
		Project: config.ProjectConfig{Org: "org", Repos: []string{"r"}},
	}
	s := New(cfg, testLogger())
	list, roles := s.buildAgentListAndRoles()

	// list should contain both agent names
	if !strings.Contains(list, "scanner") {
		t.Errorf("expected scanner in list: %s", list)
	}
	if !strings.Contains(list, "supervisor") {
		t.Errorf("expected supervisor in list: %s", list)
	}

	// roles should show display name and model
	if !strings.Contains(roles, "Scanner Agent") {
		t.Errorf("expected display name in roles: %s", roles)
	}
	if !strings.Contains(roles, "sonnet") {
		t.Errorf("expected model in roles: %s", roles)
	}
	// supervisor has no display name, should use the key
	if !strings.Contains(roles, "supervisor") {
		t.Errorf("expected supervisor key in roles: %s", roles)
	}
}

func TestBuildAgentListAndRoles_EmptyModel(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"worker": {Backend: "claude", Enabled: true},
		},
		Project: config.ProjectConfig{Org: "org", Repos: []string{"r"}},
	}
	s := New(cfg, testLogger())
	_, roles := s.buildAgentListAndRoles()
	if !strings.Contains(roles, "default") {
		t.Errorf("expected default model: %s", roles)
	}
}

func TestSubstituteTemplate_AgentListVars(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "claude", Model: "sonnet", Enabled: true},
		},
		Project: config.ProjectConfig{
			Org:   "org",
			Repos: []string{"r"},
		},
	}
	s := New(cfg, testLogger())

	actionable := &github.ActionableResult{}
	template := "Agents: ${AGENT_LIST}, Enabled: ${ENABLED_AGENTS}, Roles:\n${AGENT_ROLES}"
	result := s.substituteTemplate(template, actionable, "test", nil)

	if !strings.Contains(result, "scanner") {
		t.Errorf("expected scanner in agent list: %s", result)
	}
	// AGENT_LIST and ENABLED_AGENTS should be the same
	if strings.Contains(result, "${AGENT_LIST}") {
		t.Error("AGENT_LIST not substituted")
	}
	if strings.Contains(result, "${ENABLED_AGENTS}") {
		t.Error("ENABLED_AGENTS not substituted")
	}
}

func TestBuildGenericMessage_WithIssues(t *testing.T) {
	s := newScheduler()
	issues := []github.Issue{
		{Repo: "r", Number: 1, Title: "custom work", Lane: "custom", Labels: []string{}},
	}
	actionable := &github.ActionableResult{
		Issues: github.IssueResult{Count: 1, Items: issues},
	}
	msg := s.buildGenericMessage("custom", issues, actionable)
	if !strings.Contains(msg, "Work items (1)") {
		t.Error("expected work items section")
	}
	if !strings.Contains(msg, "r#1") {
		t.Error("expected issue reference")
	}
}
