package panesignal

import (
	"strings"
	"testing"
)

func TestOMPWorkingAndReadiness(t *testing.T) {
	for _, pane := range []string{" ⎋ Reading skill improvement guidance", "⠧ 4m > running"} {
		if !ShowsOMPMidTurn(pane) || !ShowsAgentWorking(pane) {
			t.Errorf("OMP mid-turn pane must remain busy: %q", pane)
		}
	}
	for _, pane := range []string{OMPStatusLineMarker + " ready", OMPStatusLogoMarker + " ready"} {
		if !HasCLIMarker(pane) || ShowsOMPMidTurn(pane) {
			t.Errorf("OMP idle status must be recognized without reading as busy: %q", pane)
		}
	}
	for _, pane := range []string{
		"Setup step 1 of 3\n❯",
		"Paste the authorization code (or full redirect URL)\n❯",
		"Setup step 1 of 3\n❯" + strings.Repeat("\n", ompSetupWizardTailLines),
		"Setup step 1 of 3\n" + strings.Repeat("\n", ompSetupWizardTailLines) + "❯",
	} {
		if !ShowsOMPSetupWizard(pane) || ShowsInputPrompt(pane) {
			t.Errorf("OMP setup wizard must not accept a kick: %q", pane)
		}
	}
	oldWizard := "Setup step 1 of 3\n" + strings.Repeat("finished\n", ompSetupWizardTailLines) + "❯"
	if ShowsOMPSetupWizard(oldWizard) {
		t.Error("setup history outside the visible tail must not suppress readiness")
	}
	if ShowsOMPSetupWizard("❯") || ShowsOMPMidTurn("") {
		t.Error("an idle or empty pane must not read as setup or active work")
	}
}
