package main

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/escalation"
)

// The targeted FIX-BEFORE-NEW re-engagement kick carries no policy template,
// so it must state the no-CI-wait rule itself (#9673): a scanner spent a full
// turn in `gh run watch` after pushing the repair.
func TestRedPRFixKickForbidsCIPolling(t *testing.T) {
	got := redPRFixKick(escalation.Observation{Repo: "hivecommons/hive", Number: 9688, HeadSHA: "69773dc"}, false)
	for _, want := range []string{
		"Do NOT open a replacement PR.",
		"do not watch, poll, or sleep on CI",
		"automerge sweep merges the PR once it is green",
		"head: 69773dc",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("redPRFixKick missing %q in:\n%s", want, got)
		}
	}
}

// An adopted PR that inherited unsigned commits is labelled no-dco; its fix
// kick must name the DCO re-sign first, not only the CI failures (#14666).
func TestRedPRFixKickNamesDCORehomeFirstForNoDCO(t *testing.T) {
	got := redPRFixKick(escalation.Observation{Repo: "hivecommons/hive", Number: 14618, Labels: []string{"no-dco"}, FailingChecks: []string{"DCO"}}, false)
	first := strings.Index(got, "FIRST (required)")
	if first < 0 || !strings.Contains(got, "--exec "+redPRFixOwnSignoffExec) {
		t.Fatalf("no-dco kick lacks the DCO re-sign action:\n%s", got)
	}
	if first > strings.Index(got, "→ fix → git commit -s") {
		t.Errorf("DCO re-sign must come before the generic fix step:\n%s", got)
	}
	plain := redPRFixKick(escalation.Observation{Repo: "hivecommons/hive", Number: 1}, false)
	if strings.Contains(plain, "FIRST (required)") || strings.Contains(plain, "--signoff") {
		t.Errorf("DCO action leaked into a signed PR's kick:\n%s", plain)
	}
}

// hivecommons/hive#19102: `git rebase --signoff <base>` appended the agent's
// bot Signed-off-by to a person's commit and broke the "Sign-off survives
// the squash" check. The no-dco advice signs off only the agent's own
// commits and tells it to leave everyone else's alone.
func TestRedPRFixKickSignsOffOnlyOwnCommits(t *testing.T) {
	got := redPRFixKick(escalation.Observation{Repo: "hivecommons/hive", Number: 19102, Labels: []string{"no-dco"}}, false)
	if strings.Contains(got, "git rebase --signoff <base>") {
		t.Errorf("no-dco kick still tells the agent to rebase --signoff every commit:\n%s", got)
	}
	for _, want := range []string{"Sign off ONLY the commits you authored", "NEVER add your Signed-off-by to a commit someone else wrote", "asking the author to sign them off"} {
		if !strings.Contains(got, want) {
			t.Errorf("no-dco kick missing %q:\n%s", want, got)
		}
	}
	// The exec amends only when the commit's author email is the agent's own.
	for _, want := range []string{"git var GIT_AUTHOR_IDENT", "git log -1 --format=%ae", "git commit --amend --no-edit --signoff"} {
		if !strings.Contains(redPRFixOwnSignoffExec, want) {
			t.Errorf("own-signoff exec %q lacks %q", redPRFixOwnSignoffExec, want)
		}
	}
}
