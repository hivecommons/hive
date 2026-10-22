// Package tmux is the single place outside pkg/agent's own manager that may
// spawn the tmux binary (hivecommons/hive#13536, src/docs/design/crux.md).
// Every Hive consumer that needs a tmux operation calls one of the functions
// here; the Crux boundary ratchet in internal/testutil fails any other
// package that names the "tmux" binary directly.
//
// The surface is deliberately the operations callers use today. Add a
// function when a call site moves behind the funnel, not ahead of one.
package tmux

import (
	"context"
	"os/exec"
)

// Binary is the tmux executable name. It is resolved through PATH by
// exec.Command, which keeps it as Args[0] so a constructed command reads as
// exactly `tmux <verb> ...` in logs and tests.
const Binary = "tmux"

// LookPath reports where tmux resolves on PATH, or the exec.LookPath error
// when it does not.
func LookPath() (string, error) {
	return exec.LookPath(Binary)
}

// Command constructs, but does not run, `tmux args...`.
func Command(args ...string) *exec.Cmd {
	return exec.Command(Binary, args...)
}

// CommandContext is Command bound to ctx.
func CommandContext(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, Binary, args...)
}

// Attach constructs `tmux attach -t <session>` for an interactive attach.
func Attach(session string) *exec.Cmd {
	return Command("attach", "-t", session)
}

// HasSession runs `tmux has-session -t <session>` and returns tmux's
// combined output alongside the exit error. tmux uses the same non-zero
// result for a missing server and a missing session; the output carries the
// distinction when the platform's tmux supplies it.
func HasSession(session string) ([]byte, error) {
	return Command("has-session", "-t", session).CombinedOutput()
}
