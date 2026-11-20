package delegatedecision

import (
	"strings"
	"testing"
	"time"
)

// TestPlainWordsHumanMergesNoMergeQueuePromise: an approval on a PR whose
// repo's auto-merge is off (the repo's setting, or a hive below ACMM L6)
// must not promise a merge-queue merge or a deadline — a person merges it,
// and the hive has nothing left to wait for. With auto-merge on the
// wording is unchanged.
func TestPlainWordsHumanMergesNoMergeQueuePromise(t *testing.T) {
	d := Decision{Verdict: VerdictApprove, Rule: "default-approve", Text: "approve #8", Rationale: "r"}

	off := PlainWords(d, CommentOptions{Kind: KindPR, EffectTimeout: 30 * time.Minute, HumanMerges: true})
	t.Logf("auto-merge off:\n%s", off)
	for _, bad := range []string{"merge queue merges", "expected within", "If it doesn't happen", "Nothing by itself"} {
		if strings.Contains(off, bad) {
			t.Errorf("auto-merge-off words carry %q:\n%s", bad, off)
		}
	}
	for _, want := range []string{
		"**Hive decided:** Approved for merge.",
		"**What happens next:** A person merges this PR — auto-merge is off for this repo (its auto-merge setting, or the hive is below ACMM L6), so the hive will not merge it and has nothing left to do here.",
		"Merge it by hand on GitHub (or Merge now, for a maintainer) once its checks are green; approving again does not help.",
	} {
		if !strings.Contains(off, want) {
			t.Errorf("auto-merge-off words lack %q:\n%s", want, off)
		}
	}
	eff := ExpectedEffectWith(d, CommentOptions{Kind: KindPR, HumanMerges: true})
	if eff.Waits || eff.What != "" || eff.Settled == "" {
		t.Errorf("auto-merge-off effect must await nothing: %+v", eff)
	}

	on := PlainWords(d, CommentOptions{Kind: KindPR, EffectTimeout: 30 * time.Minute})
	want := "**What happens next:** The merge queue merges this PR once its checks are green — expected within 30 min.\n" +
		"**If it doesn't happen:** A maintainer can merge it by hand or send it back for a new commit; approving again does not help."
	if !strings.Contains(on, want) || strings.Contains(on, "a person merges this PR") {
		t.Errorf("auto-merge-on words changed:\n%s", on)
	}
	if got, base := ExpectedEffectWith(d, CommentOptions{Kind: KindPR}), ExpectedEffect(d, KindPR); got != base || !got.Waits {
		t.Errorf("ExpectedEffect must stay the auto-merge-on effect: %+v vs %+v", base, got)
	}
}

// TestHumanMergesOnlyTouchesTheMergeCase: the auto-merge switch changes
// only the verdicts whose effect was the merge-queue merge; an issue, a
// park-keeping verdict and a supersede read exactly as before.
func TestHumanMergesOnlyTouchesTheMergeCase(t *testing.T) {
	for _, tc := range []struct {
		d    Decision
		kind Kind
	}{
		{Decision{Verdict: VerdictApprove}, KindIssue},
		{Decision{Verdict: VerdictRecommendOnly, KeepsPark: true}, KindPR},
		{Decision{Verdict: VerdictAdoptSupersede}, KindPR},
		{Decision{Verdict: VerdictDirection}, KindPR},
		{Decision{Verdict: VerdictCloseSuperseded}, KindPR},
	} {
		on := PlainWords(tc.d, CommentOptions{Kind: tc.kind, EffectTimeout: 30 * time.Minute})
		off := PlainWords(tc.d, CommentOptions{Kind: tc.kind, EffectTimeout: 30 * time.Minute, HumanMerges: true})
		if on != off {
			t.Errorf("%s on %s changed with HumanMerges:\non:  %s\noff: %s", tc.d.Verdict, tc.kind, on, off)
		}
	}
}
