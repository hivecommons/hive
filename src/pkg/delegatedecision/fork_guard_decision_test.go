package delegatedecision

import (
	"strings"
	"testing"
)

// A fork PR parked for a fork guard (stage reason `fork-guard:<checks>`,
// hivecommons/hive#14273): its only red check refuses the PR because the head
// is in a fork. Neither the contributor nor an in-place adoption can turn that
// check green (both push to the same fork branch), so a delegated decision
// must not hand anyone "make CI green" or unpark the PR to do it. Before the
// fix, an adopted-in-place PR parked this way was decided "hive's fixer: make
// CI green without widening scope on #N; then it merges" and went straight
// back to 085.
func TestDecideForkGuardParkDoesNotAskForGreenCI(t *testing.T) {
	const refused = "the hive recorded that it cannot finish this fork PR on the fork's own branch (fork-rehome-refused)"
	base := Input{Kind: KindPR, Repo: "acme/widget", Number: 42, Author: "contributor", ContributorOnline: true,
		AdoptedByHive: true, Reasons: []string{"fork-guard:publish"}, RedChecks: []string{"publish"}}
	for _, tc := range []struct {
		name string
		in   Input
	}{
		{name: "adopted in place", in: base},
		{name: "adopted, push refused, author online", in: with(base, func(i *Input) { i.AdoptedPushRefused = refused })},
		{name: "adopted, push refused, author offline", in: with(base, func(i *Input) {
			i.AdoptedPushRefused = refused
			i.ContributorOnline = false
		})},
		{name: "not adopted, author online", in: with(base, func(i *Input) { i.AdoptedByHive = false })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Run(tc.in)
			if strings.Contains(d.Text, "make CI green") {
				t.Errorf("a fork-guard park was told to make CI green: rule %s verdict %s text %q", d.Rule, d.Verdict, d.Text)
			}
			if d.Verdict == VerdictDirection {
				t.Errorf("a fork-guard park was unparked to a fixer or author who cannot turn the guarded check green: rule %s text %q", d.Rule, d.Text)
			}
		})
	}
}

// The fork guard is a maintainer's step wherever the decision lands. With
// nothing else owed the hive recommends it and keeps the park; with other
// items owed they go to their owner as before and the direction names the
// guarded check as the maintainer's step after them. A head with no guard,
// or whose guarded check is green now, reads exactly as it did.
func TestDecideForkGuardMaintainerStep(t *testing.T) {
	const refused = "the hive recorded that it cannot finish this fork PR on the fork's own branch (fork-rehome-refused)"
	const step = "then a maintainer acts on the fork-guarded check `publish` as its own message asks, and it merges"
	base := Input{Kind: KindPR, Repo: "acme/widget", Number: 42, Author: "contributor", HeadSHA: "abc1234def567",
		ForkGuardChecks: []string{"publish"}, RedChecks: []string{"publish"}}
	for _, tc := range []struct {
		name      string
		in        Input
		rule      string
		verdict   Verdict
		keepsPark bool
		want      []string
		not       []string
	}{
		{
			name: "adopted in place, only the guard is red", in: with(base, func(i *Input) { i.AdoptedByHive = true }),
			rule: RuleForkGuard, verdict: VerdictRecommendOnly, keepsPark: true,
			want: []string{"hive recommends: review the change on #42, then act on the fork-guarded check `publish` as its own message asks (run or admit it for head `abc1234`, or push the branch to the base repository), or close #42; owner to confirm"},
			not:  []string{"make CI green", "hive's fixer"},
		},
		{
			name: "adopted in place, guard plus a reviewer finding: the fixer, then the maintainer",
			in: with(base, func(i *Input) {
				i.AdoptedByHive = true
				i.ReviewerRequiresHuman = true
				i.ReviewerFinding = "the reviewer marked this change high-risk"
			}),
			rule: RuleAdoptedFixer, verdict: VerdictDirection,
			want: []string{"hive's fixer: address the reviewer's finding (the reviewer marked this change high-risk) on #42; " + step},
			not:  []string{"make CI green"},
		},
		{
			name: "push refused, author online, guard plus a review finding",
			in: with(base, func(i *Input) {
				i.AdoptedByHive = true
				i.AdoptedPushRefused = refused
				i.ContributorOnline = true
				i.HeadReviewVerdict = "changes_requested"
			}),
			rule: RuleReturnToContributor, verdict: VerdictDirection,
			want: []string{"address the changes the hive's review requested on this head on #42; " + step},
			not:  []string{"make CI green"},
		},
		{
			name: "push refused, author offline, guard only",
			in: with(base, func(i *Input) {
				i.AdoptedByHive = true
				i.AdoptedPushRefused = refused
			}),
			rule: RuleForkGuard, verdict: VerdictRecommendOnly, keepsPark: true,
			want: []string{"the fork-guarded check `publish`"},
			not:  []string{"make CI green", "pushes the fix"},
		},
		{
			name: "push refused, author offline, guard plus a conflict",
			in: with(base, func(i *Input) {
				i.AdoptedByHive = true
				i.AdoptedPushRefused = refused
				i.Conflicting = true
			}),
			rule: RuleAdoptedPushRefused, verdict: VerdictRecommendOnly, keepsPark: true,
			want: []string{"a maintainer pushes the fix to #42 (rebase onto the base branch), then acts on the fork-guarded check `publish` as its own message asks, or closes it"},
			not:  []string{"make CI green"},
		},
		{
			name: "not adopted, author online, guard only",
			in:   with(base, func(i *Input) { i.ContributorOnline = true }),
			rule: RuleForkGuard, verdict: VerdictRecommendOnly, keepsPark: true,
			want: []string{"`publish`"},
			not:  []string{"make CI green"},
		},
		{
			name: "author offline, guard only: not adopted over it",
			in:   base,
			rule: RuleForkGuard, verdict: VerdictRecommendOnly, keepsPark: true,
			want: []string{"`publish`"},
			not:  []string{"make CI green", "supersede", "adoption/supersede"},
		},
		{
			name: "guard beside another red check: that one is owed, the guard after it",
			in: with(base, func(i *Input) {
				i.ContributorOnline = true
				i.ForkGuardChecks = nil
				i.Reasons = []string{"fork-guard:publish"}
				i.RedChecks = []string{"lint", "publish"}
			}),
			rule: RuleReturnToContributor, verdict: VerdictDirection,
			want: []string{"the author is connected: make CI green without widening scope (`lint`) on #42; " + step},
		},
		{
			name: "guard plus missing sign-off, author online: fix then merge",
			in: with(base, func(i *Input) {
				i.ContributorOnline = true
				i.DCOMissing = true
			}),
			rule: "fix-then-merge", verdict: VerdictDirection,
			want: []string{"add the DCO sign-off; keep the smallest reversible change, " + step},
			not:  []string{"make CI green"},
		},
		{
			name: "two guarded checks",
			in: with(base, func(i *Input) {
				i.ForkGuardChecks = []string{"publish", "sign"}
				i.RedChecks = []string{"publish", "sign"}
			}),
			rule: RuleForkGuard, verdict: VerdictRecommendOnly, keepsPark: true,
			want: []string{"the fork-guarded checks `publish`, `sign` as their own messages ask (run or admit them for head `abc1234`"},
		},
		{
			name: "fork PR with a red check that is not guarded",
			in: with(base, func(i *Input) {
				i.ContributorOnline = true
				i.ForkGuardChecks = nil
				i.RedChecks = []string{"lint"}
			}),
			rule: RuleReturnToContributor, verdict: VerdictDirection,
			want: []string{"the author is connected: make CI green without widening scope on #42; then it merges"},
		},
		{
			name: "recorded guard now green, another check red",
			in: with(base, func(i *Input) {
				i.ContributorOnline = true
				i.ForkGuardChecks = nil
				i.Reasons = []string{"fork-guard:publish"}
				i.RedChecks = []string{"lint"}
			}),
			rule: RuleReturnToContributor, verdict: VerdictDirection,
			want: []string{"the author is connected: make CI green without widening scope on #42; then it merges"},
			not:  []string{"fork-guarded"},
		},
		{
			name: "same-repo PR, CI red",
			in: Input{Kind: KindPR, Repo: "acme/widget", Number: 42, Author: "contributor", ContributorOnline: true,
				RedChecks: []string{"publish"}},
			rule: RuleReturnToContributor, verdict: VerdictDirection,
			want: []string{"the author is connected: make CI green without widening scope on #42; then it merges"},
		},
		{
			name: "a guard reason, but no check named red on this head",
			in: with(base, func(i *Input) {
				i.ContributorOnline = true
				i.ForkGuardChecks = nil
				i.Reasons = []string{"fork-guard:publish"}
				i.RedChecks = nil
			}),
			rule: RuleReturnToContributor, verdict: VerdictDirection,
			want: []string{"the author is connected: make CI green without widening scope on #42; then it merges"},
		},
		{
			name: "author offline, guard plus the hive review's change request: adopted, as the engine adopts it",
			in:   with(base, func(i *Input) { i.HeadReviewVerdict = "changes_requested" }),
			rule: RuleContributorOffline, verdict: VerdictAdoptSupersede,
			want: []string{"supersede now"},
		},
		{
			name: "author offline, guard only, grace elapsed: still the maintainer's step",
			in:   with(base, func(i *Input) { i.AdoptionGraceElapsed = true }),
			rule: RuleForkGuard, verdict: VerdictRecommendOnly, keepsPark: true,
		},
		{
			name: "author offline, guard only, a person's PR covers the issue: closed as a duplicate",
			in:   with(base, func(i *Input) { i.Competing = []CompetingPR{{Number: 50, PersonAuthored: true}} }),
			rule: RuleCoveredByOtherPR, verdict: VerdictCloseSuperseded,
		},
		{
			name: "author offline, guard only, the issue is claimed: left open for the claim",
			in: with(base, func(i *Input) {
				i.IssueClaimHolder = "a maintainer"
				i.IssueClaimIssue = 39
			}),
			rule: RuleCoveredByOtherPR, verdict: VerdictRecommendOnly, keepsPark: true,
			want: []string{"leave #42 open"},
		},
		{
			name: "adopted in place, guard only, a person's PR covers the issue: stays parked with the maintainer",
			in: with(base, func(i *Input) {
				i.AdoptedByHive = true
				i.Competing = []CompetingPR{{Number: 50, PersonAuthored: true}}
			}),
			rule: RuleForkGuard, verdict: VerdictRecommendOnly, keepsPark: true,
			want: []string{"`publish`"},
		},
		{
			name: "reporter-trust hold, claimed issue, guard only: never approved",
			in: with(base, func(i *Input) {
				i.ReporterTrustHold = true
				i.IssueClaimHolder = "x"
			}),
			rule: RuleForkGuard, verdict: VerdictRecommendOnly, keepsPark: true,
		},
		{
			name: "guard only, a merger with no PR number: not approved",
			in:   with(base, func(i *Input) { i.SupersededBy = &MergedCloser{} }),
			rule: RuleForkGuard, verdict: VerdictRecommendOnly, keepsPark: true,
		},
		{
			name: "guarded PR a merged PR already delivered: closed as superseded",
			in: with(base, func(i *Input) {
				i.SupersededBy = &MergedCloser{PR: 40, Issue: 39}
			}),
			rule: RuleAlreadyFixed, verdict: VerdictCloseSuperseded,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Run(tc.in)
			if d.Rule != tc.rule || d.Verdict != tc.verdict || d.KeepsPark != tc.keepsPark {
				t.Fatalf("rule %s verdict %s keepsPark %v, want %s %s %v: %q", d.Rule, d.Verdict, d.KeepsPark, tc.rule, tc.verdict, tc.keepsPark, d.Text)
			}
			for _, w := range tc.want {
				if !strings.Contains(d.Text, w) {
					t.Errorf("text %q does not contain %q", d.Text, w)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(d.Text, n) {
					t.Errorf("text %q contains %q", d.Text, n)
				}
			}
			for _, bad := range []string{"—", "–", "@"} {
				if strings.Contains(d.Text, bad) {
					t.Errorf("decision text carries %q: %q", bad, d.Text)
				}
			}
		})
	}
}

// AdoptsImmediately agrees with Decide on a head red only on a fork guard:
// the adoption engine does not adopt such a head on its own (the guard asks
// a maintainer to publish the branch, adoptionForkGuardOnly), and Decide
// recommends the maintainer's step, even once the offline grace elapsed. A
// guard beside another red check or a review ask is adopted for an offline
// author as before.
func TestForkGuardOnlyIsNotAdoptedImmediately(t *testing.T) {
	in := Input{Kind: KindPR, Repo: "acme/widget", Number: 42, Reasons: []string{"fork-guard:publish"}, RedChecks: []string{"publish"}}
	for name, guardOnly := range map[string]Input{
		"guard only":                   in,
		"guard only, grace elapsed":    with(in, func(i *Input) { i.AdoptionGraceElapsed = true }),
		"guard only, adoption stalled": with(in, func(i *Input) { i.AdoptionStalled = true }),
	} {
		if AdoptsImmediately(guardOnly) {
			t.Errorf("%s: adopted in supersede mode at once", name)
		}
		if d := Run(guardOnly); d.Rule != RuleForkGuard || d.Verdict == VerdictAdoptSupersede {
			t.Errorf("%s: Decide answered rule %s verdict %s", name, d.Rule, d.Verdict)
		}
	}
	for name, owed := range map[string]Input{
		"another red check": with(in, func(i *Input) { i.RedChecks = []string{"lint", "publish"} }),
		"a review ask":      with(in, func(i *Input) { i.HeadReviewVerdict = "changes_requested" }),
	} {
		if !AdoptsImmediately(owed) || Run(owed).Verdict != VerdictAdoptSupersede {
			t.Errorf("%s beside the guard: not adopted for an offline author", name)
		}
	}
}

// A person's fork PR red only on a guard, with a reviewer question for
// them: the question still goes to the author, and the guard is named as
// the maintainer's step after it.
func TestForkGuardPersonQuestionReturnsToAuthor(t *testing.T) {
	in := Input{Kind: KindPR, Repo: "acme/widget", Number: 42, Author: "contributor", HumanAuthor: true,
		Question: "is the retry bounded?", ForkGuardChecks: []string{"publish"}, RedChecks: []string{"publish"}}
	d := Run(in)
	want := "@contributor, the reviewer asked: is the retry bounded? Then a maintainer acts on the fork-guarded check `publish` as its own message asks, and it merges"
	if d.Rule != RuleReturnToAuthor || d.Text != want {
		t.Fatalf("rule %s text %q, want %s %q", d.Rule, d.Text, RuleReturnToAuthor, want)
	}
	d = Run(with(in, func(i *Input) { i.Question = "check the retry bound" }))
	want = "@contributor, the reviewer asked: check the retry bound; then a maintainer acts on the fork-guarded check `publish` as its own message asks, and it merges"
	if d.Rule != RuleReturnToAuthor || d.Text != want {
		t.Fatalf("question without end punctuation: rule %s text %q, want %q", d.Rule, d.Text, want)
	}
	if d := Run(with(in, func(i *Input) { i.Question = "" })); d.Rule != RuleForkGuard || strings.Contains(d.Text, "supersede") {
		t.Fatalf("without a question: rule %s text %q", d.Rule, d.Text)
	}
}

// What hive saw: the CI fact names the fork-guarded check as a maintainer's
// step rather than a plain "not green".
func TestFactsForkGuard(t *testing.T) {
	ci := func(in Input) string {
		for _, f := range in.Facts() {
			if f.Label == "CI" {
				return f.Value
			}
		}
		return ""
	}
	in := Input{Kind: KindPR, ForkGuardChecks: []string{"publish"}, RedChecks: []string{"publish"}}
	if got, want := ci(in), "red only on fork-guarded `publish`: a maintainer's step"; got != want {
		t.Errorf("guard only: CI fact %q, want %q", got, want)
	}
	in = Input{Kind: KindPR, Reasons: []string{"fork-guard:publish"}, RedChecks: []string{"lint", "publish"}}
	if got, want := ci(in), "not green: `lint` red, and fork-guarded `publish` is a maintainer's step"; got != want {
		t.Errorf("guard beside lint: CI fact %q, want %q", got, want)
	}
	in = Input{Kind: KindPR, ForkGuardChecks: []string{"publish", "sign"}, RedChecks: []string{"lint", "publish", "sign"}}
	if got, want := ci(in), "not green: `lint` red, and fork-guarded `publish`, `sign` are a maintainer's step"; got != want {
		t.Errorf("two guards beside lint: CI fact %q, want %q", got, want)
	}
	if got := ci(Input{Kind: KindPR, RedChecks: []string{"lint"}}); got != "not green" {
		t.Errorf("no guard: CI fact %q", got)
	}
}

// A `fork-guard:` reason with no check names is no guard.
func TestForkGuardFromReasons(t *testing.T) {
	for reason, want := range map[string]string{
		"fork-guard:publish":           "publish",
		" fork-guard: publish, sign ":  "publish,sign",
		"fork-guard:":                  "",
		"adoption-stalled:conflicting": "",
	} {
		if got := strings.Join(forkGuardFromReasons([]string{reason}), ","); got != want {
			t.Errorf("forkGuardFromReasons(%q) = %q, want %q", reason, got, want)
		}
	}
}
