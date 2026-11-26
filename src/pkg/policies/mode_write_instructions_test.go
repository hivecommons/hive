package policies

import (
	"regexp"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/agentmode"
	"github.com/hivecommons/hive/pkg/config"
)

// Default policies must not ask an agent for actions its mode, its token or
// the relays refuse (hivecommons/hive#20036). The proxy, the gh wrapper and
// the relays already refuse the writes; these tests keep the prompt text in
// step with them, so an agent does not spend turns on a refused call and then
// report its work as blocked.

// packTemplate is a default template a shipped ACMM pack names, with the
// lowest mode and the levels any pack runs it at.
type packTemplate struct {
	mode     agentmode.AgentMode
	converse bool
	levels   []int
}

func packTemplates(t *testing.T) map[string]*packTemplate {
	t.Helper()
	packs := config.ACMMPacks()
	if len(packs) == 0 {
		t.Fatal("no ACMM packs loaded")
	}
	out := map[string]*packTemplate{}
	for _, pack := range packs {
		for _, a := range pack.Agents {
			if a.KickTemplate == "" || a.Mode == "" {
				continue
			}
			mode, ok := agentmode.ParseAgentMode(a.Mode)
			if !ok {
				t.Fatalf("L%d %s: unparseable mode %q", pack.Level, a.Name, a.Mode)
			}
			pt, seen := out[a.KickTemplate]
			if !seen {
				pt = &packTemplate{mode: mode, converse: a.Converse != nil && *a.Converse}
				out[a.KickTemplate] = pt
			}
			if mode < pt.mode {
				pt.mode = mode
			}
			pt.levels = append(pt.levels, pack.Level)
		}
	}
	return out
}

// writeNeedsPush names text that only an ISSUES_AND_PRS or higher agent can
// follow: the push relay and the PR relay refuse it below that mode
// (crux/agent/manager_modes.go CanPush).
var writeNeedsPush = []string{
	"hive-open-pr", "gh pr create", "git push", "hive-push-branch", "gh pr close",
	"## After You Push", "Pushed the branch", "fix and push", "fix it and push",
	"propose, one PR each",
}

// writeNeedsIssues names text that only an ISSUES_ONLY or higher agent can
// follow: the issue-request relay refuses issue, comment, claim, close and
// label requests below that mode (CanCreateIssues).
var writeNeedsIssues = []string{
	"hive-open-issue", "gh issue create", "relay's `label` command",
	"--needs-decision", "--not-agent-work", "--blocked-on-pr",
	"file ONE issue", "File ONE issue", "file a `", "Open one `[",
}

// writeNeedsComment names comment writes. Below ISSUES_ONLY only an agent with
// the `converse` capability may make them (crux/proxy/rules.go).
var writeNeedsComment = []string{
	"gh issue comment", "gh pr comment", "relay's `comment` command",
}

// negatedWrite matches a sentence that forbids the write rather than asking
// for it ("Do not use `gh pr comment`", "NEVER attempt out-of-tier writes: no
// `gh run rerun`"). Those are correct text.
var negatedWrite = regexp.MustCompile(`(?i)\b(do not|don't|never|cannot|can not|must not|no ` + "`" + `|refuse[sd]?|not allowed)\b`)

var sentenceBreak = regexp.MustCompile(`(?:[.;:!?])\s+`)

// outOfModeWrites returns each sentence of text that asks for one of the
// write patterns.
func outOfModeWrites(text string, patterns []string) []string {
	var hits []string
	for _, line := range strings.Split(text, "\n") {
		for _, sentence := range sentenceBreak.Split(line, -1) {
			if negatedWrite.MatchString(sentence) {
				continue
			}
			for _, p := range patterns {
				if strings.Contains(sentence, p) {
					hits = append(hits, "["+p+"] "+strings.TrimSpace(sentence))
				}
			}
		}
	}
	return hits
}

// patternsAboveMode is every write pattern the mode cannot follow.
func patternsAboveMode(mode agentmode.AgentMode, converse bool) []string {
	var out []string
	if !mode.CanPush() {
		out = append(out, writeNeedsPush...)
	}
	if !mode.CanCreateIssues() {
		out = append(out, writeNeedsIssues...)
		if !converse {
			out = append(out, writeNeedsComment...)
		}
	}
	return out
}

// TestPackTemplatesAskOnlyForWritesTheirModeAllows (#20039): every default
// template a pack runs below ISSUES_AND_PRS is scanned for write instructions
// above the pack's mode for it.
func TestPackTemplatesAskOnlyForWritesTheirModeAllows(t *testing.T) {
	for name, pt := range packTemplates(t) {
		patterns := patternsAboveMode(pt.mode, pt.converse)
		if len(patterns) == 0 {
			continue
		}
		t.Run(name, func(t *testing.T) {
			source, embedded, ok := readPolicyPair(t, name)
			if !ok {
				return
			}
			for variant, text := range map[string]string{"source": source, "embedded": embedded} {
				for _, hit := range outOfModeWrites(text, patterns) {
					t.Errorf("%s %s runs at %s (levels %v) but asks for a write that mode cannot make: %s", variant, name, pt.mode, pt.levels, hit)
				}
			}
		})
	}
}

// The scan itself must catch the blocks it exists to keep out, and must let
// negated mentions through.
func TestOutOfModeWritesScan(t *testing.T) {
	advisory := patternsAboveMode(agentmode.ModeAdvisory, false)
	for _, text := range []string{
		"## After You Push: Do Not Wait for CI (MANDATORY)",
		"Escalate through `hive-open-issue --needs-decision`.",
		"Post one note via the issue relay's `comment` command.",
	} {
		if len(outOfModeWrites(text, advisory)) == 0 {
			t.Errorf("scan missed an ADVISORY write in %q", text)
		}
	}
	for _, text := range []string{
		"Do not use `gh pr review`, `gh pr comment` or `hive-open-pr`.",
		"⛔ NEVER attempt out-of-tier writes: no `gh run rerun`.",
	} {
		if hits := outOfModeWrites(text, advisory); len(hits) != 0 {
			t.Errorf("scan flagged a negated mention %q: %v", text, hits)
		}
	}
	if hits := outOfModeWrites("Post one note via the issue relay's `comment` command.", patternsAboveMode(agentmode.ModeAdvisory, true)); len(hits) != 0 {
		t.Errorf("a converse agent may comment: %v", hits)
	}
	if patternsAboveMode(agentmode.ModeIssuesAndPRs, false) != nil {
		t.Error("ISSUES_AND_PRS can make every write the scan names")
	}
}

// TestDefaultPoliciesDoNotAskForCIRerun (#20040): no agent can re-run CI. The
// gh wrapper allows only `gh run list|view`, the proxy has no rule for any
// Actions write, and no token tier asks for Actions write. The policies must
// send a re-run to a maintainer or the [shared-ci] sweep instead.
func TestDefaultPoliciesDoNotAskForCIRerun(t *testing.T) {
	banned := []string{
		"Use `gh run rerun",
		"Retrigger failed jobs with `gh run rerun",
		"explicitly configured `workflow_dispatch`",
		"re-run the check on the default branch",
		"Re-run or check recent runs",
		"HIVE_CI_RETRIGGER_COOLDOWN_MINUTES",
	}
	for _, name := range policyTemplateNames(t) {
		source, embedded, ok := readPolicyPair(t, name)
		if !ok {
			continue
		}
		for _, b := range banned {
			if strings.Contains(source, b) || strings.Contains(embedded, b) {
				t.Errorf("%s asks for a CI re-run agents cannot make: %q", name, b)
			}
		}
	}
	for _, name := range []string{"ci-maintainer-holdgated.md", "ci-maintainer-full.md", "sec-check-holdgated.md", "sec-check-full.md"} {
		source, embedded, ok := readPolicyPair(t, name)
		if !ok {
			continue
		}
		for _, want := range []string{"## CI Retrigger Integrity", "Never push to retrigger CI", "You cannot re-run CI"} {
			if !strings.Contains(source, want) || !strings.Contains(embedded, want) {
				t.Errorf("%s lost %q", name, want)
			}
		}
	}
}

// TestIssuesAndPRsTemplatesDoNotAskForWorkflowPRs (#20041): at ISSUES_AND_PRS
// the scheduler adds "Workflow files are out of reach at this mode" (the
// contributor token cannot push .github/workflows/**), so a template that runs
// at that mode must not also ask for a workflow-file PR.
func TestIssuesAndPRsTemplatesDoNotAskForWorkflowPRs(t *testing.T) {
	banned := []string{
		"workflow changes, dependency updates",
		"Implement the CI workflow fix",
		"Write the test code or CI workflow changes",
		"(new tests, test fixtures, CI workflow)",
		"CI workflow improvements",
		"`.github/workflows/*.yml` changes",
		"propose, one PR each",
	}
	checked := 0
	for _, pack := range config.ACMMPacks() {
		for _, a := range pack.Agents {
			if a.Mode != agentmode.ModeIssuesAndPRs.String() || a.KickTemplate == "" {
				continue
			}
			source, embedded, ok := readPolicyPair(t, a.KickTemplate)
			if !ok {
				continue
			}
			checked++
			for _, b := range banned {
				if strings.Contains(source, b) || strings.Contains(embedded, b) {
					t.Errorf("L%d %s (%s) asks for a workflow-file PR its mode cannot push: %q", pack.Level, a.Name, a.KickTemplate, b)
				}
			}
			if strings.HasPrefix(a.KickTemplate, "ci-maintainer") || strings.HasPrefix(a.KickTemplate, "quality") {
				// The pointer is worded in lower case on purpose: the scheduler's
				// tests key on the section's own "Workflow files are out of reach"
				// heading being absent below ISSUES_AND_PRS.
				if !strings.Contains(embedded, "workflow files are out of reach at this mode") && !strings.Contains(embedded, "can NOT PR `.github/workflows") {
					t.Errorf("L%d %s (%s) does not point a workflow-file fix at the push-ceiling rule", pack.Level, a.Name, a.KickTemplate)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no ISSUES_AND_PRS pack templates found")
	}
}

// TestNeedsHumanAddIsNotForbidden (#20043): the label relay lets an agent add
// `needs-human` and refuses only its removal (pkg/github issue_request_label.go
// agentMayAddLabel). A template whose escalation block says to add it must not
// also list it as a forbidden hive-controlled label.
func TestNeedsHumanAddIsNotForbidden(t *testing.T) {
	for _, name := range policyTemplateNames(t) {
		source, embedded, ok := readPolicyPair(t, name)
		if !ok {
			continue
		}
		for variant, text := range map[string]string{"source": source, "embedded": embedded} {
			if strings.Contains(text, "no hive-controlled labels (`needs-human`") {
				t.Errorf("%s %s forbids adding `needs-human`, which the label relay allows", variant, name)
			}
		}
	}
}

// TestHoldGatedTemplatesDoNotPromiseMergeOnGreen (#20044): at ACMM L3-L5 every
// agent PR carries ready-for-review, which blocks the merge until a maintainer
// removes it. No template a pack runs at those levels may say green CI merges it.
func TestHoldGatedTemplatesDoNotPromiseMergeOnGreen(t *testing.T) {
	const promise = "merges your PR the moment its"
	for name, pt := range packTemplates(t) {
		held := false
		for _, l := range pt.levels {
			if l >= 3 && l <= 5 {
				held = true
			}
		}
		if !held {
			continue
		}
		source, embedded, ok := readPolicyPair(t, name)
		if !ok {
			continue
		}
		if strings.Contains(source, promise) || strings.Contains(embedded, promise) {
			t.Errorf("%s runs at levels %v but promises the sweep merges the PR on green", name, pt.levels)
		}
	}
}

// TestPRTitleRuleStatedOnce (#20042): ${GH_AUTH} carries the PR-title rule.
// A template that expands it must not restate a second, different rule.
func TestPRTitleRuleStatedOnce(t *testing.T) {
	for _, name := range policyTemplateNames(t) {
		_, embedded, ok := readPolicyPair(t, name)
		if !ok || !strings.Contains(embedded, "${GH_AUTH}") {
			continue
		}
		if strings.Contains(embedded, "Title the PR the way the TARGET repository titles PRs") {
			t.Errorf("%s expands ${GH_AUTH} and restates the PR-title rule; defer to Project Authentication instead", name)
		}
	}
}
