package main

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The targeted FIX-BEFORE-NEW re-engagement kick carries no policy template,
// so it must state the no-CI-wait rule itself (#9673): a scanner spent a full
// turn in `gh run watch` after pushing the repair.
func TestRedPRFixKickForbidsCIPolling(t *testing.T) {
	got := redPRFixKick(escalation.Observation{Repo: "hivecommons/hive", Number: 9688, HeadSHA: "69773dc"}, false, false)
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
	got := redPRFixKick(escalation.Observation{Repo: "hivecommons/hive", Number: 14618, Labels: []string{"no-dco"}, FailingChecks: []string{"DCO"}}, false, false)
	first := strings.Index(got, "FIRST (required)")
	if first < 0 || !strings.Contains(got, "--exec "+redPRFixOwnSignoffExec) {
		t.Fatalf("no-dco kick lacks the DCO re-sign action:\n%s", got)
	}
	if first > strings.Index(got, "→ fix → git commit -s") {
		t.Errorf("DCO re-sign must come before the generic fix step:\n%s", got)
	}
	plain := redPRFixKick(escalation.Observation{Repo: "hivecommons/hive", Number: 1}, false, false)
	if strings.Contains(plain, "FIRST (required)") || strings.Contains(plain, "--signoff") {
		t.Errorf("DCO action leaked into a signed PR's kick:\n%s", plain)
	}
}

// hivecommons/hive#19102: `git rebase --signoff <base>` appended the agent's
// bot Signed-off-by to a person's commit and broke the "Sign-off survives
// the squash" check. The no-dco advice signs off only the agent's own
// commits and tells it to leave everyone else's alone.
func TestRedPRFixKickSignsOffOnlyOwnCommits(t *testing.T) {
	got := redPRFixKick(escalation.Observation{Repo: "hivecommons/hive", Number: 19102, Labels: []string{"no-dco"}}, false, false)
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

// hivecommons/hive#19110: the merge-request re-engagement kick reached a
// person's red PR and the lane pushed its own coverage fix, which the squash
// would land under the person's name with the lane's sign-off. The kick
// carries the person's-PR rule whatever the labels say.
func TestRedPRFixKickCarriesPersonPRRule(t *testing.T) {
	for _, o := range []escalation.Observation{
		{Repo: "hivecommons/hive", Number: 19110},
		{Repo: "hivecommons/hive", Number: 19110, Labels: []string{"no-dco"}},
	} {
		got := redPRFixKick(o, false, false)
		if !strings.Contains(got, "author is a person") || !strings.Contains(got, stagelabels.PersonPRAgentCommitRule) {
			t.Errorf("kick for %v lacks the person's-PR rule:\n%s", o.Labels, got)
		}
	}
}

// hivecommons/hive#20044: at ACMM L3-L5 every agent PR carries the
// ready-for-review gate label, so green CI does not merge it. The repair kick
// must say who merges it instead of promising the sweep will.
func TestRedPRFixKickHeldPRDoesNotPromiseMergeOnGreen(t *testing.T) {
	o := escalation.Observation{Repo: "hivecommons/hive", Number: 20044}
	for _, level := range []int{acmmHoldGatedMinLevel, 4, acmmHoldGatedMaxLevel} {
		held := shouldHoldAgentPR("scanner", o.Repo, level, nil, nil)
		got := redPRFixKick(o, false, held)
		if strings.Contains(got, "automerge sweep merges the PR once it is green") {
			t.Errorf("L%d: held repair kick still promises a merge on green:\n%s", level, got)
		}
		if !strings.Contains(got, "`"+github.ReadyForReviewLabelName()+"`") || !strings.Contains(got, "removes that label") {
			t.Errorf("L%d: held repair kick must name the gate label and who lifts it:\n%s", level, got)
		}
		if !strings.Contains(got, "do not watch, poll, or sleep on CI") {
			t.Errorf("L%d: the no-poll rule must stay:\n%s", level, got)
		}
	}
	if !shouldHoldAgentPR("scanner", o.Repo, 4, nil, nil) || shouldHoldAgentPR("scanner", o.Repo, 6, nil, nil) {
		t.Fatal("premise changed: the ACMM gate no longer holds L4 and frees L6; update the kick text with it")
	}
	if got := redPRFixKick(o, false, shouldHoldAgentPR("scanner", o.Repo, 6, nil, nil)); !strings.Contains(got, "automerge sweep merges the PR once it is green") {
		t.Errorf("L6 repair kick keeps the merge-on-green sentence:\n%s", got)
	}
}
