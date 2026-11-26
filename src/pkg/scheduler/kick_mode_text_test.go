package scheduler

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/policies"
)

// modeKickTemplate is a minimal policy so the only write instructions in the
// rendered kick come from the seams under test.
const modeKickTemplate = "# Mode policy\n\n## Work list\n${ISSUE_LIST}\n"

// modeKickScheduler builds a scheduler with one "scanner" agent at the given
// configured mode (empty = unset) and ACMM level, using modeKickTemplate.
func modeKickScheduler(t *testing.T, mode string, level int) *Scheduler {
	t.Helper()
	redirectPolicySeams(t)
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "examples", "kubestellar", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "mode-scanner.md"), []byte(modeKickTemplate), 0o644); err != nil {
		t.Fatal(err)
	}
	origCI := ciFailingPath
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	t.Cleanup(func() { ciFailingPath = origCI })

	cfg := &config.Config{
		ACMMLevel: &level,
		Project:   config.ProjectConfig{Org: "test-org", Repos: []string{"test-org/console", "test-org/docs"}},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner": {Mode: mode, KickTemplate: "mode-scanner.md"},
			},
		},
		Policies: config.PoliciesConfig{LocalDir: dir},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(cfg, logger)
}

func modeKick(t *testing.T, s *Scheduler, agent string) string {
	t.Helper()
	issue := makeIssue("test-org/console", 7, "an open issue", agent, 5, nil, false)
	msgs := s.BuildKickMessages(actionableWithIssues([]github.Issue{issue}), []string{agent})
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	return msgs[0].Message
}

// hivecommons/hive#20037: the claim and closing seams are in every kick, but
// their text must not ask for a write the agent's mode cannot make. The issue
// relay refuses claim/issue/label below ISSUES_ONLY and the PR and push relays
// refuse below ISSUES_AND_PRS.
func TestKickSeamsMatchAgentMode(t *testing.T) {
	const level = 4
	for _, tc := range []struct {
		mode        string
		wantClaim   string
		wantClosing string
		banned      []string
	}{
		{
			mode: "ADVISORY", wantClaim: inProgressClaimRuleAdvisory, wantClosing: concreteKickClosingInstructionAdvisory,
			banned: []string{"filing issues", "PRs through the hive relays", "hive-open-issue claim"},
		},
		{
			mode: "ISSUES_ONLY", wantClaim: inProgressClaimRuleIssuesOnly, wantClosing: concreteKickClosingInstructionIssuesOnly,
			banned: []string{"PRs through the hive relays", "hive-open-issue claim"},
		},
		{mode: "ISSUES_AND_PRS", wantClaim: inProgressClaimRule, wantClosing: concreteKickClosingInstruction},
		{mode: "ISSUES_PRS_MERGE", wantClaim: inProgressClaimRule, wantClosing: concreteKickClosingInstruction},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			msg := modeKick(t, modeKickScheduler(t, tc.mode, level), "scanner")
			for _, banned := range tc.banned {
				if strings.Contains(msg, banned) {
					t.Errorf("%s kick asks for a refused write %q:\n%s", tc.mode, banned, msg)
				}
			}
			claimAt := strings.Index(msg, tc.wantClaim)
			closingAt := strings.Index(msg, tc.wantClosing)
			if claimAt < 0 || closingAt < 0 {
				t.Fatalf("%s kick must keep a claim seam and a closing seam (claim at %d, closing at %d):\n%s", tc.mode, claimAt, closingAt, msg)
			}
			if claimAt > closingAt || !strings.HasSuffix(strings.TrimSpace(msg), tc.wantClosing) {
				t.Errorf("%s kick: seam order changed, the claim seam precedes the closing line and the closing line ends the kick:\n%s", tc.mode, msg)
			}
		})
	}
}

// With no `mode:` the relay falls back to crux.DefaultAgentMode at the hive's
// ACMM level; the seams must use that same mode, or the two disagree.
func TestKickSeamsUseRelayDefaultModeWhenUnset(t *testing.T) {
	for _, tc := range []struct {
		level     int
		wantClaim string
	}{
		{level: 2, wantClaim: inProgressClaimRuleAdvisory},
		{level: 4, wantClaim: inProgressClaimRuleIssuesOnly},
		{level: 6, wantClaim: inProgressClaimRule},
	} {
		s := modeKickScheduler(t, "", tc.level)
		if got, want := s.kickWriteMode("scanner"), crux.DefaultAgentMode("scanner", tc.level); got != want {
			t.Fatalf("L%d: seam mode %v, relay default %v", tc.level, got, want)
		}
		if msg := modeKick(t, s, "scanner"); !strings.Contains(msg, tc.wantClaim) {
			t.Errorf("L%d: unset mode must get the relay default's claim seam %q:\n%s", tc.level, tc.wantClaim, msg)
		}
	}
}

// supervisorPackScheduler configures every agent of a shipped ACMM pack the
// way the pack does, so the supervisor kick renders from its real template.
func supervisorPackScheduler(t *testing.T, level int) *Scheduler {
	t.Helper()
	redirectPolicySeams(t)
	origCI := ciFailingPath
	ciFailingPath = filepath.Join(t.TempDir(), "ci-failing.json")
	t.Cleanup(func() { ciFailingPath = origCI })
	pack, err := config.ACMMPackByLevel(level)
	if err != nil {
		t.Fatal(err)
	}
	agents := map[string]config.AgentConfig{}
	for _, pa := range pack.Agents {
		include := pa.IncludeRepos
		agents[pa.Name] = config.AgentConfig{Role: pa.Role, Mode: pa.Mode, KickTemplate: pa.KickTemplate, IncludeRepos: &include}
	}
	cfg := &config.Config{
		ACMMLevel:          &level,
		Project:            config.ProjectConfig{Org: "test-org", Repos: []string{"test-org/console", "test-org/docs"}},
		AgentRuntimeConfig: config.AgentRuntimeConfig{Agents: agents},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(cfg, logger)
}

// hivecommons/hive#20038: the supervisor's policy forbids all GitHub use. The
// kick assembled around it must not tell it to use gh, clone and implement,
// claim issues, or follow the built-in code-and-PR house style, while still
// carrying the work-source, claim and closing seams (#12134).
func TestSupervisorKickFromPackHasNoGitHubInstructions(t *testing.T) {
	for _, level := range []int{2, 3, 4, 5, 6} {
		s := supervisorPackScheduler(t, level)
		msg := modeKick(t, s, "supervisor")
		if !strings.Contains(msg, "NO GitHub interaction whatsoever") {
			t.Fatalf("L%d: supervisor kick did not render supervisor-nogithub.md:\n%s", level, msg)
		}
		for _, banned := range []string{
			"gh pr view", "hive-gh",
			"git clone", "implement as the issue scopes it", "AUTHORIZED REPOS",
			"HOUSE STYLE (source: built-in)",
			"hive-open-issue claim", "filing issues/PRs", "filing issues",
		} {
			if strings.Contains(msg, banned) {
				t.Errorf("L%d: supervisor kick contains %q:\n%s", level, banned, msg)
			}
		}
		for _, seam := range []string{governorWorkSourceRuleNoGitHub, inProgressClaimRuleAdvisory, concreteKickClosingInstructionNoGitHub} {
			if !strings.Contains(msg, seam) {
				t.Errorf("L%d: supervisor kick lost a seam %q:\n%s", level, seam, msg)
			}
		}
	}
}

// The no-GitHub variants are for the ADVISORY supervisor only: an agent that
// may write keeps the ordinary seam text, as does a supervisor an operator
// promoted to a writing mode.
func TestNoGitHubVariantOnlyForAdvisorySupervisor(t *testing.T) {
	s := supervisorPackScheduler(t, 4)
	if !s.isNoGitHubAgent("supervisor") {
		t.Fatal("the pack supervisor must get the no-GitHub variant")
	}
	for _, agent := range []string{"scanner", "quality", "reviewer"} {
		if s.isNoGitHubAgent(agent) {
			t.Errorf("%s must not get the no-GitHub variant", agent)
		}
		if got := s.governorWorkSourceRuleFor(agent); got != governorWorkSourceRule {
			t.Errorf("%s work-source seam changed: %q", agent, got)
		}
	}
	promoted := s.cfg.Agents["supervisor"]
	promoted.Mode = "ISSUES_ONLY"
	s.cfg.Agents["supervisor"] = promoted
	if s.isNoGitHubAgent("supervisor") {
		t.Error("a supervisor promoted to ISSUES_ONLY must get the ordinary text")
	}
}

// hivecommons/hive#20044: at ACMM L3-L5 the PR-request watcher labels every
// agent PR ready-for-review and that label blocks the merge, so the
// FIX-BEFORE-NEW seam and the reviewer lane must not promise a merge on green.
// L6 keeps the sweep wording.
func TestMergeOnGreenTextFollowsHoldGate(t *testing.T) {
	label := "`" + github.ReadyForReviewLabelName() + "`"
	for _, tc := range []struct {
		level int
		held  bool
	}{{3, true}, {4, true}, {5, true}, {6, false}} {
		level := tc.level
		s := New(&config.Config{ACMMLevel: &level}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
		if got := s.hiveHoldsAgentPRs("scanner"); got != tc.held {
			t.Fatalf("L%d: hiveHoldsAgentPRs = %v, want %v", level, got, tc.held)
		}
		fix := strings.Replace(formatRedPRFixData([]byte(redPRFixture), "scanner"), redPRFixMergeAfterGreenDefault, s.mergeAfterGreenSentence("scanner"), 1)
		lane := s.buildReviewerMessageHardcoded("adjudicator", level, "(work list)")
		for name, text := range map[string]string{"fix-before-new": fix, "reviewer lane": lane} {
			promises := strings.Contains(text, redPRFixMergeAfterGreenDefault) || strings.Contains(text, "merges it the moment its checks are green")
			if tc.held && (promises || !strings.Contains(text, label)) {
				t.Errorf("L%d %s: held PR text must name %s and not promise a merge on green:\n%s", level, name, label, text)
			}
			if !tc.held && !promises {
				t.Errorf("L%d %s: L6 keeps the merge-on-green wording:\n%s", level, name, text)
			}
		}
	}
}

// The red-team lane is held at every level, L6 included.
func TestMergeOnGreenTextHoldsRedTeamAtL6(t *testing.T) {
	level := 6
	s := New(&config.Config{
		ACMMLevel:          &level,
		AgentRuntimeConfig: config.AgentRuntimeConfig{Agents: map[string]config.AgentConfig{"redteam": {Role: "redteam"}}},
	}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if !s.hiveHoldsAgentPRs("redteam") {
		t.Error("the red-team lane is held at L6 too")
	}
	if s.hiveHoldsAgentPRs("scanner") {
		t.Error("an ordinary L6 agent is not held")
	}
}

// The FIX-BEFORE-NEW seam as the kick renders it (redPRFixSection, not the
// formatter alone) follows the hold gate (#20044).
func TestRedPRFixSectionFollowsHoldGate(t *testing.T) {
	label := "`" + github.ReadyForReviewLabelName() + "`"
	for _, tc := range []struct {
		level int
		held  bool
	}{{4, true}, {6, false}} {
		s := modeKickScheduler(t, "ISSUES_AND_PRS", tc.level)
		if err := os.WriteFile(ciFailingPath, []byte(redPRFixture), 0o644); err != nil {
			t.Fatal(err)
		}
		section := s.redPRFixSection("scanner")
		if !strings.Contains(section, "FIX-BEFORE-NEW") {
			t.Fatalf("L%d: fixture rendered no fix-before-new section:\n%s", tc.level, section)
		}
		promises := strings.Contains(section, redPRFixMergeAfterGreenDefault)
		if tc.held && (promises || !strings.Contains(section, label)) {
			t.Errorf("L%d: held seam must name %s and not promise a merge on green:\n%s", tc.level, label, section)
		}
		if !tc.held && (!promises || strings.Contains(section, label)) {
			t.Errorf("L%d: the sweep merges green PRs here:\n%s", tc.level, section)
		}
	}
}

// The relays read `mode:` only (Manager.agentMode); a `tools:` block does not
// change what they allow, so it must not change the seam text either.
func TestKickWriteModeIgnoresToolsLikeTheRelay(t *testing.T) {
	s := modeKickScheduler(t, "ADVISORY", 4)
	agent := s.cfg.Agents["scanner"]
	agent.Tools = &config.ToolsConfig{}
	s.cfg.Agents["scanner"] = agent
	if got := s.kickWriteMode("scanner"); got != crux.ModeAdvisory {
		t.Fatalf("kickWriteMode = %v, want ADVISORY (the configured mode the relay checks)", got)
	}
}

// ${GH_AUTH} must not teach PR opening to an agent whose mode cannot push
// (#20039); a pushing agent keeps the full block.
func TestGHAuthForModeDropsPRStepsBelowPush(t *testing.T) {
	for _, tc := range []struct {
		mode string
		full bool
	}{{"ADVISORY", false}, {"ISSUES_ONLY", false}, {"ISSUES_AND_PRS", true}, {"ISSUES_PRS_MERGE", true}} {
		s := modeKickScheduler(t, tc.mode, 4)
		got := s.ghAuthInstructionsFor("scanner")
		if tc.full {
			if got != s.ghAuthInstructions() {
				t.Errorf("%s: a pushing agent must get the full block", tc.mode)
			}
			continue
		}
		for _, banned := range []string{"To OPEN A PULL REQUEST", "hive-open-pr --repo", "PR title rule", "git push / git fetch"} {
			if strings.Contains(got, banned) {
				t.Errorf("%s: ${GH_AUTH} still carries %q:\n%s", tc.mode, banned, got)
			}
		}
		for _, want := range []string{"## Project Authentication", ghAuthNoPushNote, ghAuthGhCLIBulletStart} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: ${GH_AUTH} lost %q:\n%s", tc.mode, want, got)
			}
		}
	}
	// The cut points must exist in the full block, or the cut silently stops.
	full := modeKickScheduler(t, "ISSUES_AND_PRS", 4).ghAuthInstructions()
	if strings.Index(full, ghAuthPRBulletStart) < 0 || strings.Index(full, ghAuthGhCLIBulletStart) < strings.Index(full, ghAuthPRBulletStart) {
		t.Fatal("ghAuthInstructions no longer has the bullets ghAuthInstructionsFor cuts")
	}
}

// #20042: every PR-capable pack template, with ${GH_AUTH} expanded, states the
// PR-title rule exactly once, and that rule is the one in ${GH_AUTH}.
func TestPRTitleRuleAppearsOnceInRenderedPackTemplates(t *testing.T) {
	const ghAuthRule = "title\n  the PR the way the TARGET repository titles PRs"
	checked := 0
	for _, pack := range config.ACMMPacks() {
		for _, a := range pack.Agents {
			mode, ok := crux.ParseAgentMode(a.Mode)
			if !ok || !mode.CanPush() || a.KickTemplate == "" {
				continue
			}
			raw, err := policies.DefaultPolicies.ReadFile("defaults/" + a.KickTemplate)
			if err != nil || !strings.Contains(string(raw), "${GH_AUTH}") {
				continue
			}
			s := modeKickScheduler(t, a.Mode, pack.Level)
			rendered := strings.ReplaceAll(string(raw), "${GH_AUTH}", s.ghAuthInstructionsFor("scanner"))
			checked++
			if n := strings.Count(rendered, ghAuthRule); n != 1 {
				t.Errorf("L%d %s: ${GH_AUTH} title rule appears %d times, want 1", pack.Level, a.KickTemplate, n)
			}
			if n := strings.Count(strings.ToLower(rendered), "titles prs"); n != 1 {
				t.Errorf("L%d %s: %d PR-title rules in the rendered kick, want exactly 1", pack.Level, a.KickTemplate, n)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no PR-capable pack template expands ${GH_AUTH}")
	}
}

// #20042: the ${GH_AUTH} prose about emoji says what the relay does. The relay
// never adds an emoji and corrects a wrong one.
func TestGHAuthTitleProseMatchesRelay(t *testing.T) {
	block := modeKickScheduler(t, "ISSUES_AND_PRS", 4).ghAuthInstructions()
	if !strings.Contains(block, "The hive never adds an\n  emoji") {
		t.Fatalf("${GH_AUTH} must say the hive never adds an emoji:\n%s", block)
	}
	if got := github.NormalizePRTitleEmoji("docs: explain foo"); got != "docs: explain foo" {
		t.Fatalf("premise changed: the relay now adds an emoji (%q); update ${GH_AUTH}", got)
	}
	if got := github.NormalizePRTitleEmoji("📝 proposal: x"); got != "📖 proposal: x" {
		t.Fatalf("premise changed: the relay no longer corrects a wrong emoji (%q); update ${GH_AUTH}", got)
	}
	if !strings.Contains(block, `("📝 proposal:" →`) {
		t.Errorf("${GH_AUTH} lost the corrected-emoji example:\n%s", block)
	}
}
