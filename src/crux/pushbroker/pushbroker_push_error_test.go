package pushbroker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"
)

// pushFailureRun drives Run to a failed push whose git output is out, and
// returns the error Run reports.
func pushFailureRun(t *testing.T, token, out string) error {
	t.Helper()
	git := &scriptedGit{
		replies: map[string]string{
			"rev-parse HEAD": "abc123\n",
			"diff-tree --root --no-commit-id --no-renames --name-only -r -z HEAD": "pkg/safe.go\x00",
			"show --text --no-textconv --format= --no-ext-diff HEAD":              "+// harmless\n",
		},
		fails:   map[string]error{"rev-parse --verify refs/remotes/origin/work": errors.New("unknown revision")},
		pushErr: errors.New("exit status 128"),
		pushOut: out,
	}
	res, err := (&Broker{
		Workspace: fakeGitWorkspace(t), Branch: "work", Repo: "hivecommons/hive",
		Minter: fakeMinter{token}, Runner: git, Logger: slog.New(slog.DiscardHandler),
	}).Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded, want a push failure")
	}
	if res.Pushed {
		t.Fatal("Pushed=true after a failed push")
	}
	if res.Error != err.Error() {
		t.Fatalf("Result.Error = %q, want it to match the returned error %q", res.Error, err)
	}
	return err
}

// #19924: a refused push must carry git's own diagnostics, not only
// "exit status 128", so a scope refusal reads differently from a network
// failure or a rejected ref.
func TestRunPushFailureCarriesGitOutput(t *testing.T) {
	out := "remote: Forbidden\nfatal: unable to access 'https://forge.example.org/hivecommons/hive.git/': The requested URL returned error: 403\n"
	err := pushFailureRun(t, "ghs_pushbrokertoken123", out)
	msg := err.Error()
	for _, want := range []string{"git push failed", "exit status 128", "remote: Forbidden", "The requested URL returned error: 403"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("push error %q does not carry %q", msg, want)
		}
	}
}

// The carried output must never leak the minted token or credentials git
// echoed back in a remote URL.
func TestRunPushFailureScrubsCredentials(t *testing.T) {
	const token = "opaque-forge-token-value-42"
	out := strings.Join([]string{
		"fatal: unable to access 'https://x-access-token:" + token + "@forge.example.org/o/r.git/': 403",
		"remote: invalid credentials for https://bob:hunter2@forge.example.org/o/r.git",
		"remote: echoed ghs_abcdefghijklmnopqrstuvwxyz0123",
		"remote: token " + token + " rejected",
	}, "\n")
	err := pushFailureRun(t, token, out)
	msg := err.Error()
	for _, secret := range []string{token, "hunter2", "bob:", "x-access-token:", "ghs_abcdefghijklmnopqrstuvwxyz0123"} {
		if strings.Contains(msg, secret) {
			t.Fatalf("push error %q leaks %q", msg, secret)
		}
	}
	if !strings.Contains(msg, "https://[REDACTED]@forge.example.org/o/r.git") {
		t.Fatalf("push error %q lost the scrubbed URL context", msg)
	}
}

func TestPushFailureDetailBoundsOutput(t *testing.T) {
	var lines []string
	for i := 0; i < pushFailureTailLines*3; i++ {
		lines = append(lines, "Counting objects: progress line")
	}
	lines = append(lines, "", "remote: Forbidden", "\r", "error: 403")
	got := pushFailureDetail([]byte(strings.Join(lines, "\n")), "")
	if n := strings.Count(got, " | ") + 1; n != pushFailureTailLines {
		t.Fatalf("detail kept %d lines, want %d: %q", n, pushFailureTailLines, got)
	}
	if !strings.HasSuffix(got, "remote: Forbidden | error: 403") {
		t.Fatalf("detail %q does not end with git's cause", got)
	}

	long := strings.Repeat("é", pushFailureTailBytes)
	got = pushFailureDetail([]byte(long), "")
	if len(got) > pushFailureTailBytes+len("...") {
		t.Fatalf("detail is %d bytes, want at most %d", len(got), pushFailureTailBytes+len("..."))
	}
	if !strings.HasPrefix(got, "...") || !utf8.ValidString(got) {
		t.Fatalf("truncated detail %q is not a valid, marked tail", got)
	}

	if got := pushFailureDetail([]byte(" \n\r\n"), "tok"); got != "" {
		t.Fatalf("blank output produced detail %q, want empty", got)
	}
	if got := pushFailureDetail(nil, ""); got != "" {
		t.Fatalf("nil output produced detail %q, want empty", got)
	}
}
