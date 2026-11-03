package hub

import (
	"testing"

	"github.com/hivecommons/hive/pkg/hivestate"
)

func TestIdleCriterionRequiresAgentObservations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		samples  int
		ratio    float64
		measured bool
	}{
		{"no observations", 0, 0, false},
		{"all running", 12, 0, true},
		{"legacy positive ratio", 0, 0.5, true},
		{"negative denominator", -1, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := sanitizeHiveStateWindow(hivestate.Window{Samples: 48, AgentSamples: tc.samples, AgentIdleRatio: tc.ratio}, 168)
			if tc.samples > 0 && w.AgentSamples != tc.samples {
				t.Fatal("sanitizer dropped observation denominator")
			}
			in := quadrantInputs{evidence: &HeartbeatEvidence{HiveState: &hivestate.Trends{Last7d: w}}}
			for _, c := range efficiencyCriteria(in) {
				if c.spec.name == critIdle {
					if c.measured != tc.measured {
						t.Fatalf("idle measured = %v, want %v", c.measured, tc.measured)
					}
					return
				}
			}
			t.Fatal("idle criterion missing")
		})
	}
}
