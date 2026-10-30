package delegatedecision

import (
	"strings"
	"testing"
)

// hivecommons/hive#15722: PR #15722 (a clanker's, adopted by the hive in
// place) parked at 085 on the reviewer's requires_human / high-risk marker
// with every check green; the delegate said "make CI green" and would have
// sent it "back to its author". The hive is the author of an adopted PR:
// the finding is its own fixer's, and CI is only red when a check failed.
func TestDecidePRAdoptedByHiveRoutesToFixer(t *testing.T) {
	base := Input{Kind: KindPR, Number: 15722, Author: "mendezr", ContributorOnline: true, AdoptedByHive: true,
		Reasons: []string{"reviewer-verdict:requires_human"}, ReviewerRequiresHuman: true,
		ReviewerFinding: "the reviewer marked this change high-risk", CIGreen: true}

	d := Decide(base)
	if d.Verdict != VerdictDirection || d.Rule != RuleAdoptedFixer {
		t.Fatalf("verdict %s rule %s text %q", d.Verdict, d.Rule, d.Text)
	}
	for _, want := range []string{"hive's fixer", "the reviewer marked this change high-risk", "#15722"} {
		if !strings.Contains(d.Text, want) {
			t.Errorf("text %q lacks %q", d.Text, want)
		}
	}
	for _, text := range []string{d.Text, d.Rationale} {
		if strings.Contains(text, "CI") || strings.Contains(text, "back to") || strings.Contains(text, "@mendezr") {
			t.Errorf("an adopted PR's direction names the contributor or CI: %q", text)
		}
	}
	if len(d.Actions) != 1 || d.Actions[0].Kind != ActionCommentDecision {
		t.Fatalf("actions %+v", d.Actions)
	}

	// Checks still running are not red: the fixer is not told to make CI
	// green, only to address the finding.
	pending := with(base, func(i *Input) { i.CIGreen = false; i.CIPending = true })
	if d := Decide(pending); d.Rule != RuleAdoptedFixer || strings.Contains(d.Text, "CI") {
		t.Fatalf("pending CI: rule %s text %q", d.Rule, d.Text)
	}
	if got := mergeBlockers(pending); got != "CI still running" {
		t.Fatalf("mergeBlockers = %q", got)
	}
	// A failing check joins the fixer's list.
	red := with(base, func(i *Input) { i.CIGreen = false })
	if d := Decide(red); d.Rule != RuleAdoptedFixer || !strings.Contains(d.Text, "make CI green") {
		t.Fatalf("red CI: rule %s text %q", d.Rule, d.Text)
	}

	// The security review's finding is a person's policy call: named in
	// full, recommend-only, park kept.
	sec := with(base, func(i *Input) {
		i.SecurityFinding = true
		i.ReviewerFinding = "the security review withheld approval: token written to the log"
	})
	d = Decide(sec)
	if d.Verdict != VerdictRecommendOnly || d.Rule != RuleAdoptedSecurityFinding || !d.KeepsPark {
		t.Fatalf("security: verdict %s rule %s", d.Verdict, d.Rule)
	}
	if !strings.Contains(d.Text, "token written to the log") || !strings.Contains(d.Text, "/hive decision") {
		t.Fatalf("security text %q", d.Text)
	}

	// Adopted, green, no finding: nothing for the fixer; the ordinary
	// rules release it.
	clean := with(base, func(i *Input) { i.ReviewerRequiresHuman = false; i.ReviewerFinding = ""; i.Reasons = nil })
	if d := Decide(clean); d.Verdict != VerdictApprove {
		t.Fatalf("clean adopted head: verdict %s rule %s", d.Verdict, d.Rule)
	}

	// Not adopted: the contributor's connected clanker gets the fix list
	// as before, and pending CI is not on it.
	theirs := with(base, func(i *Input) { i.AdoptedByHive = false; i.CIGreen = false; i.CIPending = true; i.Conflicting = true })
	d = Decide(theirs)
	if d.Rule != RuleReturnToContributor || strings.Contains(d.Text, "CI") || !strings.Contains(d.Text, "rebase") {
		t.Fatalf("contributor: rule %s text %q", d.Rule, d.Text)
	}
}
