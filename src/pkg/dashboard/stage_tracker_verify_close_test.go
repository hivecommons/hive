package dashboard

import (
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The verify-close lane: the scanner verified the issue complete and the
// hive stopped short of closing it, so the row leads with a one-click Close.
func TestVerifyCloseIssueDecisionLeadsWithClose(t *testing.T) {
	it := StageTrackerItem{Kind: "issue", Number: 17157, Stage: stagelabels.IssueNeedsDecision.Number(),
		Labels: []string{ghpkg.PartiallyAddressedLabel, ghpkg.VerifyCloseLabel}}
	d := stageTrackerDecisionNeededWith(it, nil, "", time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC), blockerOptionsContext{})
	if d == nil || d.AskedBy != decisionAskedByScanner || len(d.Options) == 0 {
		t.Fatalf("decision = %+v, want the verify-close decision", d)
	}
	lead := d.Options[0]
	if lead.Action != decisionActionCloseIssue || !lead.Lead || !lead.Enabled {
		t.Fatalf("lead option = %+v, want an enabled one-click %s", lead, decisionActionCloseIssue)
	}

	it.Labels = []string{ghpkg.PartiallyAddressedLabel}
	d = stageTrackerDecisionNeededWith(it, nil, "", time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC), blockerOptionsContext{})
	if d != nil && d.AskedBy == decisionAskedByScanner {
		t.Fatal("control: without the lane label the ordinary 045 decision applies")
	}
}
