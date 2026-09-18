package review

import (
	"strings"
	"testing"
)

func testPR() PullRequest {
	return PullRequest{
		Repo:    "projectbluefin/common",
		Number:  971,
		Title:   "docs(skills): add GPU vendor toolkit interface spec",
		Author:  "kubestellar-hive[bot]",
		HeadSHA: "0f84e7b0ac6fc2f7937397a28bc0a810ccd9431e",
		URL:     "https://github.com/projectbluefin/common/pull/971",
	}
}

// TestPromptOmitsPublishByDefault is the compatibility guarantee: every hive
// that has not opted in keeps the exact prompt it had before. The publish step
// must be additive.
func TestPromptOmitsPublishByDefault(t *testing.T) {
	got := BuildPerspectivePrompt(PerspectiveCorrectness, testPR())

	if strings.Contains(got, "PUBLISH YOUR VERDICT") {
		t.Error("default prompt contains the publish instruction; it must be opt-in")
	}
	if strings.Contains(got, "hive-review") {
		t.Error("default prompt mentions hive-review; it must be opt-in")
	}
	// The original content must still be there.
	if !strings.Contains(got, "Return exactly one JSON object") {
		t.Error("default prompt lost the JSON verdict instruction")
	}
}

// TestPromptPublishIsAdditive checks that opting in adds the publish step
// WITHOUT dropping anything: the JSON aggregate still feeds the merge gate, so
// a reviewer that now comments must keep producing it.
func TestPromptPublishIsAdditive(t *testing.T) {
	base := BuildPerspectivePrompt(PerspectiveCorrectness, testPR())
	got := BuildPerspectivePromptOpts(PerspectiveCorrectness, testPR(), true)

	if !strings.HasPrefix(got, base) {
		t.Fatal("opted-in prompt is not a strict superset of the default prompt; " +
			"the publish step must be appended, not substituted")
	}
	if !strings.Contains(got, "PUBLISH YOUR VERDICT") {
		t.Error("opted-in prompt is missing the publish instruction")
	}
}

// TestPromptPublishCommandIsCorrect pins the exact relay invocation. A prompt
// that names the wrong flag or the wrong repo sends the agent to `gh pr review`
// when the shim fails, which is the unaudited path this exists to avoid.
func TestPromptPublishCommandIsCorrect(t *testing.T) {
	got := BuildPerspectivePromptOpts(PerspectiveCorrectness, testPR(), true)

	want := "hive-review 971 --repo projectbluefin/common --comment --body-file"
	if !strings.Contains(got, want) {
		t.Errorf("prompt does not contain the expected relay command.\nwant substring: %s", want)
	}
	if !strings.Contains(got, "never `gh pr review`") {
		t.Error("prompt does not steer the agent away from the unaudited gh path")
	}
}

// TestPromptPublishWithholdsMergeAuthority is the safety assertion. Publishing
// must not become approving: the relay gates approve/request_changes behind a
// higher bar, and a prompt that invited them would push agents into requests
// that are denied — or, on a push-capable agent, actually influence a merge.
func TestPromptPublishWithholdsMergeAuthority(t *testing.T) {
	got := BuildPerspectivePromptOpts(PerspectiveCorrectness, testPR(), true)

	publish := got[strings.Index(got, "PUBLISH YOUR VERDICT"):]
	for _, forbidden := range []string{"--approve", "--request-changes", "--resolve-thread"} {
		if strings.Contains(publish, forbidden) {
			t.Errorf("publish instruction offers %q; it must be comment-only", forbidden)
		}
	}
	if !strings.Contains(publish, "Only --comment") {
		t.Error("publish instruction does not restrict the agent to --comment")
	}
	if !strings.Contains(publish, "a human decides") {
		t.Error("publish instruction does not reserve the decision for a human")
	}
}

// TestPromptPublishDiscouragesNoiseComments guards the failure mode the
// measured experiment identified: false positives and padding. Now that the
// output lands on a contributor's PR rather than in a file nobody reads, a
// prompt that does not license silence will produce noise at queue scale.
func TestPromptPublishDiscouragesNoiseComments(t *testing.T) {
	got := BuildPerspectivePromptOpts(PerspectiveCorrectness, testPR(), true)

	if !strings.Contains(got, "skip the comment entirely") {
		t.Error("publish instruction does not permit skipping a useless comment")
	}
	if !strings.Contains(got, "file:line") {
		t.Error("publish instruction does not require file:line evidence")
	}
}

// TestPromptPublishAppliesToEveryPerspective makes sure the publish step is not
// accidentally bound to one perspective — every reviewer in the swarm needs it.
func TestPromptPublishAppliesToEveryPerspective(t *testing.T) {
	for _, p := range DefaultPerspectives {
		got := BuildPerspectivePromptOpts(p, testPR(), true)
		if !strings.Contains(got, "PUBLISH YOUR VERDICT") {
			t.Errorf("perspective %q is missing the publish instruction", p)
		}
	}
}

// TestRoutingMarkerIsPinned pins the exact marker a maintainer filters on. The
// value of the marker is entirely in its being identical across every repo and
// every reviewer; a reworded variant is unsearchable and therefore useless.
func TestRoutingMarkerIsPinned(t *testing.T) {
	got := BuildPerspectivePromptOpts(PerspectiveCorrectness, testPR(), true)

	if !strings.Contains(got, "**HUMAN DECISION NEEDED**") {
		t.Error("publish prompt does not pin the HUMAN DECISION NEEDED marker")
	}
	if !strings.Contains(got, "requires_human or reject") {
		t.Error("publish prompt does not tie the marker to the blocking verdicts")
	}
}

// TestRoutingDoesNotMentionBotAuthors is the point of the author check: the
// common case on a hive is an agent-authored PR, and mentioning the App that
// opened it notifies nobody while looking like the review was routed.
func TestRoutingDoesNotMentionBotAuthors(t *testing.T) {
	for _, author := range []string{
		"kubestellar-hive[bot]",
		"app/kubestellar-hive",
		"",
	} {
		pr := testPR()
		pr.Author = author
		got := BuildPerspectivePromptOpts(PerspectiveCorrectness, pr, true)

		if strings.Contains(got, "(the PR author) so the person who can act") {
			t.Errorf("author %q: prompt asks the reviewer to mention a non-human author", author)
		}
		if !strings.Contains(got, "Do NOT @-mention the PR author") {
			t.Errorf("author %q: prompt omits the do-not-mention instruction", author)
		}
	}
}

// TestRoutingMentionsHumanAuthors is the converse: when a person opened the PR,
// the mention is the fastest path from finding to decision.
func TestRoutingMentionsHumanAuthors(t *testing.T) {
	pr := testPR()
	pr.Author = "@clubanderson"
	got := BuildPerspectivePromptOpts(PerspectiveCorrectness, pr, true)

	if !strings.Contains(got, "mention @clubanderson (the PR author)") {
		t.Error("prompt does not ask the reviewer to mention the human PR author")
	}
	// The leading @ must not be doubled when the author already carries one.
	if strings.Contains(got, "@@") {
		t.Error("prompt double-prefixed the author handle with @")
	}
	if strings.Contains(got, "Do NOT @-mention the PR author") {
		t.Error("prompt suppressed the mention for a human author")
	}
}

// TestRoutingAsksForReviewLimits covers the capability-honesty half: a reviewer
// that cannot judge something must say so rather than approve around it.
func TestRoutingAsksForReviewLimits(t *testing.T) {
	got := BuildPerspectivePromptOpts(PerspectiveCorrectness, testPR(), true)

	if !strings.Contains(got, "Report the limits of your own review") {
		t.Error("publish prompt does not ask the reviewer to report what it could not judge")
	}
}

// TestRoutingIsOptInWithPublish keeps routing on the same switch as publishing:
// a hive that has not opted into comments must not have its prompt changed.
func TestRoutingIsOptInWithPublish(t *testing.T) {
	got := BuildPerspectivePrompt(PerspectiveCorrectness, testPR())

	if strings.Contains(got, "HUMAN DECISION NEEDED") {
		t.Error("default prompt contains the routing marker; routing must be opt-in with publishing")
	}
	if strings.Contains(got, "ROUTING") {
		t.Error("default prompt contains the routing block; routing must be opt-in with publishing")
	}
}
