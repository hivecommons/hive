package automerge

import (
	"context"
	"strings"
	"testing"
)

// projectbluefin/bluefin-bling#200 and #182 (2026-11-22..23): the sign-off
// attribution hold told a maintainer to merge with `gh pr merge N --merge`
// in a repo that allows only squash (auto_merge.squash_only, GitHub merge
// commits off, ruleset and merge queue SQUASH only). Only the squash-only
// refusal named the policy; every other refusal offered the merge commit.

const squashOnlyTestRule = "the PR touches a human-merge path (auto_merge.human_merge_paths)"

// Repos that allow merge commits keep today's text, byte for byte, and the
// squash-only refusal keeps its own text whether or not Detail.SquashOnly
// is set.
func TestMergeCommitRefusalTextMergeCommitRepoUnchanged(t *testing.T) {
	const (
		approveRole = "The hive selects a merge commit for this shape by itself only on a `role/trusted` PR or a head a trusted maintainer has approved; this one is neither yet. " +
			"**Ask (maintainer):** every commit already carries a sign-off matching its own author, so DCO holds without rewriting history: approve the current head with a GitHub **Approve** review (the hive then selects the merge commit itself), or merge it by hand with a merge commit (`gh pr merge 991 --merge`, not squash)."
		ruleRole = "The hive will not merge this head itself, approved or not: " + squashOnlyTestRule + ". " +
			"**Ask (maintainer):** every commit already carries a sign-off matching its own author, so DCO holds without rewriting history: review the change and merge it by hand with a merge commit (`gh pr merge 991 --merge`, not squash); an approving review would not land it."
		hiveOnly = "Every non-merge commit on the head is the hive's, so there is no contributor authorship for a merge commit to preserve. **Ask (maintainer):** merge with a merge commit (`gh pr merge 991 --merge`, not squash), or close this PR if the hive's change is not wanted."
		unsigned = "Some commits on the head carry no sign-off at all, so the hive did not select a merge commit. **Ask (maintainer):** decide whether those commits need a sign-off before merging; if not, merge with a merge commit (`gh pr merge 991 --merge`, not squash)."
		labels   = "Every commit already carries a sign-off matching its own author, so the hive elected a merge commit, but the write-back gate keeps the hive's labels off this PR, so `merge-method/merge` could not be applied and the attribution check still applies the squash rule. "
		labelsNo = labels + "**Ask (maintainer):** add `merge-method/merge` (the check re-runs under the merge rule and the hive merges with a merge commit), or merge by hand with `gh pr merge 991 --merge`, not squash."
		labelsRu = labels + "**Ask (maintainer):** add `merge-method/merge` so the attribution check uses the merge rule, then merge by hand with `gh pr merge 991 --merge`, not squash: " + squashOnlyTestRule + ". An approving review would not land it."
		def      = "**Ask (maintainer):** every commit already carries a sign-off matching its own author, so a maintainer can merge this PR with a merge commit (`gh pr merge 991 --merge`, not squash) and DCO holds without rewriting history."
		squash   = "Every commit already carries a sign-off matching its own author, so a merge commit would land them without rewriting history, but `acme/widget` is squash-only (`auto_merge.squash_only`), so the hive did not select one. "
		squashNo = squash + "**Ask (maintainer):** either lift the squash-only policy for this repo (the hive then merges with a merge commit on its next sweep), or re-author the hive's commits under a maintainer's own sign-off and push a new head."
		squashRu = squash + "**Ask (maintainer):** either lift the squash-only policy for this repo (a maintainer still merges it by hand with a merge commit: " + squashOnlyTestRule + "), or re-author the hive's commits under a maintainer's own sign-off and push a new head."
		sentinel = " A sentinel alert is open on this PR: resolve it and have a maintainer clear its label first."
	)
	for _, tc := range []struct {
		name, refused, rule, want string
		squashOnly                bool
	}{
		{name: "role", refused: mergeCommitRefusedRole, want: approveRole},
		{name: "role with a rule", refused: mergeCommitRefusedRole, rule: squashOnlyTestRule, want: ruleRole},
		{name: "hive only", refused: mergeCommitRefusedHiveOnly, want: hiveOnly},
		{name: "unsigned", refused: mergeCommitRefusedUnsigned, want: unsigned},
		{name: "labels suppressed", refused: mergeCommitRefusedLabelsSuppressed, want: labelsNo},
		{name: "labels suppressed with a rule", refused: mergeCommitRefusedLabelsSuppressed, rule: squashOnlyTestRule, want: labelsRu},
		{name: "default", want: def},
		{name: "squash-only refusal", refused: mergeCommitRefusedSquashOnly, want: squashNo},
		{name: "squash-only refusal with a rule", refused: mergeCommitRefusedSquashOnly, rule: squashOnlyTestRule, want: squashRu},
		{name: "squash-only refusal, squash-only fact set", refused: mergeCommitRefusedSquashOnly, squashOnly: true, want: squashNo},
		{name: "squash-only refusal with a rule, squash-only fact set", refused: mergeCommitRefusedSquashOnly, rule: squashOnlyTestRule, squashOnly: true, want: squashRu},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := stageObservation{Number: 991, Display: "acme/widget", Detail: stageDetail{MergeCommitRefused: tc.refused, ManualMergeRule: tc.rule, SquashOnly: tc.squashOnly}}
			if got := mergeCommitRefusalText(obs); got != tc.want {
				t.Fatalf("text changed:\n got %q\nwant %q", got, tc.want)
			}
			obs.Detail.SentinelAlert = true
			if got := mergeCommitRefusalText(obs); got != tc.want+sentinel {
				t.Fatalf("sentinel text changed:\n got %q\nwant %q", got, tc.want+sentinel)
			}
		})
	}
}

// In a squash-only repo every other refusal keeps its own context and
// question, never offers `--merge` or an approval, and names the routes the
// repo has: a squash by hand that keeps the sign-offs, re-authoring the
// hive's commits, or lifting the policy on the hive and on GitHub.
func TestMergeCommitRefusalTextSquashOnlyNamesRealRoutes(t *testing.T) {
	const (
		squashRoute = "accept the squash, which lands the hive's commits under the PR author's name, and squash by hand with a message that keeps each commit's `Signed-off-by` line, edited in the web UI or passed as `gh pr merge 991 --squash --body-file FILE` (`gh pr merge 991 --squash` alone sends GitHub's default squash message, which can drop those lines, and a merge queue squashes with the repo's default message and no edit, so they may be lost there)"
		reauthor    = "re-author the hive's commits under a maintainer's own sign-off and push a new head"
		lift        = "lift the squash-only policy for this repo (`auto_merge.squash_only`, and allow merge commits on GitHub, in the repo settings and the branch ruleset), then a maintainer merges with a merge commit."
		gate        = ", and only once the `Sign-off survives the squash` check passes or an admin bypasses it"
		hiveOnlyNo  = " (not recommended here: no commit carries the PR author's `Signed-off-by`, so the squash fails DCO unless its message carries the PR author's own sign-off)"
	)
	common := []string{
		"`acme/widget` is squash-only (`auto_merge.squash_only`), so a merge commit is not available",
		"**Ask (maintainer):**",
		squashRoute, reauthor, lift,
	}
	ruleNote := "approved or not: " + squashOnlyTestRule
	for _, tc := range []struct {
		name, refused, rule string
		gate                bool
		want                []string
		not                 []string
	}{
		{name: "role", refused: mergeCommitRefusedRole, want: []string{"only on a `role/trusted` PR or a head a trusted maintainer has approved", "so an approval would not get one either", "(a) " + squashRoute + "; (b) " + reauthor + "; or (c) " + lift}},
		{name: "role with a rule", refused: mergeCommitRefusedRole, rule: squashOnlyTestRule, want: []string{"The hive will not merge this head itself, " + ruleNote, "review the change and land it by hand (an approving review would not land it)"}},
		{name: "hive only", refused: mergeCommitRefusedHiveOnly, want: []string{"no contributor authorship for a merge commit to preserve", "close this PR if the hive's change is not wanted", "(a) " + reauthor + "; (b) " + squashRoute + hiveOnlyNo + "; or (c) " + lift}},
		{name: "hive only with a rule", refused: mergeCommitRefusedHiveOnly, rule: squashOnlyTestRule, want: []string{"close this PR if the hive's change is not wanted", ruleNote, "an approving review would not land it"}},
		{name: "hive only with the attribution check", refused: mergeCommitRefusedHiveOnly, gate: true, want: []string{"(a) " + reauthor + "; (b) " + squashRoute + gate + hiveOnlyNo + "; or (c) " + lift}},
		{name: "unsigned", refused: mergeCommitRefusedUnsigned, want: []string{"Some commits on the head carry no sign-off at all", "decide whether those commits need a sign-off before merging; if not, pick one: (a) " + squashRoute + "; (b) "}, not: []string{gate, "not recommended"}},
		{name: "unsigned with the attribution check", refused: mergeCommitRefusedUnsigned, gate: true, want: []string{"(a) " + squashRoute + gate + "; (b) " + reauthor}},
		{name: "unsigned with a rule", refused: mergeCommitRefusedUnsigned, rule: squashOnlyTestRule, want: []string{"decide whether those commits need a sign-off", ruleNote, "an approving review would not land it"}},
		{name: "labels suppressed", refused: mergeCommitRefusedLabelsSuppressed, want: []string{"write-back gate", "`merge-method/merge`", "would not help"}, not: []string{"add `merge-method/merge`"}},
		{name: "labels suppressed with a rule", refused: mergeCommitRefusedLabelsSuppressed, rule: squashOnlyTestRule, want: []string{"would not help", ruleNote, "an approving review would not land it"}, not: []string{"add `merge-method/merge`"}},
		{name: "default", want: []string{"Every commit already carries a sign-off matching its own author, but", "(a) " + squashRoute + "; (b) " + reauthor}},
		{name: "default with a rule", rule: squashOnlyTestRule, want: []string{ruleNote, "an approving review would not land it"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := stageObservation{Number: 991, Display: "acme/widget", Detail: stageDetail{MergeCommitRefused: tc.refused, ManualMergeRule: tc.rule, SquashOnly: true, SquashSignoffGate: tc.gate}}
			got := mergeCommitRefusalText(obs)
			for _, want := range append(append([]string{}, common...), tc.want...) {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q:\n%s", want, got)
				}
			}
			for _, bad := range append([]string{"--merge", "approve the current head", "**Approve**", "the hive merges with a merge commit", "merge or queue", "@", "\u2014", "\u2013"}, tc.not...) {
				if strings.Contains(got, bad) {
					t.Fatalf("offers %q in a squash-only repo:\n%s", bad, got)
				}
			}
			if strings.Count(got, ruleNote) > 1 {
				t.Fatalf("rule named twice:\n%s", got)
			}
			if tc.gate != strings.Contains(got, gate) {
				t.Fatalf("attribution check caveat (gate=%v):\n%s", tc.gate, got)
			}
			obs.Detail.SentinelAlert = true
			if withSentinel := mergeCommitRefusalText(obs); withSentinel != got+" A sentinel alert is open on this PR: resolve it and have a maintainer clear its label first." {
				t.Fatalf("sentinel: %q", withSentinel)
			}
		})
	}
}

// The bluefin-bling#200 shape end to end: the 085 note for unsigned commits
// in a squash-only repo whose head runs the attribution check names the
// squash routes, not `--merge`, and says the check must pass first.
func TestSignoffAttributionDecisionCommentSquashOnlyUnsigned(t *testing.T) {
	obs := stageObservation{Number: 200, Display: "acme/widget", Author: "contributor", HeadSHA: "sha", Detail: stageDetail{MergeCommitRefused: mergeCommitRefusedUnsigned, SquashOnly: true, SquashSignoffGate: true, DCOGateHead: "sha", DCOSquashUnsafeHive: 1, DCOTotal: 3}}
	got := SignoffAttributionDecisionComment(obs)
	for _, want := range []string{"(the `Sign-off survives the squash` check stays red)", "decide whether those commits need a sign-off", "`gh pr merge 200 --squash --body-file FILE`", "only once the `Sign-off survives the squash` check passes or an admin bypasses it", "re-author the hive's commits", "lift the squash-only policy", "allow merge commits on GitHub"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "--merge") {
		t.Fatalf("offers a merge commit in a squash-only repo:\n%s", got)
	}
}

// withMergeAskFacts reads the squash-only fact from the same policy, for the
// same "owner/repo", as mergeCommitElection, even when the sweep displays a
// bare repo name.
func TestWithMergeAskFactsReadsSquashOnlyPolicy(t *testing.T) {
	c := &Engine{stageLabelPolicy: func() StageLabelPolicy {
		return StageLabelPolicy{SquashOnly: func(repo string) bool { return repo == "acme/widget" }}
	}}
	obs := stageObservation{Owner: "acme", Repo: "widget", Display: "widget", Number: 1}
	if got := c.withMergeAskFacts(context.Background(), "k", obs); !got.Detail.SquashOnly {
		t.Fatal("squash-only repo not marked")
	}
	obs.Repo, obs.Display = "other", "other"
	if got := c.withMergeAskFacts(context.Background(), "k", obs); got.Detail.SquashOnly {
		t.Fatal("repo that allows merge commits marked squash-only")
	}
	none := &Engine{stageLabelPolicy: func() StageLabelPolicy { return StageLabelPolicy{} }}
	if got := none.withMergeAskFacts(context.Background(), "k", stageObservation{Owner: "acme", Repo: "widget"}); got.Detail.SquashOnly {
		t.Fatal("no squash-only policy marked squash-only")
	}
}
