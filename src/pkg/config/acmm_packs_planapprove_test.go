package config

import "testing"

// TestPlanAutoApproveForLevel checks the plan_auto_approve knob resolves from
// the embedded pack YAML: true for high-trust levels (L5/L6), false for the
// lower levels, and false for unknown levels.
func TestPlanAutoApproveForLevel(t *testing.T) {
	cases := []struct {
		level int
		want  bool
	}{
		{1, false},
		{2, false},
		{3, false},
		{4, false},
		{5, true},
		{6, true},
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
	p5, err := ACMMPackByLevel(5)
	if err != nil {
		t.Fatalf("level 5: %v", err)
	}
	if !p5.Governor.PlanAutoApprove {
		t.Error("level 5 pack should have plan_auto_approve=true")
	}
	p1, err := ACMMPackByLevel(1)
	if err != nil {
		t.Fatalf("level 1: %v", err)
	}
	if p1.Governor.PlanAutoApprove {
		t.Error("level 1 pack should have plan_auto_approve=false")
	}
}
