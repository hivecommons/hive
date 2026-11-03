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

// Agent counters (restarts, stall nudges, CI-poll nudges, crash state) are
// no longer synthesised into bare "hive-code defect" evidence here (#13470):
// the agent manager and watchdog feed the runtime tracker at the moment they
// observe the condition, with kick/pane context, so the self-report carries
// something an owner can act on.
func TestFleetReportEvidenceNoLongerSynthesisesAgentCounters(t *testing.T) {
	fleetreport.SetDefaultTrackerForTest(t, fleetreport.NewRuntimeTracker(nil))
	s := &Server{}
	status := &StatusPayload{Agents: []FrontendAgent{
		{Name: "scanner", Role: "scanner", State: "stopped", Restarts: 43, StallNudges: 5, StallRecoveries: 0, CIPollNudges: 2},
		{Name: "quality", Role: "quality", State: "failed", Restarts: 39, StructuredStatus: "Stalled: waiting"},
	}}
	if got := s.buildFleetReportEvidence(status); len(got) != 0 {
		t.Fatalf("dashboard synthesised bare-counter evidence: %#v", got)
	}
}

func TestFleetReportEvidenceCarriesTrackerStallWithContext(t *testing.T) {
	fleetreport.SetDefaultTrackerForTest(t, fleetreport.NewRuntimeTracker(nil))
	for i := 0; i < fleetreport.AgentStallNudgeStreak; i++ {
		fleetreport.Observe(fleetreport.ClassAgentStall, "scanner", "agent stalled after kick: 3 continue nudge(s) sent without the pane moving", "agent=scanner lane=scanner")
	}
	s := &Server{}
	got := s.buildFleetReportEvidence(&StatusPayload{Agents: []FrontendAgent{{Name: "scanner", Role: "scanner", State: "running", StallNudges: 3}}})
	if len(got) != 1 || got[0].Class != fleetreport.ClassAgentStall || got[0].Agent != "scanner" || got[0].Diagnostics == "" {
		t.Fatalf("evidence = %#v", got)
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
