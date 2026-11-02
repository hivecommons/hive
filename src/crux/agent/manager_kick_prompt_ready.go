package agent

import (
	"fmt"
	"time"
)

// handleClaudePermissionDialogForKick restores only the agent's expected bypass
// posture. Operator launch overrides without bypass remain blocked.
func (m *Manager) handleClaudePermissionDialogForKick(agent *AgentProcess, backend, title string, answers *int) string {
	if !agentExpectsSkipPermissions(agent, backend) || *answers >= claudePermissionDialogAnswerAttempts {
		reason := claudePermissionDialogNotReadyReason(title)
		m.logger.Warn("kick undeliverable: CLI blocked on a Claude Code permission dialog",
			"agent", agent.Name, "reason", reason, "answers", *answers,
			"skip_permissions_expected", agentExpectsSkipPermissions(agent, backend))
		return reason
	}
	*answers++
	m.answerClaudePermissionDialogForKick(agent, title, *answers)
	return ""
}

func (m *Manager) checkKickPromptReadyLocked(agent *AgentProcess, visible, trigger string) error {
	menuFooter := paneTUIMenuFooter(effectiveBackend(agent), visible)
	dialogTitle, hasDialog := claudePermissionDialog(visible)
	if !paneShowsInputPrompt(visible) || paneShowsConsentScreen(visible) || paneShowsAgentWorking(visible) || paneShowsQuestionForm(visible) || menuFooter != "" {
		reason := "pane not at a ready input prompt"
		if menuFooter != "" {
			reason = tuiMenuNotReadyReason(menuFooter)
		}
		if hasDialog {
			reason = claudePermissionDialogNotReadyReason(dialogTitle)
		}
		m.markKickUndeliverableLocked(agent, time.Now(), reason)
		m.logger.Warn("kick delivery skipped: CLI is not at a ready prompt; refusing to type kick into shell",
			"agent", agent.Name, "trigger", trigger, "has_cli_marker", paneHasCLIMarker(visible),
			"consent_screen", paneShowsConsentScreen(visible), "working", paneShowsAgentWorking(visible),
			"question_form", paneShowsQuestionForm(visible), "permission_dialog", dialogTitle, "menu_footer", menuFooter)
		if hasDialog {
			return m.kickNotReadyErrorLocked(agent, time.Now(), fmt.Errorf("agent %s %s", agent.Name, reason))
		}
		if menuFooter != "" {
			return fmt.Errorf("agent %s pane not at a ready input prompt: %s", agent.Name, reason)
		}
		return m.kickNotReadyErrorLocked(agent, time.Now(), fmt.Errorf("agent %s pane not at a ready input prompt", agent.Name))
	}
	return nil
}
