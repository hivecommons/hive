package review

import (
	"strings"
	"testing"
)

// The schema example quoted to agents must itself pass the relay's validator,
// or the agent is told to copy a shape that will be refused.
func TestVerdictSchemaExampleValidates(t *testing.T) {
	reports, err := ValidateReportsFor([]byte(VerdictSchemaExample), PerspectiveSet{})
	if err != nil {
		t.Fatalf("VerdictSchemaExample must validate: %v", err)
	}
	if len(reports) != 1 || reports[0].Perspective != PerspectiveCorrectness || reports[0].Verdict != VerdictRequiresHuman {
		t.Fatalf("unexpected parse of the example: %+v", reports)
	}
}

func TestPromptRoutesHiveAuthoredIncompleteFixesToChangesRequested(t *testing.T) {
	pr := PullRequest{Repo: "hivecommons/hive", Number: 12875, AuthorAgent: "scanner"}
	got := BuildPerspectivePrompt(PerspectiveCorrectness, pr)
	want := "A Hive-authored incomplete fix with concrete remediation is changes_requested, not requires_human."
	if !strings.Contains(got, want) || !strings.Contains(got, "add producer code OR reword a changelog") || !strings.Contains(got, "duplicates already-landed work") {
		t.Fatalf("prompt missing incomplete-fix routing instruction: %q", got)
	}
}
