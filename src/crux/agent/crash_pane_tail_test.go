package agent

import (
	"strings"
	"testing"
)

func TestCrashPaneTailRedactsLaunchToken(t *testing.T) {
	pane := strings.Join([]string{
		"",
		"$ export HIVE_AGENT=reviewer; /usr/local/bin/claude --model claude-sonnet-5 --mcp-config '{\"mcpServers\":{\"hive-task\":{\"type\":\"http\",\"url\":\"https://hub/api/contribute/mcp?number=0&repo=o%2Fr&task_id=t&token=hive_mcp_v1.secret.part\"}}}'",
		"error: unknown option '--mcp-server'",
		"",
		"hive-reviewer@host:/data/agents/reviewer$ ",
	}, "\n")
	got := crashPaneTail(pane)
	if strings.Contains(got, "hive_mcp_v1.secret.part") {
		t.Fatalf("crash pane tail republishes the launch token: %q", got)
	}
	if !strings.Contains(got, "token=<redacted>\"}}}'") {
		t.Fatalf("redaction did not stop at the closing quote: %q", got)
	}
	if !strings.Contains(got, "error: unknown option '--mcp-server'") {
		t.Fatalf("crash pane tail lost the CLI exit message: %q", got)
	}
	if strings.Contains(got, "\n\n") || strings.HasPrefix(got, "\n") {
		t.Fatalf("crash pane tail keeps blank lines: %q", got)
	}
}

func TestRedactTokenQueryValues(t *testing.T) {
	cases := map[string]string{
		"a?token=abc&x=1":           "a?token=<redacted>&x=1",
		"token=abc":                 "token=<redacted>",
		"token=abc token=def end":   "token=<redacted> token=<redacted> end",
		"no credential here":        "no credential here",
		"'url?x=1&token=t' --flag":  "'url?x=1&token=<redacted>' --flag",
		"COPILOT_GITHUB_TOKEN=ghp1": "COPILOT_GITHUB_TOKEN=ghp1",
	}
	for in, want := range cases {
		if got := redactTokenQueryValues(in); got != want {
			t.Errorf("redactTokenQueryValues(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCrashExitCodeReadsLastStatusAndSkipsTypedLiteral(t *testing.T) {
	typed := `$ /usr/local/bin/claude --model x; echo "` + cliExitMarker + `$?"`
	cases := map[string]string{
		"":                                     "unknown",
		typed:                                  "unknown",
		typed + "\n" + cliExitMarker + "1\n$ ": "1",
		cliExitMarker + "0\n" + typed + "\n" + cliExitMarker + "137\n$ ": "137",
		// An older launch's status above a newer typed line is not this
		// launch's status.
		cliExitMarker + "0\n" + typed + "\n$ ": "unknown",
	}
	for pane, want := range cases {
		if got := crashExitCode(pane); got != want {
			t.Errorf("crashExitCode(%q) = %q, want %q", pane, got, want)
		}
	}
}

func TestWithCLIExitEchoMatchesNoCLIMarker(t *testing.T) {
	line := withCLIExitEcho("claude --model x")
	if !strings.HasSuffix(line, `; echo "`+cliExitMarker+`$?"`) {
		t.Fatalf("launch line = %q", line)
	}
	// What bash prints after the CLI returns must still read as a bare shell.
	if paneHasCLIMarker(cliExitMarker + "1\n$ ") {
		t.Fatalf("exit echo %q matches a CLI pane marker; a dead CLI would look alive", cliExitMarker)
	}
}
