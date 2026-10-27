package agent

import "testing"

// TestDismissInferencePrompts_PressEnter covers the "Press Enter to continue"
// branch: the pane shows that text, then flips to a ready marker so the loop
// returns.
func TestDismissInferencePrompts_PressEnter(t *testing.T) {
	m, agent, script := newDismissPromptHarness(t, "covpe", ": Press Enter to continue", 1)
	runDismissPromptScript(t, m, agent, script, []string{"Enter"})
}

// TestDismissInferencePrompts_FixWithSettingsError covers the settings-error
// "Fix with Claude" navigation branch (double Down past Fix/Exit to Continue).
func TestDismissInferencePrompts_FixWith(t *testing.T) {
	m, agent, script := newDismissPromptHarness(t, "covfw", ": Enter to confirm\n❯ Fix with Claude", 3)
	runDismissPromptScript(t, m, agent, script, []string{"Down", "Down", "Enter"})
}
