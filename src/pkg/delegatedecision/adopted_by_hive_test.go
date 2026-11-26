package delegatedecision

import (
	"strings"
	"testing"
	"time"
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

// projectbluefin/utah-packages#1309: the delegate decided "hive's fixer: ..."
// on a PR the hive adopted in place, yet its comment read "Told the author
// agent what to change" and "The author agent pushes a new commit". Once the
// relay confirmed the direction is the hive's fixer's (HiveFixerConfirmed)
// both lines name the fixer. Without that confirmation, the stalled
// follow-up and the dedup answer included, the wording is the plain
// direction's and promises no fixer; any other direction is unchanged.
func TestPlainWordsAdoptedFixerNamesTheHivesFixer(t *testing.T) {
	adopted := Decision{Verdict: VerdictDirection, Rule: RuleAdoptedFixer, Text: "hive's fixer: make CI green on #1309; then it merges", Rationale: "r"}
	words := PlainWords(adopted, CommentOptions{Kind: KindPR, HiveFixerConfirmed: true})
	for _, want := range []string{
		"**Hive decided:** Told the hive's fixer what to change.",
		"**What happens next:** The hive's fixer pushes a new commit with that change to this PR's branch; nothing is asked of the PR's author.",
	} {
		if !strings.Contains(words, want) {
			t.Errorf("confirmed adopted-fixer words lack %q:\n%s", want, words)
		}
	}
	if strings.Contains(words, "author agent") {
		t.Errorf("confirmed adopted-fixer words name the author agent:\n%s", words)
	}
	if eff := ExpectedEffectWith(adopted, CommentOptions{Kind: KindPR, HiveFixerConfirmed: true}); !eff.Waits || eff.IfNot == "" {
		t.Errorf("adopted-fixer effect must still be awaited: %+v", eff)
	}

	// Not confirmed (the relay routed to the contributor, or refused):
	// no promise of the hive's fixer, in the comment or anywhere else.
	unconfirmed := PlainWords(adopted, CommentOptions{Kind: KindPR})
	for _, want := range []string{
		"**Hive decided:** Told the author agent what to change.",
		"**What happens next:** The author agent pushes a new commit with that change.",
	} {
		if !strings.Contains(unconfirmed, want) {
			t.Errorf("unconfirmed adopted-fixer words lack %q:\n%s", want, unconfirmed)
		}
	}
	if strings.Contains(unconfirmed, "fixer pushes") || strings.Contains(unconfirmed, "hive's fixer what to change") || strings.Contains(unconfirmed, "nothing is asked") {
		t.Errorf("unconfirmed adopted-fixer words promise the hive's fixer:\n%s", unconfirmed)
	}
	if got := ExpectedEffect(adopted, KindPR).What; got != "the author agent pushes a new commit with that change" {
		t.Errorf("ExpectedEffect without confirmation = %q", got)
	}
	stalled := StalledCommentBody(adopted, KindPR, time.Date(2026, 11, 26, 1, 12, 55, 0, time.UTC), "", nil, "hive-a")
	if !strings.Contains(stalled, "Hive decided told the author agent what to change at") || strings.Contains(stalled, "fixer pushes") || strings.Contains(stalled, "nothing is asked") {
		t.Errorf("stalled follow-up promises the hive's fixer:\n%s", stalled)
	}

	plain := Decision{Verdict: VerdictDirection, Rule: "fix-then-merge", Text: "rebase #8; then it merges", Rationale: "r"}
	for _, opts := range []CommentOptions{{Kind: KindPR}, {Kind: KindPR, HiveFixerConfirmed: true}} {
		words = PlainWords(plain, opts)
		for _, want := range []string{
			"**Hive decided:** Told the author agent what to change.",
			"**What happens next:** The author agent pushes a new commit with that change.",
		} {
			if !strings.Contains(words, want) {
				t.Errorf("plain direction words lack %q:\n%s", want, words)
			}
		}
		if strings.Contains(words, "hive's fixer") {
			t.Errorf("a plain direction names the hive's fixer:\n%s", words)
		}
	}
}
