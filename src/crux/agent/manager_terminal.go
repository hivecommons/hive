package agent

import (
	"time"
)

func (m *Manager) terminalSession() TerminalSession {
	if m.terminal != nil {
		return m.terminal
	}
	return tmuxTerminal{manager: m}
}

// funcTerminal is a compact test fake. Nil funcs return the zero-value result
// or no-op, matching the previous nil seam behavior without shelling out.
type funcTerminal struct {
	capturePane        func(*AgentProcess) string
	captureVisiblePane func(*AgentProcess) string
	sessionAttached    func(*AgentProcess) bool
	sendLiteral        func(*AgentProcess, string)
	sendKeys           func(*AgentProcess, ...string)
	// sendLiteralErr/sendKeysErr take precedence over sendLiteral/sendKeys:
	// when the Err variant is set, the non-error hook is not called. Set one
	// or the other, not both.
	sendLiteralErr           func(*AgentProcess, string) error
	sendKeysErr              func(*AgentProcess, ...string) error
	sleepDuringPromptDismiss func(time.Duration)
	captureFullLog           func(*AgentProcess) (string, error)
	clearHistory             func(*AgentProcess)
}

func (f funcTerminal) CapturePane(agent *AgentProcess) string {
	if f.capturePane != nil {
		return f.capturePane(agent)
	}
	return ""
}

func (f funcTerminal) CaptureVisiblePane(agent *AgentProcess) string {
	if f.captureVisiblePane != nil {
		return f.captureVisiblePane(agent)
	}
	return ""
}

func (f funcTerminal) SessionAttached(agent *AgentProcess) bool {
	if f.sessionAttached != nil {
		return f.sessionAttached(agent)
	}
	return true
}

func (f funcTerminal) SendLiteral(agent *AgentProcess, text string) error {
	if f.sendLiteralErr != nil {
		return f.sendLiteralErr(agent, text)
	}
	if f.sendLiteral != nil {
		f.sendLiteral(agent, text)
	}
	return nil
}

func (f funcTerminal) SendKeys(agent *AgentProcess, keys ...string) error {
	if f.sendKeysErr != nil {
		return f.sendKeysErr(agent, keys...)
	}
	if f.sendKeys != nil {
		f.sendKeys(agent, keys...)
	}
	return nil
}

func (f funcTerminal) SleepDuringPromptDismiss(d time.Duration) {
	if f.sleepDuringPromptDismiss != nil {
		f.sleepDuringPromptDismiss(d)
	}
}

func (f funcTerminal) CaptureFullLog(agent *AgentProcess) (string, error) {
	if f.captureFullLog != nil {
		return f.captureFullLog(agent)
	}
	return "", nil
}

func (f funcTerminal) ClearHistory(agent *AgentProcess) {
	if f.clearHistory != nil {
		f.clearHistory(agent)
	}
}
