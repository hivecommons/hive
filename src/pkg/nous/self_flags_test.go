package nous

import "testing"

func TestSelfFlagGate(t *testing.T) {
	var gate SelfFlagGate
	assignment := &SelfFlagAssignment{ExperimentID: "exp-1", Flag: "review-v2", Enabled: true}
	if gate.Enabled("review-v2") || gate.Enabled("") {
		t.Fatal("zero-value gate must be off")
	}
	gate.Apply(false, assignment)
	if gate.Enabled("review-v2") {
		t.Fatal("hub assignment bypassed local opt-in")
	}
	gate.Apply(true, assignment)
	if !gate.Enabled("review-v2") || gate.Enabled("other") || gate.Assignment().ExperimentID != "exp-1" {
		t.Fatal("assignment not applied to exactly its flag")
	}
	assignment.Enabled = false
	if !gate.Enabled("review-v2") {
		t.Fatal("caller mutated gate through assignment pointer")
	}
	gate.Apply(true, assignment)
	if gate.Enabled("review-v2") {
		t.Fatal("explicit revert did not turn off flag")
	}
	assignment.Enabled = true
	for _, tc := range []struct {
		name  string
		optIn bool
		next  *SelfFlagAssignment
	}{
		{"withdrawal or old hub", true, nil},
		{"local opt-out", false, assignment},
		{"invalid assignment", true, &SelfFlagAssignment{Flag: "review-v2", Enabled: true}},
		{"different experiment flag", true, &SelfFlagAssignment{ExperimentID: "exp-2", Flag: "other", Enabled: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate.Apply(true, assignment)
			gate.Apply(tc.optIn, tc.next)
			if gate.Enabled("review-v2") {
				t.Fatal("old flag remained enabled")
			}
		})
	}
}

func TestSelfFlagAssignmentValidation(t *testing.T) {
	if err := (SelfFlagAssignment{}).Validate(); err == nil || err.Error() != "invalid self-experiment experiment_id" {
		t.Fatalf("validation order must be stable: %v", err)
	}
	for _, a := range []SelfFlagAssignment{
		{}, {ExperimentID: "exp"}, {Flag: "flag"},
		{ExperimentID: " exp", Flag: "flag"},
		{ExperimentID: "exp", Flag: "flag\n"},
	} {
		if a.Validate() == nil {
			t.Errorf("accepted invalid assignment: %+v", a)
		}
	}
}
