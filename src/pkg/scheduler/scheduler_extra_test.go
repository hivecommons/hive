package scheduler

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func extraActionable() *github.ActionableResult {
	return &github.ActionableResult{
		Issues: github.IssueResult{
			Count:         3,
			SLAViolations: 1,
			Items: []github.Issue{
				{Repo: "repo1", Number: 1, Title: "bug fix", Labels: []string{"kind/bug"}, Lane: "scanner", AgeMinutes: 120, ComplexityTier: "simple", ModelRec: "haiku"},
				{Repo: "repo1", Number: 2, Title: "refactor needed", Labels: []string{"kind/task"}, Lane: "architect", AgeMinutes: 60},
				{Repo: "repo1", Number: 3, Title: "test coverage gap", Labels: []string{"kind/task"}, Lane: "quality", AgeMinutes: 45},
			},
		},
		PRs: github.PRResult{
			Count: 1,
			Items: []github.PullRequest{
				{Repo: "repo1", Number: 10, Title: "fix: thing", Author: "bot"},
			},
		},
		Hold: github.HoldResult{Total: 1},
	}
}

func TestBuildKickMessages_Scanner_Extra(t *testing.T) {
	s := newScheduler()
	actionable := extraActionable()
	msgs := s.BuildKickMessages(actionable, []string{"scanner"})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Agent != "scanner" {
		t.Errorf("agent = %q", msgs[0].Agent)
	}
	if !strings.Contains(msgs[0].Message, "[agent:scanner]") {
		t.Error("expected scanner header")
	}
	if !strings.Contains(msgs[0].Message, "AUTHORIZED REPOS") {
		t.Error("expected repos section")
	}
}

func TestBuildKickMessages_CIMaintainer(t *testing.T) {
	s := newScheduler()
	actionable := extraActionable()
	msgs := s.BuildKickMessages(actionable, []string{"ci-maintainer"})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Message, "[agent:ci-maintainer]") {
		t.Error("expected ci-maintainer header")
	}
}

func TestBuildKickMessages_Supervisor_Extra(t *testing.T) {
	s := newScheduler()
	actionable := extraActionable()
	msgs := s.BuildKickMessages(actionable, []string{"supervisor"})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Message, "[agent:supervisor]") {
		t.Error("expected supervisor header")
	}
	if !strings.Contains(msgs[0].Message, "NEVER work on issues") {
		t.Error("expected supervisor restrictions")
	}
}

func TestBuildKickMessages_Tester(t *testing.T) {
	s := newScheduler()
	actionable := extraActionable()
	msgs := s.BuildKickMessages(actionable, []string{"quality"})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Message, "[agent:quality]") {
		t.Error("expected quality header")
	}
	if !strings.Contains(msgs[0].Message, "coverage") && !strings.Contains(msgs[0].Message, "COVERAGE TARGET") {
		t.Error("expected coverage content in quality message")
	}
}

func TestBuildKickMessages_Architect_Extra(t *testing.T) {
	s := newScheduler()
	actionable := extraActionable()
	msgs := s.BuildKickMessages(actionable, []string{"architect"})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Message, "[agent:architect]") {
		t.Error("expected architect header")
	}
}

func TestBuildKickMessages_Outreach_Extra(t *testing.T) {
	s := newScheduler()
	actionable := extraActionable()
	msgs := s.BuildKickMessages(actionable, []string{"outreach"})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Message, "[agent:outreach]") {
		t.Error("expected outreach header")
	}
	// Outreach should NOT have repos section appended
	if strings.Contains(msgs[0].Message, "AUTHORIZED REPOS (you may ONLY") {
		t.Error("outreach should not have repos section appended")
	}
}

func TestBuildKickMessages_SecCheck(t *testing.T) {
	s := newScheduler()
	actionable := extraActionable()
	msgs := s.BuildKickMessages(actionable, []string{"sec-check"})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Message, "[agent:sec-check]") {
		t.Error("expected sec-check header")
	}
}

func TestBuildKickMessages_EmptyAgentList_Extra(t *testing.T) {
	s := newScheduler()
	actionable := extraActionable()
	msgs := s.BuildKickMessages(actionable, []string{})
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages, got %d", len(msgs))
	}
}

func TestFilterByLane_Extra(t *testing.T) {
	issues := []github.Issue{
		{Number: 1, Lane: "scanner"},
		{Number: 2, Lane: "architect"},
		{Number: 3, Lane: ""}, // empty lane matches all
		{Number: 4, Lane: "quality"},
	}

	result := filterByLane(issues, "scanner")
	if len(result) != 2 { // #1 (scanner) + #3 (empty)
		t.Errorf("filterByLane(scanner) = %d, want 2", len(result))
	}
}

func TestExtractKeywords_NoiseFiltering(t *testing.T) {
	issues := []github.Issue{
		{Labels: []string{"kind/bug", "help wanted"}, ComplexityTier: "simple"},
		{Labels: []string{"kind/bug", "performance"}, ComplexityTier: "moderate"},
	}

	keywords := extractKeywords(issues)
	if len(keywords) == 0 {
		t.Error("expected non-empty keywords")
	}

	// "kind/bug" and "help wanted" are noise labels -- should be filtered
	for _, kw := range keywords {
		if kw == "kind/bug" || kw == "help wanted" {
			t.Errorf("noise label %q should be filtered", kw)
		}
	}

	// "performance" should be present
	found := false
	for _, kw := range keywords {
		if kw == "performance" {
			found = true
		}
	}
	if !found {
		t.Error("expected 'performance' in keywords")
	}
}

func TestBuildReposSection_Extra(t *testing.T) {
	s := newScheduler()
	section := s.buildReposSection()
	if !strings.Contains(section, "test-org/console") {
		t.Error("expected test-org/console in repos section")
	}
}

// TestBuildReposSection_ScopeNotProvisioning covers #4464: the repos section is
// what `include_repos: true` puts in a kick, and a guide agent read it as a
// promise that the repos were on disk — reporting "Repository worktree not
// provisioned for guide agent despite include_repos=true configuration" as an
// infrastructure blocker, which then sat in the operator's advisory digest.
// Nothing provisions a per-agent worktree, so the section must say what the
// list is and is not, and must say that an absent checkout is not a fault.
func TestBuildReposSection_ScopeNotProvisioning(t *testing.T) {
	section := newScheduler().buildReposSection()
	for _, want := range []string{
		"AUTHORIZATION SCOPE",
		"per-agent git worktree",
		"clone one yourself",
		"NOT an infrastructure fault",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("repos section missing %q — an agent can still read the list as a provisioning promise:\n%s", want, section)
		}
	}
}

func TestBuildReposSection_FullPaths(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{
			Org:   "myorg",
			Repos: []string{"otherorg/special-repo", "repo1"},
		},
	}
	s := New(cfg, testLogger())
	section := s.buildReposSection()
	if !strings.Contains(section, "otherorg/special-repo") {
		t.Error("expected full path for cross-org repo")
	}
}

func TestScannerMessage_SLAViolations_Extra(t *testing.T) {
	s := newScheduler()
	actionable := &github.ActionableResult{
		Issues: github.IssueResult{
			Count:         1,
			SLAViolations: 3,
			Items:         []github.Issue{{Repo: "repo1", Number: 1, Title: "issue", Lane: "scanner"}},
		},
		PRs: github.PRResult{Count: 0},
	}
	msgs := s.BuildKickMessages(actionable, []string{"scanner"})
	if !strings.Contains(msgs[0].Message, "[agent:scanner]") {
		t.Error("expected scanner kick header")
	}
}

func TestSetPrimer(t *testing.T) {
	s := newScheduler()
	if s.primer != nil {
		t.Error("primer should be nil initially")
	}
	s.SetPrimer(nil)
	if s.primer != nil {
		t.Error("primer should still be nil")
	}
}
