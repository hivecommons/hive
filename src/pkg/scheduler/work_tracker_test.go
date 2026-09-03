package scheduler

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func workTrackerScheduler(t *testing.T, wsType, template string) *Scheduler {
	t.Helper()
	redirectPolicySeams(t)
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "examples", "kubestellar", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "custom-quality.md"), []byte(template), 0o644); err != nil {
		t.Fatal(err)
	}
	level := 3
	cfg := &config.Config{
		ACMMLevel: &level,
		Project: config.ProjectConfig{
			Org: "acme", Name: "Acme", PrimaryRepo: "widget", Repos: []string{"acme/widget"},
		},
		Agents: map[string]config.AgentConfig{
			"quality": {Role: "quality", Mode: "ISSUES_AND_PRS", KickTemplate: "custom-quality.md"},
		},
		Policies: config.PoliciesConfig{LocalDir: dir},
	}
	cfg.Governor.WorkSource = config.WorkSourceConfig{
		Type: wsType,
		Linear: config.LinearSourceConfig{
			APIKey:       "k",
			HoldLabels:   []string{"hold", "on-hold"},
			AssignedOnly: true,
			Teams: []config.LinearTeamSourceConfig{
				{Key: "ENG", Repo: "acme/widget", States: []string{"Todo"}, Projects: []config.LinearProjectSourceConfig{{Name: "Console", Repo: "acme/console"}}},
			},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(cfg, logger)
}

// A Linear-sourced hive gets the tracker section appended to a customized
// template that never mentions it — same seam as held-PR coordination.
func TestWorkTracker_AppendedForLinearSource(t *testing.T) {
	s := workTrackerScheduler(t, "linear", "CUSTOM POLICY BODY\n${ISSUE_LIST}\n${PR_LIST}")
	msg := s.BuildAgentMessage("quality", nil, &github.ActionableResult{})
	for _, want := range []string{
		"CUSTOM POLICY BODY",
		workTrackerHeader,
		"ENG → acme/widget (states: Todo)",
		`project "Console" → acme/console`,
		"LINEAR_ACCESS_TOKEN",
		"LINEAR_API_KEY",
		"issueCreate",
		"Fixes TEAM-123",
		"`[TEAM-123]: <short description>`",
		"Part of TEAM-123",
		"hold, on-hold",
		"assigned or delegated",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("kick missing %q:\n%s", want, msg)
		}
	}
	if strings.Count(msg, workTrackerHeader) != 1 {
		t.Errorf("tracker section should appear exactly once:\n%s", msg)
	}
}

// ${WORK_TRACKER} places the section explicitly; the seam must not add a
// second copy.
func TestWorkTracker_ExplicitPlacementNotDuplicated(t *testing.T) {
	s := workTrackerScheduler(t, "linear", "HEAD\n${WORK_TRACKER}\nTAIL\n${ISSUE_LIST}")
	msg := s.BuildAgentMessage("quality", nil, &github.ActionableResult{})
	if strings.Count(msg, workTrackerHeader) != 1 {
		t.Fatalf("expected exactly one tracker section:\n%s", msg)
	}
	if strings.Index(msg, workTrackerHeader) > strings.Index(msg, "TAIL") {
		t.Errorf("section should be where ${WORK_TRACKER} was placed, before TAIL:\n%s", msg)
	}
}

// GitHub-sourced hives are untouched: no section, and ${WORK_TRACKER}
// resolves to nothing.
func TestWorkTracker_AbsentForGitHubSource(t *testing.T) {
	for _, ws := range []string{"", "github"} {
		s := workTrackerScheduler(t, ws, "BODY\n${WORK_TRACKER}\n${ISSUE_LIST}")
		msg := s.BuildAgentMessage("quality", nil, &github.ActionableResult{})
		if strings.Contains(msg, workTrackerHeader) || strings.Contains(msg, "${WORK_TRACKER}") {
			t.Errorf("work_source %q: unexpected tracker content:\n%s", ws, msg)
		}
	}
}

// A fail-closed (empty) kick stays empty.
func TestWorkTracker_EmptyMessageStaysEmpty(t *testing.T) {
	s := workTrackerScheduler(t, "linear", "x")
	if got := s.addWorkTrackerSection(""); got != "" {
		t.Errorf("addWorkTrackerSection(\"\") = %q", got)
	}
}
