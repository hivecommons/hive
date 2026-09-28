package review

import (
	"strings"
	"testing"
)

// The reviewer's read step must also fetch open review-bot threads
// (hivecommons/hive#9360): before this, a reviewer could call a PR "safe"
// while an unanswered Codex P1 sat on the same commit, because nothing ever
// told it to look. Only fires when classification.review_bots names a login;
// an empty list means the feature is off.
func TestPromptTellsReviewerToReadOpenBotThreads(t *testing.T) {
	pr := PullRequest{Repo: "o/r", Number: 42, HeadSHA: "deadbeef"}
	got := BuildPerspectivePromptWith(PerspectiveCorrectness, pr, PromptOptions{
		ReviewBotLogins: []string{"chatgpt-codex-connector[bot]"},
	})

	for _, want := range []string{
		"chatgpt-codex-connector[bot]",
		"reviewThreads",
		"agree",
		"disagree",
		"cannot verify",
		"blocks a clean or \"safe\" verdict",
		"do not reply in or resolve these threads yourself",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}

// Off is off: an empty ReviewBotLogins must not mention bot threads at all,
// matching classification.review_bots naming no login.
func TestPromptOmitsBotThreadsWhenDisabled(t *testing.T) {
	pr := PullRequest{Repo: "o/r", Number: 42}
	got := BuildPerspectivePromptWith(PerspectiveCorrectness, pr, PromptOptions{})
	for _, reject := range []string{"REVIEW-BOT FINDINGS", "reviewThreads"} {
		if strings.Contains(got, reject) {
			t.Errorf("prompt must not mention bot threads when review_bots is off, found %q:\n%s", reject, got)
		}
	}
}

// The combined prompt (one kick for every perspective) gets the same section.
func TestCombinedPromptTellsReviewerToReadOpenBotThreads(t *testing.T) {
	pr := PullRequest{Repo: "o/r", Number: 7}
	got := BuildCombinedPrompt(pr, []Perspective{PerspectiveCorrectness, PerspectiveSecurity}, PromptOptions{
		ReviewBotLogins: []string{"Copilot"},
	})
	if !strings.Contains(got, "Copilot") || !strings.Contains(got, "reviewThreads") {
		t.Errorf("combined prompt must also fetch open bot threads:\n%s", got)
	}
}
