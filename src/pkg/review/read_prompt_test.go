package review

import (
	"strings"
	"testing"
)

// The prompt demands file:line citations, so it must also say how to obtain
// the diff. Without that, whether the reviewer actually read the change was
// left to chance — and an agent that skipped it could still emit confident,
// ungrounded prose, which costs a maintainer time to refute.
func TestPromptTellsReviewerToReadBodyAndDiff(t *testing.T) {
	pr := PullRequest{Repo: "o/r", Number: 42, Title: "t", HeadSHA: "deadbeef", Author: "someone"}
	got := BuildPerspectivePromptWith(PerspectiveCorrectness, pr, PromptOptions{})

	for _, want := range []string{
		"gh pr view 42 --repo o/r",
		"gh pr diff 42 --repo o/r",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt does not tell the reviewer to run %q:\n%s", want, got)
		}
	}

	// The body is the author's intent; the diff is what they did. Findings
	// worth reporting live in the gap, so both must be named.
	if !strings.Contains(got, "body") || !strings.Contains(got, "diff") {
		t.Fatal("prompt must name both the PR body and the diff")
	}

	// Pin citations to the dispatched head so a review cannot silently cite a
	// revision it never read.
	if !strings.Contains(got, "deadbeef") {
		t.Fatal("prompt must pin the reviewer's citations to the dispatched head SHA")
	}

	// The anti-hallucination escape hatch: an unreadable diff is an honest
	// requires_human, never a guess.
	if !strings.Contains(got, "requires_human") {
		t.Fatal("prompt must offer requires_human when the diff cannot be read")
	}

	// Reading instructions must not smuggle in the publish path, which stays
	// opt-in (see TestPromptOmitsPublishByDefault).
	if strings.Contains(got, "hive-review") {
		t.Fatal("read instruction leaked the publish relay into the default prompt")
	}
}

// Scope discipline: a review that relitigates code the PR does not touch reads
// as an obstacle rather than help, which is how an advisory reviewer loses the
// maintainers whose cooperation it depends on.
func TestPromptKeepsReviewScopedToTheDiff(t *testing.T) {
	got := BuildPerspectivePromptWith(PerspectiveStyle, PullRequest{Repo: "o/r", Number: 1}, PromptOptions{})
	if !strings.Contains(got, "not the surrounding code") {
		t.Fatalf("prompt must scope the review to the diff:\n%s", got)
	}
}
