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

func TestSubtleDefectGuidanceBelongsToCorrectness(t *testing.T) {
	pr := PullRequest{Repo: "o/r", Number: 7}
	if p := BuildPerspectivePrompt(PerspectiveCorrectness, pr); !strings.Contains(p, "Messages that stopped being true") || !strings.Contains(p, "Sentinels in aggregates") {
		t.Fatal("correctness prompt lacks the subtle-defect checks")
	}
	if p := BuildPerspectivePrompt(PerspectiveStyle, pr); strings.Contains(p, "Sentinels in aggregates") {
		t.Fatal("only correctness carries the subtle-defect checks")
	}
	if c := BuildCombinedPrompt(pr, []Perspective{PerspectiveCorrectness, PerspectiveStyle}, PromptOptions{}); !strings.Contains(c, "Sentinels in aggregates") {
		t.Fatal("combined prompt with correctness lacks the checks")
	}
	if c := BuildCombinedPrompt(pr, []Perspective{PerspectiveStyle}, PromptOptions{}); strings.Contains(c, "Sentinels in aggregates") {
		t.Fatal("combined prompt without correctness must not carry them")
	}
}
