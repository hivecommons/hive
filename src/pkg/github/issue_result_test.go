package github

import "testing"

func TestIssueResultFromItemsCountsSLAViolations(t *testing.T) {
	items := []Issue{
		{Number: 1, AgeMinutes: slaThresholdMinutes},
		{Number: 2, AgeMinutes: slaThresholdMinutes + 1},
		{Number: 3, AgeMinutes: slaThresholdMinutes + 20},
	}
	got := IssueResultFromItems(items)
	if got.Count != len(items) || len(got.Items) != len(items) {
		t.Fatalf("result shape = %+v, want %d items", got, len(items))
	}
	if got.SLAViolations != 2 {
		t.Errorf("SLAViolations = %d, want 2", got.SLAViolations)
	}
}
