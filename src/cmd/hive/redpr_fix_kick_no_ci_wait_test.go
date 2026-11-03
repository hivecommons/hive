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

// `no-dco` is written on every managed repo but gates only where DCO is
// enforced (#12544). A red bot PR in a repo with no DCO check must not be told
// to force-push re-signed commits; it gets one soft note instead.
func TestRedPRFixKickDemandsResignOnlyWhenDCOGates(t *testing.T) {
	ungated := redPRFixKick(escalation.Observation{Repo: "projectbluefin/utah", Number: 987, Labels: []string{"no-dco"}, FailingChecks: []string{"build_main"}}, false)
	if strings.Contains(ungated, "FIRST (required)") || strings.Contains(ungated, "git rebase --signoff") || strings.Contains(ungated, "--force-with-lease") {
		t.Errorf("ungated no-dco kick still demands a re-sign:\n%s", ungated)
	}
	if !strings.Contains(ungated, "no DCO check is failing here") {
		t.Errorf("ungated no-dco kick lacks the soft note:\n%s", ungated)
	}
	for _, gate := range []string{"DCO", "dco"} {
		gated := redPRFixKick(escalation.Observation{Repo: "hivecommons/hive", Number: 1, Labels: []string{"no-dco"}, FailingChecks: []string{"build", gate}}, false)
		if !strings.Contains(gated, "FIRST (required)") {
			t.Errorf("gated (%s) no-dco kick lost the re-sign:\n%s", gate, gated)
		}
	}
}
