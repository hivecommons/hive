package crux

import (
	"testing"

	"github.com/hivecommons/hive/crux/agent"
)

func TestCopilotTokenConstants_MatchAgentPackage(t *testing.T) {
	cases := map[string]string{
		CopilotTokenSourceEnv:            agent.CopilotTokenSourceEnv,
		CopilotTokenSourceDashboardLogin: agent.CopilotTokenSourceDashboardLogin,
		CopilotTokenSourceDurableFile:    agent.CopilotTokenSourceDurableFile,
		CopilotTokenSourceCLIConfig:      agent.CopilotTokenSourceCLIConfig,
		CopilotUserTokenPath:             agent.CopilotUserTokenPath,
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("crux constant %q != agent constant %q", got, want)
		}
	}
}

func TestPaneHelpers_MatchAgentPackage(t *testing.T) {
	panes := []string{
		"",
		"Welcome\n\x1b[1mhello\x1b[0m\n> ",
		"Please visit https://github.com/login/device and enter code ABCD-1234",
		"token ghp_0123456789abcdefghijABCDEFGHIJ0123456789 leaked",
		"Do you trust the files in this folder?\n1. Yes, proceed\n2. No, exit",
	}
	for _, p := range panes {
		if got, want := SanitizePaneText(p, 5), agent.SanitizePaneText(p, 5); got != want {
			t.Errorf("SanitizePaneText(%q) = %q, want %q", p, got, want)
		}
		if got, want := RedactPaneTokens(p), agent.RedactPaneTokens(p); got != want {
			t.Errorf("RedactPaneTokens(%q) = %q, want %q", p, got, want)
		}
		for _, backend := range []string{"claude", "copilot", "codex"} {
			if got, want := PaneShowsBlockingPrompt(backend, p), agent.PaneShowsBlockingPrompt(backend, p); got != want {
				t.Errorf("PaneShowsBlockingPrompt(%q, %q) = %v, want %v", backend, p, got, want)
			}
		}
		lines := []string{p}
		if got, want := ClassifyAuthPane(lines), agent.ClassifyAuthPane(lines); got != want {
			t.Errorf("ClassifyAuthPane(%q) = %+v, want %+v", p, got, want)
		}
	}
}
