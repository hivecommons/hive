package review

import (
	"strings"
	"testing"
)

// projectbluefin/homebrew-tap#60: a review of head 95440f7 cited "cask-payload
// fails on this head (job 127519980309)"; that job ran on d892290. Every
// review prompt pins CI evidence to the reviewed head, like code citations.
func TestReviewPromptsPinCIEvidenceToHead(t *testing.T) {
	pr := combinedBoilerplatePR()
	want := "CI evidence follows the same rule: a check run, job or log speaks for this head only when its head_sha is " + testSHA
	jobRead := "gh api repos/o/r/actions/jobs/<job id> --jq .head_sha"
	for name, prompt := range map[string]string{
		"combined": BuildCombinedPrompt(pr, DefaultPerspectives, combinedBoilerplateOpts()),
		"decision": BuildDecisionPrompt(pr, DefaultPerspectives, Aggregate{}, combinedBoilerplateOpts()),
	} {
		if !strings.Contains(prompt, want) || !strings.Contains(prompt, jobRead) || !strings.Contains(prompt, "A run on an earlier head says nothing about this one") {
			t.Errorf("%s prompt does not pin CI evidence to the head", name)
		}
	}
	pr.HeadSHA = ""
	if strings.Contains(buildReadInstruction(pr), "CI evidence follows the same rule") {
		t.Error("with no head to name, the rule has nothing to pin to")
	}
}
