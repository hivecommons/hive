package scheduler

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/github"
)

func heldPRCoordinationScheduler(t *testing.T, level int, mode string) *Scheduler {
	t.Helper()
	redirectPolicySeams(t)
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "examples", "kubestellar", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "custom-quality.md"), []byte("CUSTOM POLICY BODY\n${ISSUE_LIST}\n${PR_LIST}"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		ACMMLevel: &level,
		Project: config.ProjectConfig{
			Org: "acme", Name: "Acme", PrimaryRepo: "widget", Repos: []string{"acme/widget"},
		},
		Agents: map[string]config.AgentConfig{
			"quality": {Role: "quality", Mode: mode, KickTemplate: "custom-quality.md"},
		},
		Policies: config.PoliciesConfig{LocalDir: dir},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(cfg, logger)
}

func TestHeldPRCoordinationReachesCustomizedAndManualKicks(t *testing.T) {
	s := heldPRCoordinationScheduler(t, 3, "ISSUES_AND_PRS")
	actionable := &github.ActionableResult{
		PRs: github.PRResult{}, // held PRs are deliberately absent here
		Hold: github.HoldResult{Total: 2, PRs: 1, Issues: 1, Items: []github.HoldItem{
			{Repo: "acme/widget", Number: 26, Type: "pr", Title: "Test run_main and main exit mapping"},
			{Repo: "acme/widget", Number: 99, Type: "issue", Title: "operator-held issue"},
		}},
	}

	// BuildAgentMessage is the manual-kick entry point as well as the renderer
	// used by scheduled kicks. The guard must wrap a customized template, not
	// depend on an operator refreshing their policy copy.
	msg := s.BuildAgentMessage("quality", nil, actionable)
	for _, want := range []string{
		"[agent:quality]",
		"Open hold-gated PR coordination — mandatory preflight",
		"acme/widget#26 Test run_main and main exit mapping",
		"gh pr view <number> --repo <repo> --json title,body,files",
		"choose a disjoint cluster",
		"CUSTOM POLICY BODY",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("kick missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "operator-held issue") {
		t.Fatalf("non-PR hold leaked into occupied PR claims:\n%s", msg)
	}
	if strings.Index(msg, "mandatory preflight") > strings.Index(msg, "CUSTOM POLICY BODY") {
		t.Fatalf("coordination must precede policy work selection:\n%s", msg)
	}
}

func TestHeldPRCoordinationRespectsCapabilityAndMergePolicy(t *testing.T) {
	actionable := &github.ActionableResult{Hold: github.HoldResult{Items: []github.HoldItem{
		{Repo: "acme/widget", Number: 26, Type: "pr", Title: "occupied work"},
	}}}
	for _, tc := range []struct {
		name  string
		level int
		mode  string
	}{
		{name: "hold-gated but issue-only", level: 4, mode: "ISSUES_ONLY"},
		{name: "PR-capable but auto-merge level", level: 6, mode: "ISSUES_AND_PRS"},
		{name: "manual level", level: 2, mode: "ISSUES_AND_PRS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := heldPRCoordinationScheduler(t, tc.level, tc.mode)
			msg := s.BuildAgentMessage("quality", nil, actionable)
			if strings.Contains(msg, "Open hold-gated PR coordination") {
				t.Fatalf("coordination applied outside a hold-gated PR writer:\n%s", msg)
			}
		})
	}
}

func TestHeldPRCoordinationScansUntrustedTitles(t *testing.T) {
	s := heldPRCoordinationScheduler(t, 3, "ISSUES_AND_PRS")
	actionable := &github.ActionableResult{Hold: github.HoldResult{Items: []github.HoldItem{
		{Repo: "acme/widget", Number: 26, Type: "pr", Title: blockingTitle},
	}}}
	msg := s.BuildAgentMessage("quality", nil, actionable)
	if strings.Contains(msg, blockingTitle) {
		t.Fatalf("raw held-PR injection leaked into kick:\n%s", msg)
	}
}

func TestHeldPRCoordinationFailsClosedWhenSnapshotDoesNotFit(t *testing.T) {
	s := heldPRCoordinationScheduler(t, 3, "ISSUES_AND_PRS")
	items := make([]github.HoldItem, maxIssuesPerKick+1)
	for i := range items {
		items[i] = github.HoldItem{Repo: "acme/widget", Number: i + 1, Type: "pr", Title: "occupied work"}
	}
	msg := s.BuildAgentMessage("quality", nil, &github.ActionableResult{
		Hold: github.HoldResult{PRs: len(items), Total: len(items), Items: items},
	})
	if !strings.Contains(msg, "1 additional open held PRs omitted; STAND DOWN this kick") ||
		!strings.Contains(msg, "unseen occupied ground cannot be proven disjoint") {
		t.Fatalf("truncated snapshot did not fail closed:\n%s", msg)
	}
}
