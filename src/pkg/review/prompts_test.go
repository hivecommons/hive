package review

import "testing"

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
