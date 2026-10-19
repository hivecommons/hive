package agent

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TerminalSession is the nil-safe boundary around tmux pane/session I/O.
type TerminalSession interface {
	// CapturePane returns the agent's pane content including scrollback
	// (bounded by tmuxCaptureLines), for diff-based output detection. Wrapped
	// display lines are joined so substring detectors see the original output.
	CapturePane(agent *AgentProcess) string
	CaptureVisiblePane(agent *AgentProcess) string
	SessionAttached(agent *AgentProcess) bool
	SendLiteral(agent *AgentProcess, text string) error
	SendKeys(agent *AgentProcess, keys ...string) error
	SleepDuringPromptDismiss(time.Duration)
	CaptureFullLog(agent *AgentProcess) (string, error)
	ClearHistory(agent *AgentProcess)
}

type tmuxTerminal struct {
	manager *Manager
}

func (t tmuxTerminal) CapturePane(agent *AgentProcess) string {
	cmd := t.manager.tmuxCmd(agent, "capture-pane", "-t", agent.tmuxSession, "-p", "-J",
		"-S", fmt.Sprintf("-%d", tmuxCaptureLines))
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func (t tmuxTerminal) CaptureVisiblePane(agent *AgentProcess) string {
	cmd := t.manager.tmuxCmd(agent, "capture-pane", "-t", agent.tmuxSession, "-p")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// SessionAttached reports whether a human may currently be interacting with
// the agent's tmux session.
//
// Fails OPEN on every uncertainty (no session, tmux error, unparseable
// output): callers use this to decide whether to type, and the safe answer
// when we cannot tell is "assume someone is there".
func (t tmuxTerminal) SessionAttached(agent *AgentProcess) bool {
	if agent == nil || agent.tmuxSession == "" {
		return true
	}
	out, err := t.manager.tmuxCmd(agent, "list-clients", "-t", agent.tmuxSession, "-F", "#{client_activity}").Output()
	if err != nil {
		return true
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		// No clients at all — the unambiguous detached case.
		return false
	}
	cutoff := time.Now().Add(-attachedClientIdleGrace)
	for _, field := range fields {
		secs, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			return true
		}
		if time.Unix(secs, 0).After(cutoff) {
			return true
		}
	}
	// Clients exist but every one of them has been silent past the grace, so
	// they are abandoned terminals rather than an operator mid-keystroke.
	return false
}

// SendLiteral types text into the pane verbatim.
//
// The text is passed after a "--" end-of-options marker. tmux parses the
// arguments of send-keys with getopt, so without the marker any text that
// begins with "-" is read as a flag and the whole command is rejected
// ("unknown flag"), typing nothing. SendKick delivers a kick as 400-rune
// chunks, and a chunk boundary that lands just before a hyphen — inside
// "onboard-ai-platform", "hold-gated", "[ONB-2651]", or a markdown bullet —
// produced a chunk starting with "-", so one 400-character slice of the kick
// silently vanished. Observed live (2026-09-05): the held-PR snapshot in a
// quality kick arrived as "onboard-ai" followed by text from the next chunk,
// and the agent stood down on the corrupted list. A failed send is logged
// rather than dropped so the next occurrence is diagnosable from the hive log.
func (t tmuxTerminal) SendLiteral(agent *AgentProcess, text string) error {
	err := t.manager.tmuxCmd(agent, "send-keys", "-t", agent.tmuxSession, "-l", "--", text).Run()
	if err != nil && t.manager.logger != nil {
		t.manager.logger.Warn("tmux send-keys failed; literal text was not typed into the pane",
			"agent", agent.Name, "session", agent.tmuxSession, "runes", len([]rune(text)), "error", err)
	}
	return err
}

func (t tmuxTerminal) SendKeys(agent *AgentProcess, keys ...string) error {
	args := append([]string{"send-keys", "-t", agent.tmuxSession}, keys...)
	return t.manager.tmuxCmd(agent, args...).Run()
}

func (t tmuxTerminal) CaptureFullLog(agent *AgentProcess) (string, error) {
	cmd := t.manager.tmuxCmd(agent, "capture-pane", "-t", agent.tmuxSession, "-p", "-J",
		"-S", fmt.Sprintf("-%d", fullLogCaptureLines), "-E", "-")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("capturing pane for %s: %w", agent.Name, err)
	}
	return string(out), nil
}

func (t tmuxTerminal) ClearHistory(agent *AgentProcess) {
	_ = t.manager.tmuxCmd(agent, "clear-history", "-t", agent.tmuxSession).Run()
}
