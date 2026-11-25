package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/config"
)

// translatorSessionAgent is unique to these tests so no other test's tmux
// call can land in the recorded log.
const translatorSessionAgent = "translator-session-retire"

// translatorSessionTmux installs a fake tmux whose show-environment answers
// showEnv (with exit code showExit) and which records every call naming the
// agent's session.
func translatorSessionTmux(t *testing.T, showEnv string, showExit int) func() string {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "tmux.log")
	writeFakeTmux(t, "#!/bin/sh\n"+
		"case \" $* \" in\n"+
		"*' hive-"+translatorSessionAgent+" '*) echo \"$@\" >> '"+logPath+"' ;;\n"+
		"*) exit 0 ;;\n"+
		"esac\n"+
		"case \" $* \" in\n"+
		"*' show-environment '*) printf '%s\\n' '"+showEnv+"'; exit "+strconv.Itoa(showExit)+" ;;\n"+
		"esac\n"+
		"exit 0\n")
	return func() string {
		raw, _ := os.ReadFile(logPath)
		return string(raw)
	}
}

func newTranslatorSessionManager(t *testing.T) (*Manager, *AgentProcess) {
	t.Helper()
	// The session "exists" throughout, so ensureTmuxSession returns before it
	// touches the filesystem; the tests assert only the kill decision.
	orig := tmuxSessionExists
	tmuxSessionExists = func(*Manager, *AgentProcess) bool { return true }
	t.Cleanup(func() { tmuxSessionExists = orig })
	m := NewManager(map[string]config.AgentConfig{translatorSessionAgent: {Backend: "claude"}}, quietTestLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents[translatorSessionAgent]
	m.mu.RUnlock()
	return m, agent
}

func TestRetireTranslatorSessionKillsTranslatorRoutedSession(t *testing.T) {
	tmuxLog := translatorSessionTmux(t, "ANTHROPIC_BASE_URL="+inferenceTranslatorBaseURL, 0)
	m, agent := newTranslatorSessionManager(t)

	m.mu.Lock()
	err := m.retireTranslatorSessionLocked(agent)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got := tmuxLog(); !strings.Contains(got, "kill-session -t hive-"+translatorSessionAgent) {
		t.Fatalf("a session whose shell routes to the translator was kept for a non-routed launch; tmux saw:\n%s", got)
	}
}

func TestRetireTranslatorSessionKeepsOtherSessions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		showEnv string
		exit    int
	}{
		{name: "variable absent", showEnv: "unknown variable: ANTHROPIC_BASE_URL", exit: 1},
		{name: "variable removed", showEnv: "-ANTHROPIC_BASE_URL", exit: 0},
		{name: "operator endpoint", showEnv: "ANTHROPIC_BASE_URL=https://gateway.example.com", exit: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmuxLog := translatorSessionTmux(t, tc.showEnv, tc.exit)
			m, agent := newTranslatorSessionManager(t)

			m.mu.Lock()
			err := m.retireTranslatorSessionLocked(agent)
			m.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if got := tmuxLog(); strings.Contains(got, "kill-session") {
				t.Fatalf("session killed though its shell does not route to the translator; tmux saw:\n%s", got)
			}
		})
	}
}
