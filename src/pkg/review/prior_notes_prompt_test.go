package review

import (
	"strings"
	"testing"
	"time"
)

// Every prompt that can raise findings tells the reviewer not to re-report
// another perspective's or an earlier head's note as a finding, and how to
// cite one instead (hivecommons/hive#15627, #15628, #15629 were one finding
// reported by three perspectives).
func TestPromptsForbidReReportingPriorNotes(t *testing.T) {
	pr := dispatchPR("sha2")
	pr.Repo = "acme/hive"
	delta := &DeltaContext{PreviousHead: "sha1", PreviousVerdict: VerdictApprove, PreviousRecordedAt: deltaNow.Add(-time.Hour), DiffKnown: true, Files: []string{"a.go"}, ChangedLines: 25}
	prompts := map[string]string{
		"perspective": BuildPerspectivePromptWith(PerspectiveStyle, pr, PromptOptions{}),
		"combined":    BuildCombinedPrompt(pr, DefaultPerspectives, PromptOptions{}),
		"delta":       deltaSection(pr, delta),
		"decision":    BuildDecisionPrompt(pr, []Perspective{PerspectiveCorrectness, PerspectiveSecurity}, Aggregate{}, PromptOptions{}),
	}
	for name, p := range prompts {
		if !strings.Contains(p, ReviewRefersToMarker+" #N -->") {
			t.Errorf("%s prompt does not name the refers-to marker", name)
		}
		if !strings.Contains(p, "Previous style note still present") && !strings.Contains(p, "Prior finding unchanged") && !strings.Contains(p, "Previous … still present") {
			t.Errorf("%s prompt does not show what a carry-over looks like", name)
		}
	}
	if p := prompts["perspective"]; !strings.Contains(p, "Do NOT re-report another perspective's note") {
		t.Errorf("perspective prompt lacks the rule:\n%s", p)
	}
	if p := prompts["decision"]; !strings.Contains(p, "Each UNDERLYING finding appears once") {
		t.Errorf("decision prompt lacks the once-per-finding rule")
	}
}
