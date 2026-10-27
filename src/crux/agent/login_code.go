package agent

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// maxLoginCodeLen bounds an operator-supplied authorization code. Real ones are
// well under this (a Google OAuth code is ~75 characters, a GitHub device code
// 8); the cap exists so this can never become a channel for pasting a script.
const maxLoginCodeLen = 512

// ErrLoginCodeUnprintable is returned for a code carrying anything that is not
// a printable, non-space character.
var ErrLoginCodeUnprintable = errors.New("authorization code contains control characters or whitespace")

// validateLoginCode checks an operator-supplied code before it is typed into an
// agent's pane.
//
// SECURITY: this is the whole reason SubmitLoginCode is a narrow endpoint
// rather than a general "send text to a pane" one. The pane is an interactive
// shell hosting a CLI; a value containing a newline or carriage return would be
// SUBMITTED at that point and everything after it typed as a fresh line, which
// turns "paste my login code" into "run this". Rejecting every non-printable
// and every space leaves a code — which is what an OAuth or device code is —
// and nothing that can carry a second command.
//
// It rejects rather than sanitises deliberately: silently stripping a character
// would submit a code the operator did not supply, and a login that fails for
// an unexplained reason is worse than one that refuses with a reason.
func validateLoginCode(code string) error {
	if code == "" {
		return errors.New("authorization code is empty")
	}
	if len(code) > maxLoginCodeLen {
		return fmt.Errorf("authorization code too long (%d bytes, max %d)", len(code), maxLoginCodeLen)
	}
	for _, r := range code {
		if !unicode.IsPrint(r) || unicode.IsSpace(r) {
			return ErrLoginCodeUnprintable
		}
	}
	return nil
}

// SubmitLoginCode types an operator-supplied authorization code into the
// agent's pane and submits it with a single Enter.
//
// WHY THIS EXISTS. The dashboard terminal is ttyd/xterm.js over tmux, and it is
// as hostile to pasting IN as it is to copying OUT — the defect
// handleAgentTerminalURLs already works around in the other direction (#5188).
// An operator completing an OAuth hand-off has to paste a code back into the
// pane, and the browser terminal mangles it. Without this the only routes left
// are a host shell and `tmux send-keys`, which is not an answer for an operator
// whose interface is a web dashboard.
//
// It grants no new capability: the dashboard already publishes a WRITABLE ttyd
// terminal for these panes (`ttyd -W`), so anyone who can reach this can
// already type anything into the agent. This is strictly narrower — one
// validated, printable, whitespace-free token followed by exactly one Enter.
//
// ONE Enter, not tmuxSendEntersForAgent's three. That helper repeats
// deliberately, as insurance that a launch line actually ran; here the pane is
// known to be sitting at a prompt, and extra Enters stop being insurance and
// become answers to whatever the CLI asks next — the codex "1. Update now"
// failure documented on that helper.
//
// The code is never logged, and never returned in an error.
func (m *Manager) SubmitLoginCode(name, code string) error {
	if err := validateLoginCode(strings.TrimSpace(code)); err != nil {
		return err
	}
	code = strings.TrimSpace(code)

	m.mu.RLock()
	agent := m.agents[name]
	m.mu.RUnlock()
	if agent == nil {
		return fmt.Errorf("agent %s not found", name)
	}
	if !m.tmuxSessionExistsForAgent(agent) {
		return fmt.Errorf("agent %s has no terminal session to submit a code to", name)
	}

	m.tmuxSendLiteralForAgent(agent, code)
	// One Enter, sent directly rather than through tmuxSendEntersForAgent.
	_ = m.tmuxCmd(agent, "send-keys", "-t", agent.tmuxSession, "Enter").Run()
	return nil
}
