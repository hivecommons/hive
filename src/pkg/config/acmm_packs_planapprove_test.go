package config

import "testing"

// TestPlanAutoApproveForLevel checks the plan_auto_approve knob resolves from
// the embedded pack YAML: true only for fully autonomous L6, false for L1-L5
// and unknown levels.
func TestPlanAutoApproveForLevel(t *testing.T) {
	cases := []struct {
		level int
		want  bool
	}{
		{1, false},
		{2, false},
		{3, false},
		{4, false},
		{5, false},  // Gate 2 stays on at L5 (RFC hivecommons/hive#7993 §4)
		{6, true},   // L6 "runs itself"
		{99, false}, // unknown level → safe default
	}
	for _, c := range cases {
		if got := PlanAutoApproveForLevel(c.level); got != c.want {
			t.Errorf("PlanAutoApproveForLevel(%d) = %v, want %v", c.level, got, c.want)
		}
	}
}

// TestPackGovernorPlanAutoApproveField verifies the field is parsed onto
// PackGovernor for the levels that set it.
func TestPackGovernorPlanAutoApproveField(t *testing.T) {
	p6, err := ACMMPackByLevel(6)
	if err != nil {
		t.Fatalf("level 6: %v", err)
	}
	if !p6.Governor.PlanAutoApprove {
		t.Error("level 6 pack should have plan_auto_approve=true")
	}
	p5, err := ACMMPackByLevel(5)
	if err != nil {
		t.Fatalf("level 5: %v", err)
	}
	if p5.Governor.PlanAutoApprove {
		t.Error("level 5 pack should have plan_auto_approve=false: Gate 2 is on at L5 (RFC #7993 §4)")
	}
	p1, err := ACMMPackByLevel(1)
	if err != nil {
		t.Fatalf("level 1: %v", err)
	}
	if p1.Governor.PlanAutoApprove {
		t.Error("level 1 pack should have plan_auto_approve=false")
	}
}
