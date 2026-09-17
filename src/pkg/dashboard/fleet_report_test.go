package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/inferencehealth"
)

func TestFleetReportEvidenceIncludesRuntimeGatewayFaults(t *testing.T) {
	SetGatewayHealthProvider(func() []inferencehealth.GatewayStatus {
		return []inferencehealth.GatewayStatus{{Name: "litellm", ErrorClass: inferencehealth.ClassAuth, HTTPStatus: 403}}
	})
	t.Cleanup(func() { SetGatewayHealthProvider(nil) })
	s := &Server{}
	got := s.buildFleetReportEvidence(&StatusPayload{})
	if len(got) != 1 {
		t.Fatalf("evidence len=%d, want 1: %#v", len(got), got)
	}
	if got[0].Component != "backend-auth" || got[0].ErrorClass != "auth http 403" || !got[0].Attributable {
		t.Fatalf("unexpected evidence: %#v", got[0])
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
