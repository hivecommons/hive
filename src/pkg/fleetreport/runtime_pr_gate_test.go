package fleetreport

import (
	"testing"

	"github.com/hivecommons/hive/pkg/prgate"
)

// The PR gate's escalation line files the existing statuses-permission class,
// and its ordinary visibility lines do not: a missing grant is shown on the
// dashboard and hub first, and only filed once merges are actually stalled.
func TestPRGateEscalationFilesTheStatusesPermissionClass(t *testing.T) {
	if got := LogFedClass(prgate.EscalationMessage); got != ClassGitHubAppStatusesPermission {
		t.Fatalf("escalation line classified as %q, want %q", got, ClassGitHubAppStatusesPermission)
	}
	for _, msg := range []string{
		"pr-gate: the installation has no Commit statuses write grant, the gate is forced off",
		"pr-gate: status refused with 403, the installation has no Commit statuses write grant",
		"pr-gate: status posted",
	} {
		if got := LogFedClass(msg); got == ClassGitHubAppStatusesPermission {
			t.Errorf("%q must not file the class", msg)
		}
	}
}

// One escalating pass must state the line as often as the class needs to file.
func TestPRGateEscalationRepeatsMeetTheClassThreshold(t *testing.T) {
	spec := Classes[ClassGitHubAppStatusesPermission]
	if prgate.EscalationRepeats < spec.MinCount {
		t.Fatalf("EscalationRepeats = %d, the class files at %d", prgate.EscalationRepeats, spec.MinCount)
	}
}
