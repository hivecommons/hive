package review

import (
	"strings"
	"testing"
)

// A first review is told to anchor line findings (hivecommons/hive#18531);
// a revision edits in place and cannot open threads, so it is not.
func TestPublishInstructionInlineComments(t *testing.T) {
	pr := PullRequest{Repo: "o/r", Number: 7}
	first := buildPublishInstruction(pr, false, false)
	if !strings.Contains(first, "--comments-file <comments>") || !strings.Contains(first, inlineCommentsInstruction) {
		t.Errorf("first-review publish instruction must offer --comments-file:\n%s", first)
	}
	if !strings.Contains(inlineCommentsInstruction, "ONE review") {
		t.Errorf("instruction must say the comments ride one review")
	}
	revise := buildPublishInstruction(pr, false, true)
	if strings.Contains(revise, "--comments-file") {
		t.Errorf("revise instruction must not offer inline comments:\n%s", revise)
	}
}
