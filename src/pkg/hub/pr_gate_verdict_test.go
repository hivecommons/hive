package hub

import (
	"net/http"
	"strings"
	"testing"
)

// The hive-posted PR-contents gate (#19038): a gate that cannot be posted
// turns the hive RED when a required context depends on it and AMBER when it
// does not, never overrides a harder red, and carries a remediation hint.
func TestApplyPRGateRedVersusAmber(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		entry     RegistryEntry
		wantState string
		wantCause string
	}{
		{"grant missing, required depends: red", HealthStateGreen, RegistryEntry{Online: true, PRGateCause: "grant-missing", PRGateDependsOn: true}, HealthStateRed, causePRGate},
		{"grant missing, no dependency: amber", HealthStateGreen, RegistryEntry{Online: true, PRGateCause: "grant-missing"}, HealthStateAmber, causePRGate},
		{"write refused, required depends: red", HealthStateGreen, RegistryEntry{Online: true, PRGateCause: "write-refused", PRGateDependsOn: true}, HealthStateRed, causePRGate},
		{"write refused, no dependency: amber", HealthStateGreen, RegistryEntry{Online: true, PRGateCause: "write-refused"}, HealthStateAmber, causePRGate},
		{"amber base with a dependency goes red", HealthStateAmber, RegistryEntry{Online: true, PRGateCause: "grant-missing", PRGateDependsOn: true}, HealthStateRed, causePRGate},
		{"amber base without a dependency stays amber untouched", HealthStateAmber, RegistryEntry{Online: true, PRGateCause: "grant-missing"}, HealthStateAmber, ""},
		{"a harder red keeps its headline", HealthStateRed, RegistryEntry{Online: true, PRGateCause: "grant-missing", PRGateDependsOn: true}, HealthStateRed, ""},
		{"gate can post: nothing changes", HealthStateGreen, RegistryEntry{Online: true}, HealthStateGreen, ""},
		{"unknown cause token is ignored", HealthStateGreen, RegistryEntry{Online: true, PRGateCause: "mystery", PRGateDependsOn: true}, HealthStateGreen, ""},
		{"offline hive is never judged from a stale cause", HealthStateUnknown, RegistryEntry{PRGateCause: "grant-missing", PRGateDependsOn: true}, HealthStateUnknown, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := applyPRGate(HealthVerdict{State: tc.in}, tc.entry)
			if v.State != tc.wantState || v.cause != tc.wantCause {
				t.Fatalf("state=%s cause=%q, want state=%s cause=%q", v.State, v.cause, tc.wantState, tc.wantCause)
			}
			attachRemediation(&v, tc.entry)
			if tc.wantCause == causePRGate {
				if v.Remediation == nil || !strings.Contains(v.Remediation.Action, "Commit statuses") {
					t.Errorf("remediation = %+v, want the owner step naming Commit statuses", v.Remediation)
				}
				if !strings.Contains(v.Reason, "PR-contents gate cannot be posted") {
					t.Errorf("reason = %q", v.Reason)
				}
			}
		})
	}
}

func TestHeartbeatCarriesPRGateCause(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	if rec := postHeartbeat(t, s, `{"hive_id":"h1","pr_gate_cause":"grant-missing","pr_gate_depends_on":true}`); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d", rec.Code)
	}
	e := storedEntry(t, s, "h1")
	if e.PRGateCause != "grant-missing" || !e.PRGateDependsOn {
		t.Fatalf("entry = %+v", e)
	}
	// A recovered hive reports nothing and the cause clears with the next beat.
	if rec := postHeartbeat(t, s, `{"hive_id":"h1"}`); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d", rec.Code)
	}
	if e = storedEntry(t, s, "h1"); e.PRGateCause != "" || e.PRGateDependsOn {
		t.Fatalf("a beat without the cause must clear it: %+v", e)
	}
}
