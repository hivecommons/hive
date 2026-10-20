package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/fleetreport"
	"github.com/hivecommons/hive/pkg/inferencehealth"
)

func TestFleetReportEvidenceIncludesRuntimeGatewayFaults(t *testing.T) {
	fleetreport.SetDefaultTrackerForTest(t, fleetreport.NewRuntimeTracker(nil))
	SetGatewayHealthProvider(func() []inferencehealth.GatewayStatus {
		return []inferencehealth.GatewayStatus{
			{Name: "litellm", ErrorClass: inferencehealth.ClassAuth, HTTPStatus: 403},
			{Name: "copilot", ErrorClass: inferencehealth.ClassOther, HTTPStatus: 502},
		}
	})
	t.Cleanup(func() { SetGatewayHealthProvider(nil) })
	s := &Server{}
	got := s.buildFleetReportEvidence(&StatusPayload{})
	if len(got) != 1 {
		t.Fatalf("evidence len=%d, want 1: %#v", len(got), got)
	}
	// Auth faults are no longer a hive-code-defect symptom here: the
	// inferencehealth store does not self-report them either (#12284),
	// so only the upstream fault remains.
	if got[0].Component != "inference-gateway" || got[0].ErrorClass != "other http 502" || !got[0].Attributable {
		t.Fatalf("unexpected evidence: %#v", got[0])
	}
}

func TestFleetReportEvidenceIncludesRuntimeErrorClasses(t *testing.T) {
	fleetreport.SetDefaultTrackerForTest(t, fleetreport.NewRuntimeTracker(nil))
	fleetreport.Observe(fleetreport.ClassRestartStorm, "quality", "restart storm: quality restarted 5 times in 10m", "")
	s := &Server{}
	got := s.buildFleetReportEvidence(&StatusPayload{})
	if len(got) != 1 || got[0].Class != fleetreport.ClassRestartStorm || !got[0].Attributable {
		t.Fatalf("evidence = %#v", got)
	}
}

// Restarts is a cumulative counter that every cadence kick increments, so a
// healthy hive accrues large values. Observed on a live spoke: all six agents
// idle between scheduled kicks, yet each was reported as a high-severity crash
// loop purely because restarts exceeded a fixed threshold.
func TestFleetReportEvidenceIgnoresCadenceDrivenRestarts(t *testing.T) {
	s := &Server{}
	status := &StatusPayload{Agents: []FrontendAgent{
		{Name: "scanner", Role: "scanner", State: "stopped", Restarts: 43},
		{Name: "quality", Role: "quality", State: "stopped", Restarts: 39},
		{Name: "review", Role: "review", State: "stopped", Restarts: 8},
	}}
	for _, ev := range s.buildFleetReportEvidence(status) {
		if ev.ErrorClass == "agent crash loop" {
			t.Fatalf("healthy cadence cycling reported as a crash loop: %#v", ev)
		}
	}
}

func TestFleetReportEvidenceStillReportsRealCrashState(t *testing.T) {
	s := &Server{}
	status := &StatusPayload{Agents: []FrontendAgent{
		{Name: "scanner", Role: "scanner", State: "crashed", Restarts: 5},
	}}
	var found bool
	for _, ev := range s.buildFleetReportEvidence(status) {
		if ev.ErrorClass == "agent crash loop" {
			found = true
			if ev.Window != 0 {
				t.Fatalf("cumulative restart count must not claim a window: %v", ev.Window)
			}
		}
	}
	if !found {
		t.Fatal("explicit crash state should still produce crash-loop evidence")
	}
}

// #9673: nudgeIfPollingCI's stop-polling nudge count is worth fleet-level
// visibility (an operator can spot an agent that keeps burning turns on CI
// despite the harness telling it to stop), even though the per-kick cap
// keeps a single incident from generating more than one nudge.
func TestFleetReportEvidenceIncludesCIPollNudges(t *testing.T) {
	s := &Server{}
	status := &StatusPayload{Agents: []FrontendAgent{
		{Name: "scanner", Role: "scanner", State: "running", CIPollNudges: 2},
	}}
	var found bool
	for _, ev := range s.buildFleetReportEvidence(status) {
		if ev.ErrorClass == "CI-poll nudge" {
			found = true
			if ev.Count != 2 {
				t.Fatalf("Count = %d, want 2", ev.Count)
			}
			if ev.Severity != "low" {
				t.Fatalf("Severity = %q, want %q", ev.Severity, "low")
			}
		}
	}
	if !found {
		t.Fatal("nonzero CIPollNudges should produce CI-poll nudge evidence")
	}
}

func TestFleetReportEvidenceOmitsCIPollNudgesWhenZero(t *testing.T) {
	s := &Server{}
	status := &StatusPayload{Agents: []FrontendAgent{
		{Name: "scanner", Role: "scanner", State: "running", CIPollNudges: 0},
	}}
	for _, ev := range s.buildFleetReportEvidence(status) {
		if ev.ErrorClass == "CI-poll nudge" {
			t.Fatalf("zero CIPollNudges should not produce evidence: %#v", ev)
		}
	}
}

func TestFleetReportEvidenceIgnoresInferenceBlockedAgents(t *testing.T) {
	fleetreport.SetDefaultTrackerForTest(t, fleetreport.NewRuntimeTracker(nil))
	s := &Server{}
	got := s.buildFleetReportEvidence(&StatusPayload{Agents: []FrontendAgent{
		{Name: "supervisor", Role: "supervisor", StructuredStatus: "blocked", StatusEvidence: "blocked: inference (auth)"},
	}})
	if len(got) != 0 {
		t.Fatalf("inference-blocked agent must not be hive-code evidence: %#v", got)
	}
}

func TestFleetReportEvidenceAgentStallRequiresRecurrence(t *testing.T) {
	cases := []struct {
		name      string
		agent     FrontendAgent
		wantCount int
	}{
		{"single nudge", FrontendAgent{Name: "a", Role: "r", State: "running", StallNudges: 1}, 0},
		{"two nudges", FrontendAgent{Name: "a", Role: "r", State: "running", StallNudges: 2}, 0},
		{"three nudges", FrontendAgent{Name: "a", Role: "r", State: "running", StallNudges: 3}, 3},
		{"all nudges recovered", FrontendAgent{Name: "a", Role: "r", State: "running", StallNudges: 5, StallRecoveries: 5}, 0},
		{"some nudges unrecovered", FrontendAgent{Name: "a", Role: "r", State: "running", StallNudges: 5, StallRecoveries: 2}, 3},
		{"live stalled status", FrontendAgent{Name: "a", Role: "r", State: "running", StructuredStatus: "Stalled: waiting", StallNudges: 0}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			status := &StatusPayload{Agents: []FrontendAgent{tc.agent}}
			got := 0
			for _, ev := range s.buildFleetReportEvidence(status) {
				if ev.ErrorClass == "agent stall" {
					got = ev.Count
					if ev.Window != 0 {
						t.Fatalf("cumulative stall count must not claim a window: %v", ev.Window)
					}
				}
			}
			if got != tc.wantCount {
				t.Fatalf("agent stall count = %d, want %d", got, tc.wantCount)
			}
		})
	}
}
