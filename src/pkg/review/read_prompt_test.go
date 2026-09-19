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
//
// Note this constrains what the reviewer may *report*, not what it may *read*.
// Those were once the same sentence ("Judge the diff, not the surrounding
// code"), and conflating them is what produced 93 "no findings" verdicts out of
// 95 on the bluefin spoke — including a 30-file, -1655-line PR. See
// TestPromptRequiresReadingTheSurroundingTree.
func TestPromptKeepsReviewScopedToTheDiff(t *testing.T) {
	got := BuildPerspectivePromptWith(PerspectiveStyle, PullRequest{Repo: "o/r", Number: 1}, PromptOptions{})
	for _, want := range []string{
		"Only defects this diff introduces or exposes are in scope",
		"pre-existing problems it does not touch stay out",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt must scope reporting to the diff (missing %q):\n%s", want, got)
		}
	}
	if strings.Contains(got, "not the surrounding code") {
		t.Fatal("prompt forbids reading the surrounding tree; that is the measured-worst reviewer configuration")
	}
}

// The hive's own measurement (policies/reviewer-queue.md) scored three reviewer
// configurations against merged PRs with known defects: the diff alone found
// 17% of them at 3.6 false positives per PR, while the diff plus the repository
// at merge-base found 67% at 1.4. Reading the tree is the single highest-value
// instruction in this kick, and shipping a prompt that omits it silently buys
// the worst arm of that experiment.
func TestPromptRequiresReadingTheSurroundingTree(t *testing.T) {
	got := BuildPerspectivePromptWith(PerspectiveCorrectness, PullRequest{
		Repo: "o/r", Number: 7, HeadSHA: "deadbeef",
	}, PromptOptions{})

	for _, want := range []string{
		"THE DIFF ALONE IS NOT ENOUGH",
		"the callers of what it changes",
		"67%",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("kick must direct the reviewer into the tree (missing %q):\n%s", want, got)
		}
	}

	// The read commands must be pinned to the dispatched head, or the reviewer
	// reads a different revision than the one it is judging.
	if !strings.Contains(got, "contents/<path>?ref=deadbeef") {
		t.Fatalf("tree-read command must be pinned to the dispatched head:\n%s", got)
	}

	// Reading widely must not become licence to report widely.
	if !strings.Contains(got, "Read widely; report narrowly") {
		t.Fatalf("kick must pair the wider read with a narrow reporting scope:\n%s", got)
	}
}
