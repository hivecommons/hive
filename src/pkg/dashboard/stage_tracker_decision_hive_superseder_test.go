package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The hive's superseding PR parked by its one exception class: the tracker
// names the stop rather than "needs your decision".
func TestDecisionForPRHiveSupersederHardStopNamesReason(t *testing.T) {
	it := StageTrackerItem{Repo: "hivecommons/hive", Number: 15649, Kind: "pr", Stage: 85, Head: "abc1234",
		Reason: stagelabels.LaneReasonHiveSupersederHardStopPrefix + stagelabels.HiveSupersederHardStopSentinel}
	d := decisionForPR(it, nil, "", false)
	if d.AskedBy != decisionAskedByLane || !strings.Contains(d.Question, "would auto-merge") || !strings.Contains(d.Question, stagelabels.HiveSupersederHardStopProse(stagelabels.HiveSupersederHardStopSentinel)) {
		t.Fatalf("decision = %+v", d)
	}
}
