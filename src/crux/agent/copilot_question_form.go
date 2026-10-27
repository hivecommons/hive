package agent

import "time"

const copilotQuestionFormEscapeDelay = 500 * time.Millisecond

// dismissCopilotQuestionFormForKick cancels Copilot CLI's ask_user form before
// a kick is delivered. It sends Escape once, re-checks, and sends a second
// Escape only if the form is still visible.
func (m *Manager) dismissCopilotQuestionFormForKick(agent *AgentProcess) {
	m.mu.Lock()
	if current, ok := m.agents[agent.Name]; ok && current == agent {
		m.markCopilotQuestionFormLocked(agent, time.Now())
	}
	m.mu.Unlock()

	m.logger.Warn(copilotQuestionFormMessage,
		"agent", agent.Name,
		"condition", copilotQuestionFormCondition)

	for i := 0; i < 2; i++ {
		m.tmuxSendKeysForAgent(agent, "Escape")
		m.sleepDuringPromptDismiss(copilotQuestionFormEscapeDelay)
		if !paneShowsCopilotQuestionForm(m.captureVisiblePaneForAgent(agent)) {
			return
		}
	}
}
