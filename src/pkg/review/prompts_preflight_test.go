package review

import (
	"strings"
	"testing"
)

func TestPromptsCarryEvidenceOnlyWhenAttached(t *testing.T) {
	pr := PullRequest{Repo: "o/r", Number: 7}
	if p := BuildPerspectivePrompt(PerspectiveSecurity, pr); strings.Contains(p, "EVIDENCE GATHERED") {
		t.Fatal("no evidence attached must leave the prompt unchanged")
	}
	pr.PreflightSection = "\nDEPENDENCY EVIDENCE: x GHSA-aaaa-bbbb-cccc\n"
	p := BuildPerspectivePrompt(PerspectiveSecurity, pr)
	if !strings.Contains(p, "EVIDENCE GATHERED BY THE HIVE") || !strings.Contains(p, "GHSA-aaaa-bbbb-cccc") || !strings.Contains(p, "a finding without that is noise") {
		t.Fatalf("evidence missing from the perspective prompt:\n%s", p)
	}
	c := BuildCombinedPrompt(pr, []Perspective{PerspectiveCorrectness, PerspectiveSecurity}, PromptOptions{})
	if !strings.Contains(c, "GHSA-aaaa-bbbb-cccc") {
		t.Fatal("evidence missing from the combined prompt")
	}
}

func TestUnderstandingGuidanceBelongsToCorrectness(t *testing.T) {
	pr := PullRequest{Repo: "o/r", Number: 7}
	const marker = "UNDERSTAND THE CHANGE BEFORE JUDGING IT"
	p := BuildPerspectivePrompt(PerspectiveCorrectness, pr)
	if !strings.Contains(p, marker) || !strings.Contains(p, "invariants and contracts") || !strings.Contains(p, "concrete evidence from the code") {
		t.Fatal("correctness prompt lacks the understanding method")
	}
	for _, gone := range []string{"Sentinels in aggregates", "Messages that stopped being true", "TWO CHECKS DIFFS HIDE"} {
		if strings.Contains(p, gone) {
			t.Fatalf("the failure-mode checklist must be gone, found %q", gone)
		}
	}
	if p := BuildPerspectivePrompt(PerspectiveStyle, pr); strings.Contains(p, marker) {
		t.Fatal("only correctness carries the understanding method")
	}
	if c := BuildCombinedPrompt(pr, []Perspective{PerspectiveCorrectness, PerspectiveStyle}, PromptOptions{}); strings.Count(c, marker) != 1 {
		t.Fatal("combined prompt with correctness carries the method exactly once")
	}
	if c := BuildCombinedPrompt(pr, []Perspective{PerspectiveStyle}, PromptOptions{}); strings.Contains(c, marker) {
		t.Fatal("combined prompt without correctness must not carry it")
	}
}
