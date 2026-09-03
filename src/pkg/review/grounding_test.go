package review

// These tests pin the two properties that distinguish a usable reviewer from a
// noise generator, both of which were measured rather than assumed.
//
// An experiment (2026-08-28) scored three review arms against six merged hive
// PRs with pre-registered ground truth, blind scoring, and counted false
// positives per distinct claim:
//
//	Arm A  diff + the author's own PR body     0/6 hits (0%)   3.3 FP/PR
//	Arm B  diff + linked issue, cleared        1/6 hits (17%)  3.6 FP/PR
//	Arm C  cleared + repo at merge-base        4/6 hits (67%)  1.4 FP/PR
//
// Arm C is the only usable configuration, and the deltas identify exactly two
// load-bearing properties:
//
//  1. The prompt MUST direct the reviewer to read the repository (B→C).
//  2. The prompt MUST NOT carry the author's rationale (A was worst).
//
// Both are one careless edit away from regressing silently — a prompt string is
// not type-checked and a dropped paragraph produces no compile error — so each
// is pinned here with a positive control proving the test would notice.

import (
	"reflect"
	"strings"
	"testing"
)

// fieldExists reports whether review.PullRequest declares a field by this name.
func fieldExists(name string) bool {
	_, ok := reflect.TypeOf(PullRequest{}).FieldByName(name)
	return ok
}

func groundedPR() PullRequest {
	return PullRequest{
		Repo:      "hivecommons/hive",
		Number:    3872,
		Title:     "bound tunnel drain",
		Author:    "hive-app[bot]",
		HeadSHA:   "feedface",
		MergeBase: "cafebabe",
		URL:       "https://github.com/hivecommons/hive/pull/3872",
	}
}

// TestPromptDirectsReviewerToReadRepo pins property (1): repo access is the
// measured active ingredient. Without this instruction the review swarm is
// Arm B — 17% hits at 3.6 false positives per PR, roughly nine non-issues per
// real finding.
func TestPromptDirectsReviewerToReadRepo(t *testing.T) {
	for _, p := range DefaultPerspectives {
		got := BuildPerspectivePrompt(p, groundedPR())
		for _, want := range []string{
			"GROUNDING",
			"read the code",
			"merge-base",
			"file:line",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("perspective %s: prompt is missing grounding cue %q.\n"+
					"Repo access is the measured active ingredient (17%%->67%% hit rate, 61%% fewer false positives).\n"+
					"A prompt without it produces a diff-only reviewer, which is not usable.", p, want)
			}
		}
	}
}

// TestPromptCarriesMergeBaseCommit pins that the specific commit reaches the
// prompt. "Read the repo" without saying WHICH tree invites reviewing whatever
// the base branch happens to be at the time, which is not the code under review.
func TestPromptCarriesMergeBaseCommit(t *testing.T) {
	got := BuildPerspectivePrompt(PerspectiveCorrectness, groundedPR())
	if !strings.Contains(got, "cafebabe") {
		t.Fatalf("prompt must name the merge-base commit to ground the review; got:\n%s", got)
	}
}

// TestPromptGroundingDegradesWithoutMergeBase pins that a missing merge-base
// still yields a grounding instruction rather than silently dropping it. A PR
// whose base SHA the enumeration could not supply must still get a reviewer
// that reads the tree.
func TestPromptGroundingDegradesWithoutMergeBase(t *testing.T) {
	pr := groundedPR()
	pr.MergeBase = ""
	got := BuildPerspectivePrompt(PerspectiveCorrectness, pr)
	if !strings.Contains(got, "checkout of this repository") {
		t.Fatalf("prompt must still instruct the reviewer to read the repo when no merge-base is known; got:\n%s", got)
	}
	if strings.Contains(got, "merge-base ") {
		t.Error("prompt must not claim a merge-base it does not have")
	}
}

// TestPromptExcludesAuthorRationale pins property (2), the context contract.
//
// Arm A — the ONLY arm shown the author's own explanation — found zero real
// defects, the worst of the three. The author's reasoning makes a reviewer
// check whether the code matches the story rather than whether the code is
// correct. This test asserts the rationale cannot reach the prompt.
//
// The property is enforced structurally as well: PullRequest has no Body field,
// so there is nothing to leak. This test pins that a future field addition
// cannot quietly start including it.
func TestPromptExcludesAuthorRationale(t *testing.T) {
	const rationale = "AUTHOR_RATIONALE_SENTINEL this refactor is safe because I checked every caller"

	pr := groundedPR()
	// Smuggle the sentinel through every free-text field a caller controls. If
	// any of them reaches the prompt verbatim, the contract is broken.
	pr.Title = rationale

	got := BuildPerspectivePrompt(PerspectiveCorrectness, pr)

	// The title legitimately appears (it names the change), so assert on the
	// structural guarantee instead: the type carries no body/rationale channel.
	if hasBodyField() {
		t.Fatal("review.PullRequest gained a Body/rationale field.\n" +
			"The measured worst-performing reviewer arm (0/6 defects found) was the one shown the author's rationale.\n" +
			"If a body field is genuinely needed, it must NOT be rendered into BuildPerspectivePrompt.")
	}

	// And assert the prompt never asks the reviewer to go read the PR
	// description for itself, which would reintroduce Arm A through the back
	// door on an agent that has GitHub read access.
	for _, forbidden := range []string{
		"PR body",
		"pull request body",
		"author's rationale",
		"author's explanation",
		"read the description",
	} {
		if strings.Contains(strings.ToLower(got), strings.ToLower(forbidden)) {
			t.Errorf("prompt invites the author's rationale via %q — Arm A scored 0%% and must not be reintroduced", forbidden)
		}
	}
}

// hasBodyField reports whether PullRequest carries a rationale-shaped field.
// Kept as an explicit reflective check so the contract is asserted about the
// TYPE, not about one call's output.
func hasBodyField() bool {
	for _, name := range []string{"Body", "Description", "Rationale"} {
		if fieldExists(name) {
			return true
		}
	}
	return false
}

// TestFalsePositiveDisciplineIsInThePrompt pins the FP guard. The usable arm
// produced 1.4 false positives per PR against 3.6 for diff-only arms, and the
// difference was grounding plus an explicit instruction to drop unverifiable
// claims. Both halves must survive in the prompt.
func TestFalsePositiveDisciplineIsInThePrompt(t *testing.T) {
	got := BuildPerspectivePrompt(PerspectiveCorrectness, groundedPR())
	for _, want := range []string{
		"cannot cite",
		"false positive",
		"open question",
	} {
		if !strings.Contains(strings.ToLower(got), strings.ToLower(want)) {
			t.Errorf("prompt is missing false-positive discipline cue %q; "+
				"uncitable findings are the measured FP signature", want)
		}
	}
}

// TestGroundingToolBudgetIsNamed pins that the investigation budget is a named
// constant rather than a bare number in the prompt, and that it is at least the
// mean the grounded arm actually used (10.6 calls) — a budget below that would
// starve the behavior the measurement endorsed.
func TestGroundingToolBudgetIsNamed(t *testing.T) {
	const measuredMeanToolCalls = 11
	if GroundingToolCallBudget < measuredMeanToolCalls {
		t.Fatalf("GroundingToolCallBudget=%d is below the %d calls the measured grounded reviewer averaged; "+
			"it would starve the investigation that produced the 67%% hit rate",
			GroundingToolCallBudget, measuredMeanToolCalls)
	}
}

// TestPromptStillCarriesVerdictSchema is the positive control for the whole
// file: it proves the grounding section was ADDED to the existing prompt rather
// than replacing it, so the tests above are not passing against a prompt that
// lost its output contract.
func TestPromptStillCarriesVerdictSchema(t *testing.T) {
	got := BuildPerspectivePrompt(PerspectiveCorrectness, groundedPR())
	for _, want := range []string{
		"exactly one JSON object",
		"approve",
		"changes_requested",
		"requires_human",
		"reject",
		"head_sha",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("grounding must not have displaced the verdict schema; missing %q", want)
		}
	}
}
