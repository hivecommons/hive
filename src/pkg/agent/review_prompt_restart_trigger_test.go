package agent

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/review"
)

// The review prompt is echoed verbatim into the reviewer's tmux pane. The
// agent poller scans that pane for FatalNetworkErrorPatterns and
// restarts the agent on a hit once it has been quiet for a few seconds. The
// prompt text "If you cannot read the diff — fetch failed, or it is too
// large" (#7553) matched "fetch failed", so every reviewer, adjudicator and
// ci-maintainer kick that paused to think was killed as a dead network:
// 245 restarts on one hive, no verdict ever recorded, and every PR on the
// repo stuck at "awaiting review approval". No prompt builder may ever
// contain one of those substrings.
func TestReviewPromptsNeverContainFatalNetworkRestartTriggers(t *testing.T) {
	pr := review.PullRequest{
		Repo:    "acme/widgets",
		Number:  42,
		Title:   "fix: thing",
		Author:  "someone",
		HeadSHA: "0123456789abcdef",
		URL:     "https://github.com/acme/widgets/pull/42",
	}
	variants := map[string]string{
		"combined-default": review.BuildCombinedPrompt(pr, review.DefaultPerspectives, review.PromptOptions{}),
		"combined-post":    review.BuildCombinedPrompt(pr, review.DefaultPerspectives, review.PromptOptions{PostComments: true, AcknowledgeNoFindings: true}),
		"combined-revise":  review.BuildCombinedPrompt(pr, review.DefaultPerspectives, review.PromptOptions{PostComments: true, Revise: true}),
	}
	for _, p := range append([]review.Perspective(nil), append(review.DefaultPerspectives, review.PerspectivePlanMatch)...) {
		variants["perspective-"+string(p)] = review.BuildPerspectivePromptWith(p, pr, review.PromptOptions{PostComments: true})
	}
	for name, prompt := range variants {
		for _, pat := range FatalNetworkErrorPatterns {
			if strings.Contains(prompt, pat) {
				t.Errorf("%s prompt contains %q, which the agent poller treats as a fatal network error and restarts the reviewer on", name, pat)
			}
		}
	}
}
