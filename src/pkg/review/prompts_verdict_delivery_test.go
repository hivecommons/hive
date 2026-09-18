package review

import (
	"strings"
	"testing"
)

// The kick is the only place the reviewer learns how to deliver its verdict.
// For 24h it demanded a JSON object and never said where to put it, so every
// verdict was printed to a terminal and discarded. These assertions exist so
// that cannot silently regress.
func TestKickTellsReviewerHowToDeliverTheVerdict(t *testing.T) {
	pr := PullRequest{Repo: "projectbluefin/common", Number: 1121, HeadSHA: "abc123"}

	t.Run("posting comments", func(t *testing.T) {
		got := BuildPerspectivePromptWith(PerspectiveCorrectness, pr, PromptOptions{PostComments: true, AcknowledgeNoFindings: true})
		if !strings.Contains(got, "--verdict-file") {
			t.Fatalf("kick never tells the agent to hand over the verdict:\n%s", got)
		}
		if !strings.Contains(got, "hive-review 1121 --repo projectbluefin/common --comment --body-file") {
			t.Fatalf("kick lost the comment command:\n%s", got)
		}
	})

	t.Run("skipping comments still records", func(t *testing.T) {
		got := BuildPerspectivePromptWith(PerspectiveCorrectness, pr, PromptOptions{PostComments: true, AcknowledgeNoFindings: false})
		if !strings.Contains(got, "--record-verdict") {
			t.Fatalf("a perspective that skips the comment is never told to record its verdict:\n%s", got)
		}
	})

	t.Run("comments disabled entirely", func(t *testing.T) {
		// Deliberately NOT instructed here. The relay authorizer gates on the
		// same push capability as opening a PR, so a hive that does not let its
		// reviewer comment would have every record_verdict denied. Naming the
		// command would only manufacture denials; see
		// TestPromptOmitsPublishByDefault for the compatibility contract.
		got := BuildPerspectivePromptWith(PerspectiveCorrectness, pr, PromptOptions{PostComments: false})
		if strings.Contains(got, "hive-review") {
			t.Fatalf("comments-off prompt must stay free of the relay:\n%s", got)
		}
	})
}
