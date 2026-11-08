package api

import "testing"

func TestGatePathsAndCodes(t *testing.T) {
	if PathGates != "/v1/gates" || GatePath("gate_a") != "/v1/gates/gate_a" ||
		GateAnswerPath("gate_a") != "/v1/gates/gate_a/answer" || GateWaitPath("gate_a") != "/v1/gates/gate_a/wait" {
		t.Fatalf("paths: %s %s %s %s", PathGates, GatePath("gate_a"), GateAnswerPath("gate_a"), GateWaitPath("gate_a"))
	}
	seen := map[string]bool{CodeRuntime: true, CodeBadRequest: true, CodeForbidden: true, CodeConflict: true, CodeTooLarge: true}
	for _, code := range []string{CodeGateContextTooLarge, CodeGateInvalidChoice, CodeGateSelfAnswer, CodeGateClosed} {
		if seen[code] {
			t.Fatalf("gate code %q overloads an existing code", code)
		}
		seen[code] = true
	}
}
