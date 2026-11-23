package stagelabels

import (
	"strings"
	"testing"
)

// The rule is what keeps an agent from pushing its own commit to a person's
// PR (hivecommons/hive#19110, #19102, #19063): it must name the check that
// fails, the one label that makes own commits safe, and both forbidden acts.
func TestPersonPRAgentCommitRuleNamesCheckLabelAndForbiddenActs(t *testing.T) {
	for _, want := range []string{
		"Sign-off survives the squash",
		MergeCommitLabel,
		"do NOT push a commit of your own",
		"NEVER add your Signed-off-by to their commits",
		"PR comment",
	} {
		if !strings.Contains(PersonPRAgentCommitRule, want) {
			t.Errorf("rule lacks %q: %s", want, PersonPRAgentCommitRule)
		}
	}
	if strings.Contains(PersonPRAgentCommitRule, "—") {
		t.Errorf("rule carries an em-dash, which the outbound filter rewrites: %s", PersonPRAgentCommitRule)
	}
}

// The exec must amend only a commit whose author email is the lane's own.
func TestOwnSignoffRebaseExecSignsOnlyOwnCommits(t *testing.T) {
	for _, want := range []string{"git var GIT_AUTHOR_IDENT", "git log -1 --format=%ae", "git commit --amend --no-edit --signoff"} {
		if !strings.Contains(OwnSignoffRebaseExec, want) {
			t.Errorf("exec %q lacks %q", OwnSignoffRebaseExec, want)
		}
	}
}
