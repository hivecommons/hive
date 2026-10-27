package scheduler

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/ioscan"
)

// kickLayoutTemplate is a stand-in for a shipped policy: static prose, the
// auth block (a constant), more prose, then the dynamic lists where the
// shipped templates put them.
const kickLayoutTemplate = `# Scanner policy

${GH_AUTH}

STATIC POLICY CORE — rules the agent follows on every kick.

## Work list
${ISSUE_LIST}

## PRs
${PR_LIST}
`

// kickLayoutScheduler builds a repo-scoped, PR-capable scanner on an L3 hive
// with canaries, independent review and a red-PR fixture, so every static
// seam and every dynamic seam has something to render.
func kickLayoutScheduler(t *testing.T) *Scheduler {
	t.Helper()
	redirectPolicySeams(t)
	swapDefaultCanaries(t)

	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "examples", "kubestellar", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "layout-scanner.md"), []byte(kickLayoutTemplate), 0o644); err != nil {
		t.Fatal(err)
	}
	origCI := ciFailingPath
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	t.Cleanup(func() { ciFailingPath = origCI })
	if err := os.WriteFile(ciFailingPath, []byte(redPRFixture), 0o644); err != nil {
		t.Fatal(err)
	}

	level, on := 3, true
	cfg := &config.Config{
		ACMMLevel: &level,
		Project:   config.ProjectConfig{Org: "test-org", PrimaryRepo: "console", Repos: []string{"test-org/console", "test-org/docs"}},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner": {
					Mode:         "ISSUES_AND_PRS",
					KickTemplate: "layout-scanner.md",
					ReviewModels: config.ReviewModelsConfig{Pool: []config.ReviewModelPoolEntry{{Backend: "claude", Model: "opus"}}},
				},
			},
		},
		Policies: config.PoliciesConfig{LocalDir: dir},
		Ioscan:   config.IoscanConfig{Enabled: &on, Canaries: &on},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(cfg, logger)
}

func kickLayoutActionable(issues ...github.Issue) *github.ActionableResult {
	prs := []github.PullRequest{{Repo: "test-org/console", Number: 300 + len(issues), Title: "a pr", Author: "someone"}}
	return &github.ActionableResult{
		Issues: github.IssueResult{Items: issues, Count: len(issues)},
		PRs:    github.PRResult{Items: prs, Count: len(prs)},
		Hold: github.HoldResult{PRs: 1, Total: 1, Items: []github.HoldItem{
			{Repo: "test-org/console", Number: 26, Type: "pr", Title: "occupied work"},
		}},
	}
}

func kickLayoutMessage(t *testing.T, s *Scheduler, issues ...github.Issue) string {
	t.Helper()
	msgs := s.BuildKickMessages(kickLayoutActionable(issues...), []string{config.CadenceTargetKey("scanner", "test-org/console")})
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	return msgs[0].Message
}

// stripCanary removes the random canary preamble, which is deliberately the
// one dynamic thing ahead of the static prefix (ADR-0008).
func stripCanary(t *testing.T, msg string) string {
	t.Helper()
	if !strings.HasPrefix(msg, "SECURITY CANARY: "+ioscan.CanaryPrefix) {
		t.Fatalf("canary preamble must lead the kick:\n%s", msg[:min(len(msg), 200)])
	}
	end := strings.Index(msg, "\n\n")
	if end < 0 {
		t.Fatalf("canary preamble not terminated by a blank line")
	}
	return msg[end+2:]
}

// Two kicks for the same agent with different work lists share a byte-identical
// prefix that runs through the header, every static seam and the policy core
// up to the first dynamic list — the property provider prompt caching needs
// (hivecommons/hive#12125). The canary stays first and random.
func TestKickLayoutStaticPrefixIsCacheable(t *testing.T) {
	s := kickLayoutScheduler(t)
	a := stripCanary(t, kickLayoutMessage(t, s,
		makeIssue("test-org/console", 1, "first issue", "scanner", 5, nil, false)))
	b := stripCanary(t, kickLayoutMessage(t, s,
		makeIssue("test-org/console", 2, "second issue", "scanner", 5, nil, false),
		makeIssue("test-org/console", 3, "third issue", "scanner", 5, nil, false)))
	if a == b {
		t.Fatal("kicks with different issue lists must differ somewhere")
	}

	common := 0
	for common < len(a) && common < len(b) && a[common] == b[common] {
		common++
	}
	prefix := a[:common]
	firstDynamic := strings.Index(a, "## Work list")
	if firstDynamic < 0 {
		t.Fatalf("template work-list heading missing:\n%s", a)
	}
	// The shared prefix must reach past the policy core into the work list.
	if common < firstDynamic {
		t.Fatalf("shared prefix ends at byte %d, before the first dynamic list at %d; divergence:\n%q\nvs\n%q",
			common, firstDynamic, a[common:min(len(a), common+120)], b[common:min(len(b), common+120)])
	}

	// Static content in the prefix, in the documented order.
	static := []string{
		"[agent:scanner]",
		"HOUSE STYLE (source: built-in)",
		governorWorkSourceRule,
		"## Independent review",
		"## Workflow files are out of reach at this mode",
		"# Scanner policy",
		"## Project Authentication",
		"STATIC POLICY CORE",
	}
	last := -1
	for _, want := range static {
		at := strings.Index(prefix, want)
		if at < 0 {
			t.Errorf("static prefix missing %q (prefix is %d bytes):\n%s", want, len(prefix), prefix)
			continue
		}
		if at < last {
			t.Errorf("%q at %d is out of documented order (previous static item at %d)", want, at, last)
		}
		last = at
	}

	// Dynamic content must all land after the prefix.
	for _, dyn := range []string{
		"first issue",
		"REPO-SCOPED CADENCE TARGET: test-org/console",
		"AUTHORIZED REPOS",
		"## Open hold-gated PR coordination",
		"FIX-BEFORE-NEW",
		inProgressClaimRule,
		"Begin now:",
	} {
		at := strings.Index(a, dyn)
		if at < 0 {
			t.Errorf("kick missing %q:\n%s", dyn, a)
			continue
		}
		if at < common {
			t.Errorf("dynamic/tail element %q at %d sits inside the shared static prefix (ends %d)", dyn, at, common)
		}
	}
}

// Every mandatory seam is still present and in the documented order after
// the reordering — nothing was dropped or folded into the template.
func TestKickLayoutSeamOrderIsFixed(t *testing.T) {
	s := kickLayoutScheduler(t)
	msg := kickLayoutMessage(t, s, makeIssue("test-org/console", 1, "first issue", "scanner", 5, nil, false))

	order := []string{
		"SECURITY CANARY: " + ioscan.CanaryPrefix,
		"[agent:scanner]",
		governorWorkSourceRule,
		"## Independent review",
		"## Workflow files are out of reach at this mode",
		"# Scanner policy",
		"## Project Authentication",
		"first issue",
		"REPO-SCOPED CADENCE TARGET: test-org/console",
		"AUTHORIZED REPOS",
		"## Open hold-gated PR coordination",
		"🔴 FIX-BEFORE-NEW",
		inProgressClaimRule,
		concreteKickClosingInstruction,
	}
	last := -1
	for _, want := range order {
		at := strings.Index(msg, want)
		if at < 0 {
			t.Fatalf("kick missing %q:\n%s", want, msg)
		}
		if at < last {
			t.Errorf("%q at %d is out of documented order (previous at %d):\n%s", want, at, last, msg)
		}
		last = at
	}
	if !strings.HasSuffix(strings.TrimSpace(msg), concreteKickClosingInstruction) {
		t.Errorf("closing instruction must be last:\n%s", msg)
	}
}

// The canary preamble and the repo-scoped target survive the reorder: the
// canary is first and differs per kick (random by construction), and the
// target is still spelled out for the agent.
func TestKickLayoutKeepsCanaryAndRepoScopedTarget(t *testing.T) {
	s := kickLayoutScheduler(t)
	issue := makeIssue("test-org/console", 1, "first issue", "scanner", 5, nil, false)
	a := kickLayoutMessage(t, s, issue)
	b := kickLayoutMessage(t, s, issue)

	preamble := func(msg string) string {
		end := strings.Index(msg, "\n")
		if end < 0 {
			return msg
		}
		return msg[:end]
	}
	for _, msg := range []string{a, b} {
		if !strings.HasPrefix(msg, "SECURITY CANARY: "+ioscan.CanaryPrefix) {
			t.Fatalf("canary preamble must lead the kick:\n%s", msg[:min(len(msg), 200)])
		}
		if !strings.Contains(msg, "REPO-SCOPED CADENCE TARGET: test-org/console") {
			t.Fatalf("repo-scoped target missing:\n%s", msg)
		}
	}
	if preamble(a) == preamble(b) {
		t.Fatalf("canary must vary per kick, got %q twice", preamble(a))
	}
	// Identical inputs → identical kick once the random canary is removed.
	if stripCanary(t, a) != stripCanary(t, b) {
		t.Errorf("same inputs must render the same kick after the canary")
	}
}

// Manual kicks (no cadence target, no repos block) get the same static prefix
// as scheduled ones — the layout is a property of assembleKick, not of the
// caller.
func TestKickLayoutManualKickSharesPrefix(t *testing.T) {
	s := kickLayoutScheduler(t)
	issue := makeIssue("test-org/console", 1, "first issue", "scanner", 5, nil, false)
	scheduled := stripCanary(t, kickLayoutMessage(t, s, issue))
	manual := s.BuildAgentMessage("scanner", []github.Issue{issue}, kickLayoutActionable(issue))

	cut := strings.Index(scheduled, "## Work list")
	if cut < 0 || !strings.HasPrefix(manual, scheduled[:cut]) {
		t.Fatalf("manual kick must share the static prefix:\n--- scheduled\n%s\n--- manual\n%s", scheduled, manual)
	}
	if strings.Contains(manual, "AUTHORIZED REPOS") || strings.Contains(manual, "REPO-SCOPED") {
		t.Errorf("manual kick must not grow the repos block:\n%s", manual)
	}
}
