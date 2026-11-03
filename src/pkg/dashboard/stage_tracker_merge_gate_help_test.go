package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStageTrackerMergeGateLabelsHelpCoverage keeps the 090 merge-gate
// labels identical across the server's decision options, the tracker's
// JS label table and the help page, with "Approve and let hive merge"
// as the lead and "Merge now as maintainer" secondary.
func TestStageTrackerMergeGateLabelsHelpCoverage(t *testing.T) {
	html := indexHTML(t)
	labels := jsConstObject(t, html, "const STAGE_TRACKER_ACTION_LABELS")
	// The merge-gate legend lives on the Activity Monitor page of the split help.
	helpDoc := filepath.Join("..", "..", "docs", "dashboard-sections", "activity-monitor.md")
	raw, err := os.ReadFile(helpDoc)
	if err != nil {
		t.Fatalf("reading %s: %v", helpDoc, err)
	}
	help := string(raw)
	for action, label := range map[string]string{
		decisionActionQueueMerge: decisionQueueMergeLabel,
		decisionActionMergeNow:   decisionMergeNowLabel,
	} {
		if want := "'" + action + "': '" + label + "'"; !strings.Contains(labels, want) {
			t.Errorf("STAGE_TRACKER_ACTION_LABELS lacks %s: %s", want, labels)
		}
		if !strings.Contains(help, "**"+label+"**") {
			t.Errorf("%s has no help entry for %q", helpDoc, label)
		}
	}
	if strings.Index(labels, "'"+decisionActionQueueMerge+"'") > strings.Index(labels, "'"+decisionActionMergeNow+"'") {
		t.Errorf("queue-merge must be listed before merge-now: %s", labels)
	}
	for _, stale := range []string{"as administrator", "Approve & merge", "Approve — let hive merge", "Let hive merge it"} {
		if strings.Contains(html, stale) || strings.Contains(help, stale) {
			t.Errorf("stale merge-gate wording %q still present", stale)
		}
	}
}
