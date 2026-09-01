package tui

import (
	"errors"
	"testing"
)

// The attach preflight communicates failures through two typed errors so the
// app (and these tests) can branch on cause without matching prose. Their
// Error/Unwrap contracts were previously uncovered: a reworded message or a
// dropped Unwrap would have shipped silently, breaking errors.Is chains and
// the operator-facing footer text.

func TestTmuxNotFoundErrorMessageAndUnwrap(t *testing.T) {
	cause := errors.New("exec: \"tmux\": executable file not found in $PATH")
	err := &tmuxNotFoundError{err: cause}

	want := "tmux is not installed or is not available in PATH"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, cause) {
		t.Error("errors.Is(err, cause) = false, want true — Unwrap must expose the LookPath error")
	}

	var typed *tmuxNotFoundError
	if !errors.As(error(err), &typed) {
		t.Error("errors.As failed to recover *tmuxNotFoundError")
	}
}

func TestTmuxSessionMissingErrorMessageVariants(t *testing.T) {
	cause := errors.New("exit status 1")

	t.Run("without detail", func(t *testing.T) {
		err := &tmuxSessionMissingError{session: "hive-scout", err: cause}
		want := `tmux session "hive-scout" is unavailable`
		if got := err.Error(); got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
	})

	t.Run("with detail", func(t *testing.T) {
		err := &tmuxSessionMissingError{
			session: "hive-scout",
			detail:  "no server running on /tmp/tmux-1001/default",
			err:     cause,
		}
		want := `tmux session "hive-scout" is unavailable: no server running on /tmp/tmux-1001/default`
		if got := err.Error(); got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
	})

	t.Run("unwrap exposes tmux exit error", func(t *testing.T) {
		err := &tmuxSessionMissingError{session: "hive-scout", err: cause}
		if !errors.Is(err, cause) {
			t.Error("errors.Is(err, cause) = false, want true — Unwrap must expose the has-session error")
		}
	})
}
