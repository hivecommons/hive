package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hivectl/commands"
)

// These tests exercise main() itself — the root-command wiring and the
// error/exit-code contract scripts depend on — not the subcommands, which are
// tested in pkg/hivectl/commands. The happy path runs main() in-process so its
// statements are visible to the coverage profile; the error lanes re-exec the
// test binary (the same pattern as cmd/hive-backup and cmd/apiproxy) so
// os.Exit is observable.

// argsSep separates re-exec'd argv values; arguments never contain control
// characters in these tests.
const argsSep = "\x1f"

// TestHelperRunMain is not a real test: it becomes the hivectl process when
// re-exec'd by runMainHelper. os.Exit inside main() terminates the subprocess,
// and the parent asserts on the exit code — the same signal callers scripting
// hivectl key off.
func TestHelperRunMain(t *testing.T) {
	if os.Getenv("HIVECTL_TEST_RUN_MAIN") != "1" {
		t.Skip("helper process for exit-code tests")
	}
	args := []string{"hivectl"}
	if raw := os.Getenv("HIVECTL_TEST_ARGS"); raw != "" {
		args = append(args, strings.Split(raw, argsSep)...)
	}
	os.Args = args
	main()
}

// runMainHelper re-execs this test binary as `hivectl args...`, returning the
// exit code and combined output.
func runMainHelper(t *testing.T, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", "TestHelperRunMain")
	cmd.Env = append(os.Environ(),
		"HIVECTL_TEST_RUN_MAIN=1",
		"HIVECTL_TEST_ARGS="+strings.Join(args, argsSep),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), string(out)
	}
	t.Fatalf("re-exec failed to run: %v\n%s", err, out)
	return -1, ""
}

// The happy path: `hivectl --help` succeeds, so main() returns instead of
// calling os.Exit. Run in-process with stdout redirected so the help text is
// asserted rather than dumped into the test log. main() runs on the test
// goroutine and returns before the pipe is read, so reassigning the os.Stdout
// variable (unlike cmd/apiproxy's concurrent server) is race-free here.
func TestMainHelpSucceedsInProcess(t *testing.T) {
	oldArgs, oldStdout := os.Args, os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Args = []string{"hivectl", "--help"}
	os.Stdout = w
	t.Cleanup(func() {
		os.Args = oldArgs
		os.Stdout = oldStdout
	})

	main()

	os.Stdout = oldStdout
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading captured stdout: %v", err)
	}
	if !strings.Contains(string(out), "hivectl") {
		t.Errorf("help output does not mention hivectl:\n%s", out)
	}
}

// A flag-parse failure is a user mistake: the root command wraps it in a
// usageError, so main must exit with ExitUsage and prefix the message with
// "Error:" on stderr.
func TestMainExitsUsageOnUnknownFlag(t *testing.T) {
	code, out := runMainHelper(t, "--definitely-not-a-flag")
	if code != commands.ExitUsage {
		t.Errorf("exit code = %d, want ExitUsage (%d)\noutput:\n%s", code, commands.ExitUsage, out)
	}
	if !strings.Contains(out, "Error:") {
		t.Errorf("stderr missing Error: prefix:\n%s", out)
	}
}

// An unknown subcommand is also rejected non-zero with the Error: prefix.
// The exact code is pinned so a cobra upgrade that reclassifies the error
// (and silently changes scripted callers' behavior) fails this test.
func TestMainExitsNonZeroOnUnknownCommand(t *testing.T) {
	code, out := runMainHelper(t, "definitely-not-a-command")
	if code != commands.ExitFailure {
		t.Errorf("exit code = %d, want ExitFailure (%d)\noutput:\n%s", code, commands.ExitFailure, out)
	}
	if !strings.Contains(out, "Error:") {
		t.Errorf("stderr missing Error: prefix:\n%s", out)
	}
}

// A dashboard that cannot be reached must map to ExitConnection so operators
// can distinguish "hive down" from "bad request" in scripts. Port 1 on
// loopback is reserved and never listening.
func TestMainExitsConnectionWhenServerUnreachable(t *testing.T) {
	code, out := runMainHelper(t, "--server", "http://127.0.0.1:1", "--timeout", "2s", "system", "status")
	if code != commands.ExitConnection {
		t.Errorf("exit code = %d, want ExitConnection (%d)\noutput:\n%s", code, commands.ExitConnection, out)
	}
	if !strings.Contains(out, "Error:") {
		t.Errorf("stderr missing Error: prefix:\n%s", out)
	}
}
