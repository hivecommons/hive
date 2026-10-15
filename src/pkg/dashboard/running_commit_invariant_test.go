package dashboard

import (
	"strings"
	"testing"
)

func TestEnforceRunningCommitInvariant(t *testing.T) {
	const running = "ea1cd50aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cases := []struct {
		name       string
		in         UpgradeAttemptStatus
		running    string
		wantSuper  bool
		wantUnchng bool
	}{
		{"succeeded mismatch", UpgradeAttemptStatus{State: upgradeAttemptSucceeded, Target: "d09cc5f"}, running, true, false},
		{"succeeded match", UpgradeAttemptStatus{State: upgradeAttemptSucceeded, Target: "ea1cd50"}, running, false, true},
		{"already superseded", UpgradeAttemptStatus{State: upgradeAttemptSucceeded, Target: "d09cc5f", Superseded: true, Detail: "x"}, running, true, true},
		{"failed", UpgradeAttemptStatus{State: upgradeAttemptFailed, Target: "d09cc5f"}, running, false, true},
		{"in progress", UpgradeAttemptStatus{State: upgradeAttemptInProgress, Target: "d09cc5f"}, running, false, true},
		{"empty running", UpgradeAttemptStatus{State: upgradeAttemptSucceeded, Target: "d09cc5f"}, "", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := enforceRunningCommitInvariant(c.in, c.running)
			if got.Superseded != c.wantSuper {
				t.Fatalf("Superseded = %v, want %v", got.Superseded, c.wantSuper)
			}
			if c.wantUnchng {
				if got != c.in {
					t.Fatalf("expected unchanged, got %+v", got)
				}
				return
			}
			if got.RunningCommit != "ea1cd50" || !strings.HasPrefix(got.Detail, "The hive is running ea1cd50.") {
				t.Fatalf("running commit should lead: %+v", got)
			}
			if got.Target != c.in.Target || got.State != c.in.State {
				t.Fatalf("target/state must be preserved: %+v", got)
			}
		})
	}
}
