package commands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newTestRoot builds the root command against discard writers. The TUI tests
// never reach a network call, so unlike execute() they need no httptest server.
func newTestRoot() (*bytes.Buffer, *bytes.Buffer, *cobra.Command) {
	var stdout, stderr bytes.Buffer
	return &stdout, &stderr, NewRootCommand(strings.NewReader(""), &stdout, &stderr)
}

// TestTUICommandIsRegistered pins that `hivectl tui` exists on the root.
// Registration is a single AddCommand line in NewRootCommand and is exactly the
// kind of thing a merge conflict resolution drops silently — the package still
// compiles, the command just stops existing.
func TestTUICommandIsRegistered(t *testing.T) {
	_, _, root := newTestRoot()

	for _, command := range root.Commands() {
		if command.Name() == "tui" {
			if command.Short == "" {
				t.Fatal("tui command has no Short description; it would render blank in `hivectl --help`")
			}
			return
		}
	}
	t.Fatal("no `tui` subcommand registered on the hivectl root")
}

// TestTUICommandRejectsArguments checks the argsNone() guard.
//
// This is also the safest end-to-end reach into the command: cobra validates
// positional args BEFORE calling RunE, so this exercises the real registered
// command through root.Execute() without ever entering tui.Run() — which would
// try to take over a terminal the test runner does not have.
func TestTUICommandRejectsArguments(t *testing.T) {
	_, _, root := newTestRoot()
	root.SetArgs([]string{"tui", "unexpected"})

	err := root.Execute()
	if err == nil {
		t.Fatal("`hivectl tui unexpected` was accepted; it takes no positional arguments")
	}
	if got := ExitCode(err); got != ExitUsage {
		t.Fatalf("exit code = %d, want %d (usage)", got, ExitUsage)
	}
}

// TestTUICommandHelpNamesTheQuitKeys guards the one piece of documentation an
// operator needs before they start a full-screen program: how to get back out.
// A TUI that captures the terminal without saying how to exit is a support
// ticket, especially over SSH.
func TestTUICommandHelpNamesTheQuitKeys(t *testing.T) {
	stdout, _, root := newTestRoot()
	root.SetArgs([]string{"tui", "--help"})

	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	help := stdout.String()
	for _, key := range []string{"q", "ctrl+c"} {
		if !strings.Contains(help, key) {
			t.Fatalf("`hivectl tui --help` does not mention %q:\n%s", key, help)
		}
	}
}
