package dashboard

import (
	"strings"
	"testing"
)

// Let hive decide answers at once, a covered issue defers to its PR's row,
// and a person's move says why (hivecommons/hive#15773).
func TestStageTrackerLetHiveDecideBusyFeedbackJS(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"Asking the hive…",
		"lead.setAttribute('aria-busy', 'true')",
		"stageTrackerSetPending([].concat(...items.map(it => stageTrackerPendingKeys(it, rows)))",
		`.ham-lead-act[aria-busy="true"]`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}

func TestStageTrackerCoveredIssueDropsPRChipJS(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{"stageTrackerPRListedOwnRow(item, a.target)", "decision pending on #"} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}

func TestStageTrackerHumanChevronSaysWhyJS(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "function stageTrackerHumanWhy(item, kind)") || !strings.Contains(html, "sa.why || humanWhy(item, sa.kind)") {
		t.Error("the human next-move tooltip must fall back to the row's reason")
	}
}
