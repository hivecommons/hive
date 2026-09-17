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
