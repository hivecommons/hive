package scheduler

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/redmain"
)

// Resident kick goldens. These pin the full text a resident agent is kicked
// with — manual kick (BuildAgentMessageFromLastActionable) and cadence kick
// (BuildKickMessages) — for a scanner and a quality agent over a fixed queue
// with every dynamic seam populated. A change to how the role GUIDANCE is
// rendered for a contributor (BuildAgentGuidance) must leave every one of
// these byte-identical.
//
// The agents use a compact stand-in template (kickGoldenTemplate) that places
// every work-list and content variable, so the goldens pin the kick ASSEMBLY —
// resolution, substitution and every seam — without breaking on each edit to
// the shipped policy prose.
//
// Regenerate deliberately with HIVE_UPDATE_KICK_GOLDENS=1 after a reviewed
// change to the resident kick.

const kickGoldenTemplate = `# ${AGENT_DISPLAY_NAME} golden policy

${GH_AUTH}

You are the **${AGENT_NAME}** agent for ${PROJECT_NAME} (${PROJECT_PRIMARY_REPO}; repos: ${PROJECT_REPOS_LIST}).

## Rules

- Only work items from the kick message.

## Merge Sweep

Merge each PR in the MERGE-ELIGIBLE list with hive-merge.

${MERGE_ELIGIBLE}

## Workflow

1. Merge sweep, then dispatch sub-agents.

## Work Lists

CI-FAILING PRs:
${CI_FAILING}

MERGE-ELIGIBLE PRs:
${MERGE_ELIGIBLE}

ACTIONABLE ISSUES (${QUEUE_ISSUES}):
${ISSUE_LIST}

ACTIONABLE PRs (${QUEUE_PRS}):
${PR_LIST}

${IN_FLIGHT}

${WORK_TRACKER}

${WRITING_GUIDE}

${KNOWLEDGE}
`

const kickGoldenMergeEligible = `{"merge_eligible":[
  {"number":41,"repo":"golden-org/console","title":"ready to merge"}
],"needs_maintainer_merge":[
  {"number":42,"repo":"golden-org/console","title":"contributor PR","reason":"contributor tier"}
]}`

const kickGoldenCIFailing = `{"generated_at":"2026-11-01T00:00:00Z","ci_failing":[
  {"number":51,"repo":"golden-org/console","title":"red scanner PR","agent":"scanner","failing_checks":["build-gate"],"excerpt":"main.go:1: boom"},
  {"number":52,"repo":"golden-org/console","title":"red quality PR","agent":"quality","failing_checks":["go test"]}
]}`

// kickGoldenScheduler is a hermetic L6 scheduler: both agents on the stand-in
// template, the policy dirs and metrics files redirected, canaries off.
func kickGoldenScheduler(t *testing.T) *Scheduler {
	t.Helper()
	redirectPolicySeams(t)
	dir := t.TempDir()

	origME, origCI, origRT := mergeEligiblePath, ciFailingPath, reviewThreadsPath
	mergeEligiblePath = filepath.Join(dir, "merge-eligible.json")
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	reviewThreadsPath = filepath.Join(dir, "review-threads.json")
	t.Cleanup(func() { mergeEligiblePath, ciFailingPath, reviewThreadsPath = origME, origCI, origRT })
	if err := os.WriteFile(mergeEligiblePath, []byte(kickGoldenMergeEligible), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ciFailingPath, []byte(kickGoldenCIFailing), 0o644); err != nil {
		t.Fatal(err)
	}
	agentsDir := filepath.Join(dir, "examples", "kubestellar", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "golden-policy.md"), []byte(kickGoldenTemplate), 0o644); err != nil {
		t.Fatal(err)
	}

	level, off := 6, false
	cfg := &config.Config{
		ACMMLevel: &level,
		Project: config.ProjectConfig{
			Org: "golden-org", Name: "Golden", PrimaryRepo: "console",
			Repos: []string{"golden-org/console", "golden-org/docs"},
		},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner": {Role: "scanner", Enabled: true, Mode: "ISSUES_AND_PRS", KickTemplate: "golden-policy.md"},
				"quality": {Role: "quality", Enabled: true, Mode: "ISSUES_AND_PRS", KickTemplate: "golden-policy.md"},
			},
		},
		Policies: config.PoliciesConfig{LocalDir: dir},
		Ioscan:   config.IoscanConfig{Enabled: &off, Canaries: &off},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(cfg, logger)
}

func kickGoldenActionable() *github.ActionableResult {
	issues := []github.Issue{
		{Repo: "golden-org/console", Number: 11, Title: "nil pointer in the scheduler loop", Labels: []string{"kind/bug"}, AgeMinutes: 30},
		{Repo: "golden-org/console", Number: 12, Title: "missing test coverage for the API", Labels: []string{"test-coverage"}, AgeMinutes: 90},
		{Repo: "golden-org/docs", Number: 13, Title: "typo in the install guide", Labels: []string{"kind/docs"}, AgeMinutes: 5},
	}
	prs := []github.PullRequest{
		{Repo: "golden-org/console", Number: 61, Title: "an open PR", Author: "someone"},
	}
	since := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	return &github.ActionableResult{
		Issues: github.IssueResult{Items: issues, Count: len(issues), SLAViolations: 1},
		PRs:    github.PRResult{Items: prs, Count: len(prs)},
		RedMain: map[string]*redmain.Fact{"golden-org/console": {
			Repo: "golden-org/console", Branch: "main", Check: "unit", Since: since, TrackingIssue: 70,
		}},
	}
}

func checkKickGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "kick-golden", name)
	if os.Getenv("HIVE_UPDATE_KICK_GOLDENS") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with HIVE_UPDATE_KICK_GOLDENS=1)", path, err)
	}
	if got != string(want) {
		common := 0
		for common < len(got) && common < len(want) && got[common] == want[common] {
			common++
		}
		t.Fatalf("resident kick %s changed at byte %d:\n got: %q\nwant: %q", name, common,
			got[common:min(len(got), common+200)], string(want[common:min(len(want), common+200)]))
	}
}

func TestResidentKickGoldensUnchanged(t *testing.T) {
	for _, agent := range []string{"scanner", "quality"} {
		t.Run("manual/"+agent, func(t *testing.T) {
			s := kickGoldenScheduler(t)
			s.SetLastActionable(kickGoldenActionable())
			checkKickGolden(t, "manual-"+agent+".txt", s.BuildAgentMessageFromLastActionable(agent))
		})
		t.Run("cadence/"+agent, func(t *testing.T) {
			s := kickGoldenScheduler(t)
			msgs := s.BuildKickMessages(kickGoldenActionable(), []string{agent})
			if len(msgs) != 1 {
				t.Fatalf("messages = %d, want 1", len(msgs))
			}
			checkKickGolden(t, "cadence-"+agent+".txt", msgs[0].Message)
		})
	}
}
