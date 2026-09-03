package tui

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hivecommons/hive/pkg/tui/client"
)

const tmuxSessionPrefix = "hive-"

// tmuxNotFoundError distinguishes an unavailable local tmux binary from a
// session lookup failure. Both are operator-facing footer errors, but keeping
// the causes typed lets callers and tests reason about them without matching
// prose.
type tmuxNotFoundError struct {
	err error
}

func (e *tmuxNotFoundError) Error() string {
	return "tmux is not installed or is not available in PATH"
}

func (e *tmuxNotFoundError) Unwrap() error { return e.err }

// tmuxSessionMissingError is returned when tmux cannot find the selected
// agent's session during the preflight. tmux uses the same non-zero result for
// a missing server and a missing session; both mean the local attach target is
// unavailable, and the diagnostic text preserves that distinction for the
// operator when tmux supplies it.
type tmuxSessionMissingError struct {
	session string
	detail  string
	err     error
}

func (e *tmuxSessionMissingError) Error() string {
	message := fmt.Sprintf("tmux session %q is unavailable", e.session)
	if e.detail != "" {
		message += ": " + e.detail
	}
	return message
}

func (e *tmuxSessionMissingError) Unwrap() error { return e.err }

// attachReadyMsg is the result of preparing an attach target — a proven local
// tmux command, an already-dialled remote terminal session, or the error that
// prevented both. The preparation is a regular tea.Cmd so it does not block
// the TUI's update loop; only a proven target is handed to Bubble Tea's exec
// machinery, which suspends the terminal.
//
// Exactly one of cmd and remote is non-nil on success. They are separate
// fields rather than one interface because they suspend the TUI through
// different Bubble Tea primitives: a *exec.Cmd goes to tea.ExecProcess, a
// remote bridge to tea.Exec.
type attachReadyMsg struct {
	cmd    *exec.Cmd
	remote *remoteAttach
	err    error
}

// attachDoneMsg is delivered after Bubble Tea has restored the terminal. A
// nil error means tmux exited normally; either way the app refreshes the fleet
// because its state may have changed while the operator was attached.
type attachDoneMsg struct {
	err error
}

func tmuxSessionFor(agent string) string {
	return tmuxSessionPrefix + agent
}

// attachCmdFor constructs, but does not run, the interactive command for an
// agent. exec.Command resolves tmux through PATH while retaining "tmux" as
// Args[0], so the command is both directly executable and easy to audit as
// exactly `tmux attach -t hive-<agent>`.
func attachCmdFor(agent string) (*exec.Cmd, error) {
	if _, err := exec.LookPath("tmux"); err != nil {
		return nil, &tmuxNotFoundError{err: err}
	}
	return exec.Command("tmux", "attach", "-t", tmuxSessionFor(agent)), nil
}

// attachDialTimeout bounds the remote dial that runs before the TUI suspends.
// Same reasoning as preflightTimeout's relation to the request timeout: this
// runs while the operator is looking at a frame that appears to be doing
// nothing, so it must fail faster than the client's own 5s handshake bound.
const attachDialTimeout = 3 * time.Second

// prepareAttach resolves where the selected agent's terminal actually is and
// proves the attach can start, before Bubble Tea releases the terminal.
// Running blindly would briefly leave the alternate screen just to print an
// expected failure, then redraw the TUI; preparing in-band keeps every
// failure in the footer instead.
//
// THE ROUTING RULE (#5644). A non-loopback HIVE_DASHBOARD_URL means the hive
// is somewhere else, so the local tmux server — even if one exists — cannot
// be holding this hive's sessions: the attach goes through the dashboard's
// authenticated /terminal proxy, and a local tmux is never consulted. A
// loopback URL keeps the existing local exec as the co-located fast path, but
// a missing local session no longer dead-ends: on the recommended Podman
// install the hive is on this machine yet its tmux sessions live inside the
// container, owned by other UIDs (bin/hive-podman-setup.sh), so the proxy is
// tried next. That fallback is never silent — the remote bridge prints which
// hive it attached through before the first byte of session output, so the
// operator always knows which session they are looking at.
func prepareAttach(agent string, api *client.Client) tea.Cmd {
	return func() tea.Msg {
		if !dashboardIsLoopback(api.BaseURL()) {
			return prepareRemoteAttach(agent, api, nil)
		}

		cmd, localErr := localAttachTarget(agent)
		if localErr == nil {
			return attachReadyMsg{cmd: cmd}
		}
		return prepareRemoteAttach(agent, api, localErr)
	}
}

// localAttachTarget is the co-located fast path's preflight: resolve tmux,
// prove the session exists, and hand back the command that will attach to it.
func localAttachTarget(agent string) (*exec.Cmd, error) {
	cmd, err := attachCmdFor(agent)
	if err != nil {
		return nil, err
	}
	session := tmuxSessionFor(agent)
	check := exec.Command(cmd.Path, "has-session", "-t", session)
	output, err := check.CombinedOutput()
	if err != nil {
		// tmux diagnostics are one or two short stderr lines. Folding them
		// keeps the message a single line even when a platform emits both.
		detail := strings.Join(strings.Fields(string(output)), " ")
		return nil, &tmuxSessionMissingError{
			session: session,
			detail:  detail,
			err:     err,
		}
	}
	return cmd, nil
}

// prepareRemoteAttach dials the dashboard's terminal proxy for the agent's
// session. localErr carries why the co-located fast path was not taken (nil
// when it was never applicable); on a dial failure it is folded into the
// error so the footer explains the whole journey, not just its last hop —
// "session missing" alone would send a Podman operator hunting a local tmux
// problem when the actionable failure is the dashboard connection.
func prepareRemoteAttach(agent string, api *client.Client, localErr error) tea.Msg {
	ctx, cancel := context.WithTimeout(context.Background(), attachDialTimeout)
	defer cancel()

	session, err := api.DialTerminal(ctx, tmuxSessionFor(agent))
	if err != nil {
		if localErr != nil {
			// Both wrapped: errors.As must still find the typed tmux error
			// (attach_test.go relies on it) AND the dial error's APIError, so
			// the footer's IsUnauthorized/IsForbidden classification keeps
			// working through the fold.
			return attachReadyMsg{err: fmt.Errorf("%w; dashboard terminal: %w", localErr, err)}
		}
		return attachReadyMsg{err: err}
	}
	return attachReadyMsg{remote: newRemoteAttach(agent, session, api.BaseURL(), localErr != nil)}
}

// attachFailureStatus renders an attach error for the footer, giving the two
// authorization outcomes the same treatment the other actions already give
// them (see handleKickResult): a 403 is a working credential whose role does
// not cover terminals, and the only useful thing to say is whose does; a 401
// is no usable credential at all, so it names what to set — both variables,
// for the same reason preflight's unauthorizedHelp names both. Everything
// else reports verbatim.
func attachFailureStatus(err error) string {
	switch {
	case client.IsForbidden(err):
		return "Attach failed: owner access required"
	case client.IsUnauthorized(err):
		return "Attach failed: authentication required — set " + client.TokenEnv + " or " + client.CookieEnv
	}
	return "Attach failed: " + err.Error()
}

// dashboardIsLoopback reports whether the dashboard URL points at this
// machine, which is what decides local-exec-first versus proxy-only above.
//
// An unparseable or hostless URL counts as loopback: that is the historical
// local-only behavior, and the remote path would only turn a clear local
// error into an obscure dial error against a URL that was never valid.
func dashboardIsLoopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return true
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
