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
	if first < 0 || !strings.Contains(got, "git rebase --signoff") {
		t.Fatalf("no-dco kick lacks the DCO re-sign action:\n%s", got)
	}
	if first > strings.Index(got, "→ fix → git commit -s") {
		t.Errorf("DCO re-sign must come before the generic fix step:\n%s", got)
	}
	plain := redPRFixKick(escalation.Observation{Repo: "hivecommons/hive", Number: 1}, false)
	if strings.Contains(plain, "git rebase --signoff") {
		t.Errorf("DCO action leaked into a signed PR's kick:\n%s", plain)
	}
}
