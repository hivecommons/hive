package tui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/charmbracelet/x/term"
	"github.com/gorilla/websocket"

	"github.com/hivecommons/hive/pkg/tui/client"
)

// remoteAttach bridges the operator's real terminal to an agent's tmux
// session over the dashboard's /terminal websocket (see client.DialTerminal).
//
// It implements tea.ExecCommand, and that choice is the design: Bubble Tea's
// Exec releases the terminal — leaves the alternate screen, restores the
// operator's scrollback — exactly as it does for the local `tmux attach`
// path, then hands stdin/stdout to Run. The remote terminal is therefore not
// re-rendered inside a Bubble Tea view through a terminal emulator; the
// operator's OWN terminal does the emulation, driven by the raw byte stream
// the remote tmux emits, which is the same fidelity `tmux attach` has. When
// the websocket closes — tmux detach (prefix + d), the session ending, or
// the connection dropping — Run returns, and the TUI resumes and refreshes,
// identically to returning from a local attach.
type remoteAttach struct {
	agent   string
	session *client.TerminalSession
	// target is the dashboard base URL, for the banner. NEVER the full dial
	// URL: that carries ?token=, and the banner is written to a scrollback
	// that outlives the session.
	target string
	// viaFallback records that a loopback dashboard was expected to have a
	// local session and did not — the Podman topology. The banner says so,
	// which is what keeps the fallback from being the silent kind the issue
	// rules out.
	viaFallback bool

	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

func newRemoteAttach(agent string, session *client.TerminalSession, target string, viaFallback bool) *remoteAttach {
	return &remoteAttach{agent: agent, session: session, target: target, viaFallback: viaFallback}
}

// SetStdin, SetStdout and SetStderr are Bubble Tea's side of tea.ExecCommand:
// it injects the program's own streams before calling Run.
func (r *remoteAttach) SetStdin(in io.Reader)   { r.stdin = in }
func (r *remoteAttach) SetStdout(out io.Writer) { r.stdout = out }
func (r *remoteAttach) SetStderr(err io.Writer) { r.stderr = err }

// Run drives the attached session until it ends. It runs with the terminal
// released by Bubble Tea: cooked mode, main screen. Its own job is to
//
//  1. say where the operator is about to be attached (and how to leave),
//  2. put the local terminal into raw mode so every keystroke — including
//     ctrl+c, which must reach the AGENT, not detach the bridge — travels,
//  3. size the remote to the local terminal, initially and on every resize,
//  4. pump bytes both ways until the websocket closes.
//
// A clean websocket close is a detach, not an error: it is the one way every
// normal ending arrives (prefix + d, the agent's session exiting, the hive
// restarting the session), so it maps to nil and a quiet return to the TUI.
func (r *remoteAttach) Run() error {
	defer func() { _ = r.session.Close() }()

	// The banner prints BEFORE raw mode so it lands as an ordinary line in
	// the operator's scrollback. It is the answer to "which session am I
	// looking at" — load-bearing for the fallback case, cheap for the rest.
	how := "remote attach via dashboard"
	if r.viaFallback {
		how = "no local tmux session; attaching via dashboard"
	}
	fmt.Fprintf(r.stdout, "hivectl: %s %s — %s — detach: tmux prefix + d\n",
		how, r.target, tmuxSessionFor(r.agent))

	cols, rows := 80, 24
	stdinFile, _ := r.stdin.(*os.File)
	if stdinFile != nil && term.IsTerminal(stdinFile.Fd()) {
		if w, h, err := term.GetSize(stdinFile.Fd()); err == nil && w > 0 && h > 0 {
			cols, rows = w, h
		}
		state, err := term.MakeRaw(stdinFile.Fd())
		if err != nil {
			return fmt.Errorf("raw mode for remote attach: %w", err)
		}
		defer func() { _ = term.Restore(stdinFile.Fd(), state) }()
	}

	if err := r.session.Start(cols, rows); err != nil {
		return fmt.Errorf("start remote terminal: %w", err)
	}

	// done tells the input pump and the resize watcher to stop. It is closed
	// exactly once, when the output loop ends — the websocket closing is the
	// single source of "this attach is over".
	done := make(chan struct{})

	// wg tracks only the goroutines Run can PROVE will exit promptly once
	// done closes: the poll-based file pump (bounded by its poll interval)
	// and the select-based resize watcher. Waiting for them matters —
	// returning while a goroutine still reads the terminal would race the
	// resumed Bubble Tea program for the operator's next keystroke and
	// swallow it. A blocking-read pump (non-file stdin, or a platform
	// without poll) cannot be interrupted, so it is deliberately NOT waited
	// for; on those inputs there is no shared terminal to race over.
	var wg sync.WaitGroup
	if stdinFile != nil && stdinPollingSupported {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pumpFile(done, stdinFile, r.session.SendInput)
		}()
	} else {
		go pumpBlocking(done, r.stdin, r.session.SendInput)
	}
	if stdinFile != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			watchResize(done, stdinFile, r.session.Resize)
		}()
	}

	var readErr error
	for {
		payload, err := r.session.ReadOutput()
		if err != nil {
			readErr = err
			break
		}
		if _, err := r.stdout.Write(payload); err != nil {
			readErr = err
			break
		}
	}
	close(done)
	wg.Wait()

	if isDetach(readErr) {
		return nil
	}
	return fmt.Errorf("remote attach ended: %w", readErr)
}

// isDetach separates "the session ended" from "the transport failed".
//
// Close codes 1000/1001 are ttyd hanging up cleanly (detach, session exit,
// ttyd's own SIGHUP-and-respawn), 1005 is a close frame with no status —
// still a deliberate close. EOF is the handshake-less version of the same
// thing. An abnormal closure (1006) or any other transport error is a broken
// connection while attached, which the operator needs to hear about: their
// last keystrokes may not have arrived.
func isDetach(err error) bool {
	if err == nil || errors.Is(err, io.EOF) {
		return true
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		switch closeErr.Code {
		case websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived:
			return true
		}
	}
	return false
}

// pumpBlocking forwards operator keystrokes with plain blocking reads, for
// inputs that are not poll-able files (tests, pipes, non-unix platforms).
// The buffer is small and per-read: input is human-scale, and forwarding
// immediately (rather than line-buffering) is what makes the remote session
// interactive.
func pumpBlocking(done <-chan struct{}, in io.Reader, send func([]byte) error) {
	buf := make([]byte, 4096)
	for {
		select {
		case <-done:
			return
		default:
		}
		n, err := in.Read(buf)
		if n > 0 {
			// A closed `done` means the websocket is gone; the send will fail
			// and end the pump, which is the correct order — bytes read before
			// the close still get their delivery attempt.
			if send(buf[:n]) != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
