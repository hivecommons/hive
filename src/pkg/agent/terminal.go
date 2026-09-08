package agent

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TerminalSession abstracts every interaction the Manager has with an
// agent's interactive terminal (today: a per-agent tmux session). It was
// extracted from eight ad-hoc func-typed test-seam fields on Manager
// (issue #5636, phase 1) so terminal IO has one named contract instead of
// scattered nilable callbacks.
//
// The production implementation is tmuxTerminal below; tests install a
// funcTerminal (see terminal_seams_test.go) to fake individual methods.
type TerminalSession interface {
	// CapturePane returns the agent's pane content including scrollback
	// (bounded by tmuxCaptureLines), for diff-based output detection.
	CapturePane(agent *AgentProcess) string
	// CaptureVisiblePane returns only the visible pane, no scrollback.
	CaptureVisiblePane(agent *AgentProcess) string
	// SessionAttached reports whether a client is attached to the agent's
	// session. Implementations should fail open (true) when unsure.
	SessionAttached(agent *AgentProcess) bool
	// SendLiteral types text into the agent's pane verbatim.
	SendLiteral(agent *AgentProcess, text string)
	// SendKeys sends key sequences (C-c, C-u, Enter, ...) to the pane.
	SendKeys(agent *AgentProcess, keys ...string)
	// Sleep paces interactive prompt-dismissal loops.
	Sleep(d time.Duration)
	// CaptureFullLog returns the agent's full retained scrollback (bounded
	// by fullLogCaptureLines), joining wrapped lines.
	CaptureFullLog(agent *AgentProcess) (string, error)
	// ClearHistory drops the session's scrollback history; the visible
	// pane is untouched.
	ClearHistory(agent *AgentProcess)
}

// term returns the Manager's terminal, defaulting to the real tmux-backed
// implementation. A zero-value Manager therefore behaves exactly as before
// the TerminalSession extraction: every call reaches tmux.
func (m *Manager) term() TerminalSession {
	if m.terminal != nil {
		return m.terminal
	}
	return tmuxTerminal{m: m}
}

// tmuxTerminal is the production TerminalSession: each method shells out to
// tmux over the agent's per-UID socket via Manager.tmuxCmd, exactly as the
// pre-extraction Manager methods did.
type tmuxTerminal struct {
	m *Manager
}

func (t tmuxTerminal) CapturePane(agent *AgentProcess) string {
	cmd := t.m.tmuxCmd(agent, "capture-pane", "-t", agent.tmuxSession, "-p",
		"-S", fmt.Sprintf("-%d", tmuxCaptureLines))
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func (t tmuxTerminal) CaptureVisiblePane(agent *AgentProcess) string {
	cmd := t.m.tmuxCmd(agent, "capture-pane", "-t", agent.tmuxSession, "-p")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func (t tmuxTerminal) SessionAttached(agent *AgentProcess) bool {
	if agent == nil || agent.tmuxSession == "" {
		return true
	}
	out, err := t.m.tmuxCmd(agent, "display-message", "-p", "-t", agent.tmuxSession, "#{session_attached}").Output()
	if err != nil {
		return true
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return true
	}
	return n > 0
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
func (t tmuxTerminal) SendLiteral(agent *AgentProcess, text string) {
	if err := t.m.tmuxCmd(agent, "send-keys", "-t", agent.tmuxSession, "-l", "--", text).Run(); err != nil && t.m.logger != nil {
		t.m.logger.Warn("tmux send-keys failed; literal text was not typed into the pane",
			"agent", agent.Name, "session", agent.tmuxSession, "runes", len([]rune(text)), "error", err)
	}
}

func (t tmuxTerminal) SendKeys(agent *AgentProcess, keys ...string) {
	args := append([]string{"send-keys", "-t", agent.tmuxSession}, keys...)
	_ = t.m.tmuxCmd(agent, args...).Run()
}

func (t tmuxTerminal) Sleep(d time.Duration) {
	time.Sleep(d)
}

func (t tmuxTerminal) CaptureFullLog(agent *AgentProcess) (string, error) {
	// -S -<n>: start n lines back into history; -E -: through the last visible
	// line; -J: join wrapped lines so copied text is not hard-wrapped at the
	// pane width; -p: print to stdout.
	cmd := t.m.tmuxCmd(agent, "capture-pane", "-t", agent.tmuxSession, "-p", "-J",
		"-S", fmt.Sprintf("-%d", fullLogCaptureLines), "-E", "-")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("capturing pane for %s: %w", agent.Name, err)
	}
	return string(out), nil
}

func (t tmuxTerminal) ClearHistory(agent *AgentProcess) {
	_ = t.m.tmuxCmd(agent, "clear-history", "-t", agent.tmuxSession).Run()
}
