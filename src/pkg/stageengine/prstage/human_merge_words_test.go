package prstage

import "testing"

// The dashboard's gate cell and the 090 entry comment read the same words
// per HumanMergeReason code (projectbluefin/website#1036), so every code a
// 090 can carry has them; HumanMergeReasonPolicy, the catch-all, has none
// and keeps its raw-reason wording.
func TestHumanMergeReasonWordsCoverEveryCode(t *testing.T) {
	for _, code := range []string{
		HumanMergeReasonWorkflowFiles, HumanMergeReasonTestsRemoved, HumanMergeReasonManualMergePolicy,
		HumanMergeReasonBranchProtection, HumanMergeReasonMergeMethod, HumanMergeReasonAutoMergeOff,
		HumanMergeReasonUntrustedAuthor, HumanMergeReasonUnauthorized, HumanMergeReasonReviewerDeferred,
		HumanMergeReasonBreakingChange,
	} {
		word, sentence, ok := HumanMergeReasonWords(code)
		if !ok || word == "" || sentence == "" {
			t.Errorf("%s: words = %q / %q (ok %v)", code, word, sentence, ok)
		}
	}
	for _, code := range []string{HumanMergeReasonPolicy, ""} {
		if _, _, ok := HumanMergeReasonWords(code); ok {
			t.Errorf("%q has words; the catch-all keeps the raw reason", code)
		}
	}
	if _, sentence, _ := HumanMergeReasonWords(HumanMergeReason("repo-auto-merge-disabled")); sentence != "Auto-merge is off for this repo, so a person merges" {
		t.Errorf("auto-merge-off sentence = %q", sentence)
	}
}
