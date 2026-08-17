package scheduler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/knowledge"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newScheduler() *Scheduler {
	cfg := &config.Config{
		Project: config.ProjectConfig{
			Org:   "test-org",
			Repos: []string{"test-org/console", "test-org/docs", "test-org/hive"},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(cfg, logger)
}

// makeIssue returns a github.Issue with the given fields. Remaining fields are
// zero-valued and safe to use in all scheduler paths.
func makeIssue(repo string, number int, title, lane string, ageMinutes int, labels []string, isTracker bool) github.Issue {
	return github.Issue{
		Repo:       repo,
		Number:     number,
		Title:      title,
		Lane:       lane,
		AgeMinutes: ageMinutes,
		Labels:     labels,
		IsTracker:  isTracker,
	}
}

// makePR returns a github.PullRequest with essential fields set.
func makePR(repo string, number int, title, author string) github.PullRequest {
	return github.PullRequest{
		Repo:   repo,
		Number: number,
		Title:  title,
		Author: author,
	}
}

// emptyActionable returns an ActionableResult with no issues, PRs, or holds.
func emptyActionable() *github.ActionableResult {
	return &github.ActionableResult{
		Issues: github.IssueResult{},
		PRs:    github.PRResult{},
		Hold:   github.HoldResult{},
	}
}

// actionableWithIssues returns an ActionableResult pre-populated with the
// provided issues. Count and Items are set; SLAViolations is left to the
// caller to set when needed.
func actionableWithIssues(issues []github.Issue) *github.ActionableResult {
	return &github.ActionableResult{
		Issues: github.IssueResult{
			Count: len(issues),
			Items: issues,
		},
		PRs:  github.PRResult{},
		Hold: github.HoldResult{},
	}
}

// ---------------------------------------------------------------------------
// BuildKickMessages — routing / agent combinations
// ---------------------------------------------------------------------------

func TestBuildKickMessages_EmptyAgentsDue(t *testing.T) {
	s := newScheduler()
	result := s.BuildKickMessages(emptyActionable(), nil)
	if len(result) != 0 {
		t.Errorf("expected 0 messages, got %d", len(result))
	}
}

func TestBuildKickMessages_SingleScannerAgent(t *testing.T) {
	s := newScheduler()
	actionable := emptyActionable()
	messages := s.BuildKickMessages(actionable, []string{"scanner"})
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0].Agent != "scanner" {
		t.Errorf("Agent = %q, want %q", messages[0].Agent, "scanner")
	}
	if !strings.Contains(messages[0].Message, "[agent:scanner]") {
		t.Errorf("scanner message missing [agent:scanner] header")
	}
}

func TestBuildKickMessages_MultipleAgents(t *testing.T) {
	s := newScheduler()
	actionable := emptyActionable()
	agents := []string{"scanner", "ci-maintainer", "supervisor"}
	messages := s.BuildKickMessages(actionable, agents)
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}
	agentSet := map[string]bool{}
	for _, m := range messages {
		agentSet[m.Agent] = true
	}
	for _, a := range agents {
		if !agentSet[a] {
			t.Errorf("missing message for agent %q", a)
		}
	}
}

func TestBuildKickMessages_GenericAgent(t *testing.T) {
	s := newScheduler()
	actionable := emptyActionable()
	messages := s.BuildKickMessages(actionable, []string{"outreach"})
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0].Agent != "outreach" {
		t.Errorf("Agent = %q, want %q", messages[0].Agent, "outreach")
	}
}

func TestBuildKickMessages_AgentOrderPreserved(t *testing.T) {
	s := newScheduler()
	actionable := emptyActionable()
	agents := []string{"supervisor", "scanner", "ci-maintainer"}
	messages := s.BuildKickMessages(actionable, agents)
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}
	for i, a := range agents {
		if messages[i].Agent != a {
			t.Errorf("messages[%d].Agent = %q, want %q", i, messages[i].Agent, a)
		}
	}
}

func TestBuildKickMessages_ClassificationApplied(t *testing.T) {
	// Issues with "typo" in title → Simple/haiku classification.
	// After ClassifyAll the lane will be "scanner" (typo hits simpleKeywords,
	// not a lane keyword), so scanner should see them.
	s := newScheduler()
	issues := []github.Issue{
		makeIssue("org/repo", 1, "Fix typo in README", "", 5, nil, false),
	}
	actionable := actionableWithIssues(issues)
	messages := s.BuildKickMessages(actionable, []string{"scanner"})
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if !strings.Contains(messages[0].Message, "[agent:scanner]") {
		t.Errorf("expected scanner kick header in message, got:\n%s", messages[0].Message)
	}
}

// ---------------------------------------------------------------------------
// buildScannerMessage — format details
// ---------------------------------------------------------------------------

func TestScannerMessage_Header(t *testing.T) {
	s := newScheduler()
	msg := s.buildScannerMessage(nil, emptyActionable())
	if !strings.Contains(msg, "[agent:scanner]") {
		t.Errorf("missing [agent:scanner] header")
	}
	if !strings.Contains(msg, "YOUR WORK LIST") {
		t.Errorf("missing YOUR WORK LIST line")
	}
}

func TestScannerMessage_Footer(t *testing.T) {
	s := newScheduler()
	msg := s.buildScannerMessage(nil, emptyActionable())
	if !strings.Contains(msg, "WORKFLOW") {
		t.Errorf("missing WORKFLOW footer")
	}
}

func TestScannerMessage_IssueCount(t *testing.T) {
	s := newScheduler()
	issues := []github.Issue{
		makeIssue("org/a", 1, "Issue one", "scanner", 10, nil, false),
		makeIssue("org/a", 2, "Issue two", "scanner", 20, nil, false),
	}
	msg := s.buildScannerMessage(issues, emptyActionable())
	if !strings.Contains(msg, "ACTIONABLE ISSUES (2, oldest first)") {
		t.Errorf("expected issue count 2, message:\n%s", msg)
	}
}

func TestScannerMessage_IssueFormatLine(t *testing.T) {
	s := newScheduler()
	issues := []github.Issue{
		{
			Repo:           "org/repo",
			Number:         42,
			Title:          "Short title",
			Lane:           "scanner",
			AgeMinutes:     15,
			Labels:         []string{"kind/bug"},
			ComplexityTier: "Medium",
			ModelRec:       "sonnet",
			IsTracker:      false,
		},
	}
	msg := s.buildScannerMessage(issues, emptyActionable())
	// Expect:   15m org/repo#42 [M/sonnet] [kind/bug] Short title
	if !strings.Contains(msg, "15m org/repo#42") {
		t.Errorf("missing age and repo#number in message:\n%s", msg)
	}
	if !strings.Contains(msg, "[M/sonnet]") {
		t.Errorf("missing tier/model marker in message:\n%s", msg)
	}
	if !strings.Contains(msg, "[kind/bug]") {
		t.Errorf("missing label in message:\n%s", msg)
	}
	if !strings.Contains(msg, "Short title") {
		t.Errorf("missing issue title in message:\n%s", msg)
	}
}

func TestScannerMessage_TrackerMarker(t *testing.T) {
	s := newScheduler()
	issues := []github.Issue{
		{
			Repo:           "org/repo",
			Number:         99,
			Title:          "Tracker issue",
			Lane:           "scanner",
			AgeMinutes:     5,
			ComplexityTier: "Simple",
			ModelRec:       "haiku",
			IsTracker:      true,
		},
	}
	msg := s.buildScannerMessage(issues, emptyActionable())
	if !strings.Contains(msg, "[TRACKER]") {
		t.Errorf("expected [TRACKER] marker in message:\n%s", msg)
	}
}

func TestScannerMessage_NoTrackerMarkerWhenFalse(t *testing.T) {
	s := newScheduler()
	issues := []github.Issue{
		{
			Repo:       "org/repo",
			Number:     1,
			Title:      "Normal issue",
			Lane:       "scanner",
			AgeMinutes: 5,
			IsTracker:  false,
		},
	}
	msg := s.buildScannerMessage(issues, emptyActionable())
	if strings.Contains(msg, "[TRACKER]") {
		t.Errorf("unexpected [TRACKER] marker in message:\n%s", msg)
	}
}

func TestScannerMessage_TitleTruncatedAt60Chars(t *testing.T) {
	s := newScheduler()
	longTitle := strings.Repeat("x", 80)
	issues := []github.Issue{
		{
			Repo:       "org/repo",
			Number:     1,
			Title:      longTitle,
			Lane:       "scanner",
			AgeMinutes: 1,
		},
	}
	msg := s.buildScannerMessage(issues, emptyActionable())
	// The full 80-char title must not appear; the truncated 60-char prefix must.
	if strings.Contains(msg, longTitle) {
		t.Errorf("title was not truncated (80 chars still present)")
	}
	if !strings.Contains(msg, strings.Repeat("x", 60)) {
		t.Errorf("expected 60-char truncated title in message")
	}
}

func TestScannerMessage_TitleExactly60CharsNotTruncated(t *testing.T) {
	s := newScheduler()
	title60 := strings.Repeat("a", 60)
	issues := []github.Issue{
		{
			Repo:       "org/repo",
			Number:     1,
			Title:      title60,
			Lane:       "scanner",
			AgeMinutes: 1,
		},
	}
	msg := s.buildScannerMessage(issues, emptyActionable())
	if !strings.Contains(msg, title60) {
		t.Errorf("60-char title should appear unchanged")
	}
}

func TestScannerMessage_MaxIssuesPerKickTruncatesAtLimit(t *testing.T) {
	s := newScheduler()
	issueCount := maxIssuesPerKick + 10
	var issues []github.Issue
	for i := 1; i <= issueCount; i++ {
		issues = append(issues, makeIssue("org/repo", i, "issue title", "scanner", i, nil, false))
	}
	msg := s.buildScannerMessage(issues, emptyActionable())

	// Count how many "org/repo#" occurrences appear (each issue line has one).
	count := strings.Count(msg, "org/repo#")
	if count != maxIssuesPerKick {
		t.Errorf("expected %d issue lines (truncated at maxIssuesPerKick), got %d", maxIssuesPerKick, count)
	}
}

func TestScannerMessage_PRListing(t *testing.T) {
	s := newScheduler()
	prs := []github.PullRequest{
		makePR("org/repo", 101, "Fix the thing", "alice"),
		makePR("org/repo", 102, "Another fix", "bob"),
	}
	actionable := &github.ActionableResult{
		PRs: github.PRResult{Count: 2, Items: prs},
	}
	msg := s.buildScannerMessage(nil, actionable)
	if !strings.Contains(msg, "ACTIONABLE PRs (2)") {
		t.Errorf("expected PR count 2, message:\n%s", msg)
	}
	if !strings.Contains(msg, "org/repo#101") {
		t.Errorf("missing PR #101 in message:\n%s", msg)
	}
	if !strings.Contains(msg, "@alice") {
		t.Errorf("missing PR author @alice in message:\n%s", msg)
	}
	if !strings.Contains(msg, "org/repo#102") {
		t.Errorf("missing PR #102 in message:\n%s", msg)
	}
}

func TestScannerMessage_PRTitleTruncatedAt70Chars(t *testing.T) {
	s := newScheduler()
	longPRTitle := strings.Repeat("p", 90)
	prs := []github.PullRequest{
		makePR("org/repo", 200, longPRTitle, "alice"),
	}
	actionable := &github.ActionableResult{
		PRs: github.PRResult{Count: 1, Items: prs},
	}
	msg := s.buildScannerMessage(nil, actionable)
	if strings.Contains(msg, longPRTitle) {
		t.Errorf("PR title was not truncated (90 chars still present)")
	}
	if !strings.Contains(msg, strings.Repeat("p", 70)) {
		t.Errorf("expected 70-char truncated PR title in message")
	}
}

func TestScannerMessage_SLAViolationShown(t *testing.T) {
	s := newScheduler()
	actionable := &github.ActionableResult{
		Issues: github.IssueResult{SLAViolations: 3},
		PRs:    github.PRResult{},
	}
	msg := s.buildScannerMessage(nil, actionable)
	if !strings.Contains(msg, "3 SLA VIOLATIONS") {
		t.Errorf("expected SLA violations warning, message:\n%s", msg)
	}
}

func TestScannerMessage_NoSLAViolationWhenZero(t *testing.T) {
	s := newScheduler()
	actionable := &github.ActionableResult{
		Issues: github.IssueResult{SLAViolations: 0},
	}
	msg := s.buildScannerMessage(nil, actionable)
	if strings.Contains(msg, "SLA VIOLATIONS") {
		t.Errorf("unexpected SLA violations warning when count is 0")
	}
}

func TestScannerMessage_ShowsAllLanes(t *testing.T) {
	s := newScheduler()
	issues := []github.Issue{
		makeIssue("org/repo", 1, "scanner issue", "scanner", 5, nil, false),
		makeIssue("org/repo", 2, "ci-maintainer issue", "ci-maintainer", 5, nil, false),
		makeIssue("org/repo", 3, "outreach issue", "outreach", 5, nil, false),
	}
	msg := s.buildScannerMessage(issues, emptyActionable())
	// Scanner sees ALL issues (not lane-filtered) — it's the triage agent.
	if !strings.Contains(msg, "org/repo#1") {
		t.Errorf("expected scanner-lane issue #1 in scanner message")
	}
	if !strings.Contains(msg, "org/repo#2") {
		t.Errorf("expected ci-maintainer-lane issue #2 in scanner message (scanner sees all)")
	}
	if !strings.Contains(msg, "org/repo#3") {
		t.Errorf("expected outreach-lane issue #3 in scanner message (scanner sees all)")
	}
}

func TestScannerMessage_EmptyLaneIncluded(t *testing.T) {
	s := newScheduler()
	issues := []github.Issue{
		makeIssue("org/repo", 10, "unclassified issue", "", 5, nil, false),
	}
	msg := s.buildScannerMessage(issues, emptyActionable())
	if !strings.Contains(msg, "org/repo#10") {
		t.Errorf("expected empty-lane issue #10 to appear in scanner message")
	}
}

func TestScannerMessage_MultipleLabels(t *testing.T) {
	s := newScheduler()
	issues := []github.Issue{
		{
			Repo:           "org/repo",
			Number:         7,
			Title:          "Multi-label issue",
			Lane:           "scanner",
			AgeMinutes:     2,
			Labels:         []string{"kind/bug", "priority/high"},
			ComplexityTier: "Medium",
			ModelRec:       "sonnet",
		},
	}
	msg := s.buildScannerMessage(issues, emptyActionable())
	if !strings.Contains(msg, "kind/bug,priority/high") {
		t.Errorf("expected comma-joined labels, message:\n%s", msg)
	}
}

func TestScannerMessage_EmptyLabels(t *testing.T) {
	s := newScheduler()
	issues := []github.Issue{
		{
			Repo:           "org/repo",
			Number:         8,
			Title:          "No-label issue",
			Lane:           "scanner",
			AgeMinutes:     3,
			Labels:         nil,
			ComplexityTier: "Medium",
			ModelRec:       "sonnet",
		},
	}
	// Should not panic on nil labels.
	msg := s.buildScannerMessage(issues, emptyActionable())
	if !strings.Contains(msg, "org/repo#8") {
		t.Errorf("expected issue #8 in message")
	}
}

func TestScannerMessage_TierFirstCharUsed(t *testing.T) {
	s := newScheduler()
	cases := []struct {
		tier      string
		wantChar  string
	}{
		{"Simple", "S"},
		{"Medium", "M"},
		{"Complex", "C"},
	}
	for _, tc := range cases {
		t.Run(tc.tier, func(t *testing.T) {
			issues := []github.Issue{
				{
					Repo:           "org/repo",
					Number:         1,
					Title:          "test",
					Lane:           "scanner",
					AgeMinutes:     1,
					ComplexityTier: tc.tier,
					ModelRec:       "sonnet",
				},
			}
			msg := s.buildScannerMessage(issues, emptyActionable())
			marker := "[" + tc.wantChar + "/sonnet]"
			if !strings.Contains(msg, marker) {
				t.Errorf("expected %q marker, message:\n%s", marker, msg)
			}
		})
	}
}

func TestScannerMessage_EmptyTierHandled(t *testing.T) {
	s := newScheduler()
	issues := []github.Issue{
		{
			Repo:       "org/repo",
			Number:     5,
			Title:      "No tier issue",
			Lane:       "scanner",
			AgeMinutes: 1,
			// ComplexityTier intentionally empty
		},
	}
	// Should not panic when tier is empty string.
	msg := s.buildScannerMessage(issues, emptyActionable())
	if !strings.Contains(msg, "org/repo#5") {
		t.Errorf("expected issue #5 in message")
	}
}

// ---------------------------------------------------------------------------
// buildCIMaintainerMessage
// ---------------------------------------------------------------------------

func TestReviewerMessage_Header(t *testing.T) {
	s := newScheduler()
	msg := s.buildCIMaintainerMessage(emptyActionable())
	if !strings.Contains(msg, "[agent:ci-maintainer]") {
		t.Errorf("missing ci-maintainer header")
	}
}

func TestReviewerMessage_ContainsQueueCounts(t *testing.T) {
	s := newScheduler()
	actionable := &github.ActionableResult{
		Issues: github.IssueResult{Count: 5},
		PRs:    github.PRResult{Count: 3},
		Hold:   github.HoldResult{Total: 2},
	}
	msg := s.buildCIMaintainerMessage(actionable)
	if !strings.Contains(msg, "5 issues") {
		t.Errorf("missing issue count, message:\n%s", msg)
	}
	if !strings.Contains(msg, "3 PRs") {
		t.Errorf("missing PR count, message:\n%s", msg)
	}
	if !strings.Contains(msg, "2 on hold") {
		t.Errorf("missing hold count, message:\n%s", msg)
	}
}

func TestReviewerMessage_ZeroCounts(t *testing.T) {
	s := newScheduler()
	msg := s.buildCIMaintainerMessage(emptyActionable())
	if !strings.Contains(msg, "0 issues") {
		t.Errorf("expected 0 issues in ci-maintainer message:\n%s", msg)
	}
	if !strings.Contains(msg, "0 PRs") {
		t.Errorf("expected 0 PRs in ci-maintainer message:\n%s", msg)
	}
}

func TestReviewerMessage_HealthCheckLine(t *testing.T) {
	s := newScheduler()
	msg := s.buildCIMaintainerMessage(emptyActionable())
	if !strings.Contains(msg, "Post-merge health check") {
		t.Errorf("missing health check instruction line")
	}
}

// ---------------------------------------------------------------------------
// buildSupervisorMessage
// ---------------------------------------------------------------------------

func TestSupervisorMessage_Header(t *testing.T) {
	s := newScheduler()
	msg := s.buildSupervisorMessage(emptyActionable())
	if !strings.Contains(msg, "[agent:supervisor]") {
		t.Errorf("missing supervisor header")
	}
}

func TestSupervisorMessage_ContainsQueueCounts(t *testing.T) {
	s := newScheduler()
	actionable := &github.ActionableResult{
		Issues: github.IssueResult{Count: 7, SLAViolations: 2},
		PRs:    github.PRResult{Count: 4},
		Hold:   github.HoldResult{Total: 1},
	}
	msg := s.buildSupervisorMessage(actionable)
	if !strings.Contains(msg, "7 issues") {
		t.Errorf("missing issue count, message:\n%s", msg)
	}
	if !strings.Contains(msg, "4 PRs") {
		t.Errorf("missing PR count, message:\n%s", msg)
	}
	if !strings.Contains(msg, "1 on hold") {
		t.Errorf("missing hold count, message:\n%s", msg)
	}
	if !strings.Contains(msg, "2 SLA violations") {
		t.Errorf("missing SLA violations count, message:\n%s", msg)
	}
}

func TestSupervisorMessage_SLAViolationsZero(t *testing.T) {
	s := newScheduler()
	msg := s.buildSupervisorMessage(emptyActionable())
	if !strings.Contains(msg, "0 SLA violations") {
		t.Errorf("expected 0 SLA violations in supervisor message:\n%s", msg)
	}
}

func TestSupervisorMessage_SweepInstructions(t *testing.T) {
	s := newScheduler()
	msg := s.buildSupervisorMessage(emptyActionable())
	if !strings.Contains(msg, "MONITORING PASS") {
		t.Errorf("missing monitoring pass header")
	}
}

// ---------------------------------------------------------------------------
// buildGenericMessage
// ---------------------------------------------------------------------------

func TestGenericMessage_Header(t *testing.T) {
	s := newScheduler()
	msg := s.buildGenericMessage("outreach", nil, emptyActionable())
	if !strings.Contains(msg, "[agent:outreach]") {
		t.Errorf("missing generic agent header, message:\n%s", msg)
	}
}

func TestGenericMessage_FiltersIssuesByLane(t *testing.T) {
	s := newScheduler()
	issues := []github.Issue{
		makeIssue("org/repo", 1, "outreach issue", "outreach", 5, nil, false),
		makeIssue("org/repo", 2, "scanner issue", "scanner", 5, nil, false),
	}
	msg := s.buildGenericMessage("outreach", issues, emptyActionable())
	if !strings.Contains(msg, "org/repo#1") {
		t.Errorf("expected outreach-lane issue #1 in generic message")
	}
	if strings.Contains(msg, "org/repo#2") {
		t.Errorf("unexpected scanner-lane issue #2 in generic message for outreach agent")
	}
}

func TestGenericMessage_EmptyLaneIncluded(t *testing.T) {
	s := newScheduler()
	issues := []github.Issue{
		makeIssue("org/repo", 3, "unclassified", "", 5, nil, false),
	}
	msg := s.buildGenericMessage("architect", issues, emptyActionable())
	if !strings.Contains(msg, "org/repo#3") {
		t.Errorf("expected empty-lane issue #3 in generic message")
	}
}

func TestGenericMessage_NoMatchingIssues(t *testing.T) {
	s := newScheduler()
	issues := []github.Issue{
		makeIssue("org/repo", 1, "scanner issue", "scanner", 5, nil, false),
	}
	// architect agent: only empty-lane or "architect"-lane issues match.
	msg := s.buildGenericMessage("architect", issues, emptyActionable())
	// Should not contain work items section since no matches.
	if strings.Contains(msg, "Work items") {
		t.Errorf("unexpected Work items section when no matching issues:\n%s", msg)
	}
}

func TestGenericMessage_WorkItemsSection(t *testing.T) {
	s := newScheduler()
	issues := []github.Issue{
		makeIssue("org/repo", 5, "outreach campaign", "outreach", 10, nil, false),
	}
	msg := s.buildGenericMessage("outreach", issues, emptyActionable())
	if !strings.Contains(msg, "Work items (1)") {
		t.Errorf("expected 'Work items (1)' section:\n%s", msg)
	}
	if !strings.Contains(msg, "org/repo#5") {
		t.Errorf("expected issue #5 in work items:\n%s", msg)
	}
	if !strings.Contains(msg, "outreach campaign") {
		t.Errorf("expected issue title in work items:\n%s", msg)
	}
}

func TestGenericMessage_CustomAgentName(t *testing.T) {
	s := newScheduler()
	msg := s.buildGenericMessage("my-custom-agent", nil, emptyActionable())
	if !strings.Contains(msg, "[agent:my-custom-agent]") {
		t.Errorf("expected custom agent name in header:\n%s", msg)
	}
}

// ---------------------------------------------------------------------------
// filterByLane
// ---------------------------------------------------------------------------

func TestFilterByLane_MatchingLane(t *testing.T) {
	issues := []github.Issue{
		makeIssue("org/r", 1, "a", "scanner", 0, nil, false),
		makeIssue("org/r", 2, "b", "ci-maintainer", 0, nil, false),
	}
	result := filterByLane(issues, "scanner")
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	if result[0].Number != 1 {
		t.Errorf("expected issue #1, got #%d", result[0].Number)
	}
}

func TestFilterByLane_EmptyLaneAlwaysIncluded(t *testing.T) {
	issues := []github.Issue{
		makeIssue("org/r", 1, "a", "", 0, nil, false),
		makeIssue("org/r", 2, "b", "scanner", 0, nil, false),
	}
	result := filterByLane(issues, "ci-maintainer")
	// Issue #1 has empty lane → included. Issue #2 has lane "scanner" → excluded.
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	if result[0].Number != 1 {
		t.Errorf("expected issue #1 (empty lane), got #%d", result[0].Number)
	}
}

func TestFilterByLane_BothMatchingAndEmpty(t *testing.T) {
	issues := []github.Issue{
		makeIssue("org/r", 1, "a", "scanner", 0, nil, false),
		makeIssue("org/r", 2, "b", "", 0, nil, false),
		makeIssue("org/r", 3, "c", "ci-maintainer", 0, nil, false),
	}
	result := filterByLane(issues, "scanner")
	if len(result) != 2 {
		t.Fatalf("expected 2 results (scanner + empty), got %d", len(result))
	}
	nums := map[int]bool{}
	for _, r := range result {
		nums[r.Number] = true
	}
	if !nums[1] {
		t.Errorf("expected issue #1 (scanner lane)")
	}
	if !nums[2] {
		t.Errorf("expected issue #2 (empty lane)")
	}
	if nums[3] {
		t.Errorf("unexpected issue #3 (ci-maintainer lane)")
	}
}

func TestFilterByLane_EmptyInput(t *testing.T) {
	result := filterByLane(nil, "scanner")
	if result != nil {
		t.Errorf("expected nil result for nil input, got %v", result)
	}
}

func TestFilterByLane_NoMatches(t *testing.T) {
	issues := []github.Issue{
		makeIssue("org/r", 1, "a", "ci-maintainer", 0, nil, false),
	}
	result := filterByLane(issues, "scanner")
	if len(result) != 0 {
		t.Errorf("expected 0 results, got %d", len(result))
	}
}

func TestFilterByLane_AllEmpty(t *testing.T) {
	issues := []github.Issue{
		makeIssue("org/r", 1, "a", "", 0, nil, false),
		makeIssue("org/r", 2, "b", "", 0, nil, false),
	}
	result := filterByLane(issues, "scanner")
	if len(result) != 2 {
		t.Errorf("expected 2 results (both empty lane), got %d", len(result))
	}
}

func TestFilterByLane_AllMatchingLane(t *testing.T) {
	issues := []github.Issue{
		makeIssue("org/r", 1, "a", "outreach", 0, nil, false),
		makeIssue("org/r", 2, "b", "outreach", 0, nil, false),
	}
	result := filterByLane(issues, "outreach")
	if len(result) != 2 {
		t.Errorf("expected 2 results, got %d", len(result))
	}
}

// ---------------------------------------------------------------------------
// Empty inputs (no issues, no PRs, no agents due)
// ---------------------------------------------------------------------------

func TestBuildKickMessages_NoIssuesNoPRs(t *testing.T) {
	s := newScheduler()
	actionable := emptyActionable()
	messages := s.BuildKickMessages(actionable, []string{"scanner", "ci-maintainer", "supervisor"})
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages even with no issues/PRs, got %d", len(messages))
	}
}

func TestScannerMessage_NoPRs(t *testing.T) {
	s := newScheduler()
	msg := s.buildScannerMessage(nil, emptyActionable())
	if !strings.Contains(msg, "ACTIONABLE PRs (0)") {
		t.Errorf("expected ACTIONABLE PRs (0) line, message:\n%s", msg)
	}
}

func TestScannerMessage_NoIssues(t *testing.T) {
	s := newScheduler()
	msg := s.buildScannerMessage(nil, emptyActionable())
	if !strings.Contains(msg, "ACTIONABLE ISSUES (0, oldest first)") {
		t.Errorf("expected ACTIONABLE ISSUES (0, ...) line, message:\n%s", msg)
	}
}

func TestBuildKickMessages_AllAgentsDueButNoWork(t *testing.T) {
	s := newScheduler()
	agents := []string{"scanner", "ci-maintainer", "supervisor", "outreach", "architect"}
	messages := s.BuildKickMessages(emptyActionable(), agents)
	if len(messages) != len(agents) {
		t.Errorf("expected %d messages, got %d", len(agents), len(messages))
	}
}

// ---------------------------------------------------------------------------
// New() constructor
// ---------------------------------------------------------------------------

func TestNew_ReturnsScheduler(t *testing.T) {
	cfg := &config.Config{}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	s := New(cfg, logger)
	if s == nil {
		t.Fatal("New() returned nil")
	}
	if s.cfg != cfg {
		t.Error("New() did not assign cfg")
	}
	if s.logger != logger {
		t.Error("New() did not assign logger")
	}
}

// ---------------------------------------------------------------------------
// BuildKickMessages — KickMessage struct fields
// ---------------------------------------------------------------------------

func TestKickMessage_AgentAndMessageSet(t *testing.T) {
	s := newScheduler()
	messages := s.BuildKickMessages(emptyActionable(), []string{"ci-maintainer"})
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	m := messages[0]
	if m.Agent == "" {
		t.Error("KickMessage.Agent is empty")
	}
	if m.Message == "" {
		t.Error("KickMessage.Message is empty")
	}
}

// ---------------------------------------------------------------------------
// AUTHORIZED REPOS section
// ---------------------------------------------------------------------------

func TestBuildKickMessages_ScannerHasAuthorizedRepos(t *testing.T) {
	s := newScheduler()
	messages := s.BuildKickMessages(emptyActionable(), []string{"scanner"})
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	msg := messages[0].Message
	if !strings.Contains(msg, "AUTHORIZED REPOS") {
		t.Error("scanner kick missing AUTHORIZED REPOS section")
	}
	if !strings.Contains(msg, "test-org/console") {
		t.Error("scanner kick missing test-org/console in repos list")
	}
	if !strings.Contains(msg, "test-org/docs") {
		t.Error("scanner kick missing test-org/docs in repos list")
	}
	if !strings.Contains(msg, "NEVER access") {
		t.Error("scanner kick missing repo restriction warning")
	}
}

func TestBuildKickMessages_ReviewerHasAuthorizedRepos(t *testing.T) {
	s := newScheduler()
	messages := s.BuildKickMessages(emptyActionable(), []string{"ci-maintainer"})
	msg := messages[0].Message
	if !strings.Contains(msg, "AUTHORIZED REPOS") {
		t.Error("ci-maintainer kick missing AUTHORIZED REPOS section")
	}
}

func TestBuildKickMessages_SupervisorHasAuthorizedRepos(t *testing.T) {
	s := newScheduler()
	messages := s.BuildKickMessages(emptyActionable(), []string{"supervisor"})
	msg := messages[0].Message
	if !strings.Contains(msg, "AUTHORIZED REPOS") {
		t.Error("supervisor kick missing AUTHORIZED REPOS section")
	}
}

func TestBuildKickMessages_OutreachNoAuthorizedRepos(t *testing.T) {
	s := newScheduler()
	messages := s.BuildKickMessages(emptyActionable(), []string{"outreach"})
	msg := messages[0].Message
	if strings.Contains(msg, "AUTHORIZED REPOS") {
		t.Error("outreach kick should NOT have AUTHORIZED REPOS (targets external repos)")
	}
}

func TestBuildReposSection_BareRepoGetsPrefixed(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{
			Org:   "my-org",
			Repos: []string{"bare-repo", "my-org/full-repo"},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := New(cfg, logger)

	section := s.buildReposSection()
	if !strings.Contains(section, "my-org/bare-repo") {
		t.Errorf("bare repo should be prefixed with org: %q", section)
	}
	if !strings.Contains(section, "my-org/full-repo") {
		t.Errorf("full repo should appear as-is: %q", section)
	}
}

// ---------------------------------------------------------------------------
// Knowledge primer wiring
// ---------------------------------------------------------------------------

func TestExtractKeywords_FromLabels(t *testing.T) {
	issues := []github.Issue{
		makeIssue("org/repo", 1, "Test", "", 5, []string{"typescript", "react", "kind/bug"}, false),
	}
	kw := extractKeywords(issues)
	found := map[string]bool{}
	for _, k := range kw {
		found[k] = true
	}
	if !found["typescript"] {
		t.Error("expected typescript keyword")
	}
	if !found["react"] {
		t.Error("expected react keyword")
	}
	if found["kind/bug"] {
		t.Error("kind/bug should be filtered as noise label")
	}
}

func TestExtractKeywords_Dedup(t *testing.T) {
	issues := []github.Issue{
		makeIssue("org/repo", 1, "A", "", 5, []string{"react"}, false),
		makeIssue("org/repo", 2, "B", "", 5, []string{"react"}, false),
	}
	kw := extractKeywords(issues)
	count := 0
	for _, k := range kw {
		if k == "react" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 'react', got %d", count)
	}
}

func TestExtractKeywords_IncludesTier(t *testing.T) {
	issues := []github.Issue{
		{Repo: "org/repo", Number: 1, ComplexityTier: "Complex"},
	}
	kw := extractKeywords(issues)
	found := false
	for _, k := range kw {
		if k == "complex" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'complex' from ComplexityTier, got %v", kw)
	}
}

func TestExtractKeywords_EmptyIssues(t *testing.T) {
	kw := extractKeywords(nil)
	if len(kw) != 0 {
		t.Errorf("expected 0 keywords, got %d", len(kw))
	}
}

func TestIsNoiseLabel(t *testing.T) {
	if !isNoiseLabel("kind/bug") {
		t.Error("kind/bug should be noise")
	}
	if !isNoiseLabel("triage/accepted") {
		t.Error("triage/accepted should be noise")
	}
	if isNoiseLabel("typescript") {
		t.Error("typescript should not be noise")
	}
}

func TestPrimeKnowledge_NilPrimer(t *testing.T) {
	s := newScheduler()
	issues := []github.Issue{
		makeIssue("org/repo", 1, "test", "", 5, []string{"react"}, false),
	}
	result := s.primeKnowledge(issues)
	if result != "" {
		t.Errorf("expected empty result with nil primer, got: %s", result)
	}
}

func TestPrimeKnowledge_EmptyIssues(t *testing.T) {
	s := newScheduler()
	result := s.primeKnowledge(nil)
	if result != "" {
		t.Errorf("expected empty result with nil issues, got: %s", result)
	}
}

func TestPrimeKnowledge_WithMockWiki(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := struct {
			Total   int `json:"total"`
			Results []struct {
				Slug       string   `json:"slug"`
				Title      string   `json:"title"`
				Score      float64  `json:"score"`
				Type       string   `json:"type"`
				Status     string   `json:"status"`
				Confidence float64  `json:"confidence"`
				Tags       []string `json:"tags"`
				Snippet    string   `json:"snippet"`
			} `json:"results"`
		}{
			Total: 1,
			Results: []struct {
				Slug       string   `json:"slug"`
				Title      string   `json:"title"`
				Score      float64  `json:"score"`
				Type       string   `json:"type"`
				Status     string   `json:"status"`
				Confidence float64  `json:"confidence"`
				Tags       []string `json:"tags"`
				Snippet    string   `json:"snippet"`
			}{
				{
					Slug:       "guard-join",
					Title:      "Guard .join() against undefined",
					Score:      0.95,
					Type:       "gotcha",
					Status:     "verified",
					Confidence: 0.95,
					Tags:       []string{"typescript"},
					Snippet:    "Always use (arr || []).join()",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	layers := []knowledge.LayerConfig{
		{Type: knowledge.LayerProject, URL: srv.URL, Shared: true},
	}
	primerCfg := knowledge.PrimerConfig{
		MaxFacts: 25,
		Priority: []string{"regression", "gotcha", "pattern"},
	}
	primer := knowledge.NewPrimer(layers, primerCfg, logger)

	s := newScheduler()
	s.SetPrimer(primer)

	issues := []github.Issue{
		makeIssue("org/repo", 1, "Fix hook crash", "", 5, []string{"typescript", "hooks"}, false),
	}

	result := s.primeKnowledge(issues)
	if result == "" {
		t.Fatal("expected non-empty knowledge section")
	}
	if !strings.Contains(result, "Guard .join()") {
		t.Errorf("expected knowledge section to contain fact title, got:\n%s", result)
	}
	if !strings.Contains(result, "Relevant Knowledge") {
		t.Errorf("expected knowledge section header, got:\n%s", result)
	}
}

func TestScannerMessage_IncludesKnowledgeWhenPrimerSet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := struct {
			Total   int `json:"total"`
			Results []struct {
				Slug       string   `json:"slug"`
				Title      string   `json:"title"`
				Score      float64  `json:"score"`
				Type       string   `json:"type"`
				Confidence float64  `json:"confidence"`
				Snippet    string   `json:"snippet"`
			} `json:"results"`
		}{
			Total: 1,
			Results: []struct {
				Slug       string   `json:"slug"`
				Title      string   `json:"title"`
				Score      float64  `json:"score"`
				Type       string   `json:"type"`
				Confidence float64  `json:"confidence"`
				Snippet    string   `json:"snippet"`
			}{
				{Slug: "test-fact", Title: "Test fact", Type: "pattern", Confidence: 0.9, Snippet: "Use test factories"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	layers := []knowledge.LayerConfig{
		{Type: knowledge.LayerProject, URL: srv.URL, Shared: true},
	}
	primer := knowledge.NewPrimer(layers, knowledge.PrimerConfig{MaxFacts: 25}, logger)

	s := newScheduler()
	s.SetPrimer(primer)

	issues := []github.Issue{
		makeIssue("org/repo", 1, "Fix tests", "scanner", 5, []string{"testing"}, false),
	}
	msg := s.buildScannerMessage(issues, emptyActionable())

	if !strings.Contains(msg, "Relevant Knowledge") {
		t.Errorf("expected Relevant Knowledge section in scanner message:\n%s", msg)
	}
	if !strings.Contains(msg, "Test fact") {
		t.Errorf("expected fact title in scanner message:\n%s", msg)
	}
}
