package crux_test

import (
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent"
)

func TestCopilotTokenConstants_MatchAgentPackage(t *testing.T) {
	cases := map[string]string{
		crux.CopilotTokenSourceEnv:            agent.CopilotTokenSourceEnv,
		crux.CopilotTokenSourceDashboardLogin: agent.CopilotTokenSourceDashboardLogin,
		crux.CopilotTokenSourceDurableFile:    agent.CopilotTokenSourceDurableFile,
		crux.CopilotTokenSourceCLIConfig:      agent.CopilotTokenSourceCLIConfig,
		crux.CopilotUserTokenPath:             agent.CopilotUserTokenPath,
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("crux constant %q != agent constant %q", got, want)
		}
	}
}

func TestRedactPaneTokens_MatchesAgentPackage(t *testing.T) {
	panes := []string{
		"",
		"Welcome\n\x1b[1mhello\x1b[0m\n> ",
		"Please visit https://github.com/login/device and enter code ABCD-1234",
		"token ghp_0123456789abcdefghijABCDEFGHIJ0123456789 leaked",
	}
	for _, p := range panes {
		if got, want := crux.RedactPaneTokens(p), agent.RedactPaneTokens(p); got != want {
			t.Errorf("RedactPaneTokens(%q) = %q, want %q", p, got, want)
		}
	}
}
