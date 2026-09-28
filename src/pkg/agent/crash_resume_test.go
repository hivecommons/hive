package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestCrashResumeFlag_ScopedBackendsOnly(t *testing.T) {
	cases := []struct {
		backend string
		uid     int
		want    string
	}{
		{"claude", 2009, "--continue"},
		{"claude", 0, "--continue"}, // cwd-scoped: safe even in a shared home
		{"copilot", 2009, "--continue"},
		{"copilot", 0, ""}, // shared $HOME: "most recent" could be another agent's
		{"codex", 2009, ""},
		{"gemini", 2009, ""},
		{"goose", 2009, ""},
		{bobBackend, 2009, ""},
	}
	for _, tc := range cases {
		if got := crashResumeFlag(tc.backend, tc.uid); got != tc.want {
			t.Errorf("crashResumeFlag(%q, %d) = %q, want %q", tc.backend, tc.uid, got, tc.want)
		}
	}
}

func TestCrashResumeFlag_CopilotSkipsForcedSharedHome(t *testing.T) {
	t.Setenv("HIVE_SHARED_AGENT_HOME", "1")
	if got := crashResumeFlag("copilot", 2009); got != "" {
		t.Fatalf("copilot with forced shared home must not resume, got %q", got)
	}
}

func TestShouldResumeAfterCrash(t *testing.T) {
	now := time.Now()
	old := now.Add(-10 * time.Minute)
	young := now.Add(-30 * time.Second)

	mk := func(started *time.Time, launchCmd string) *AgentProcess {
		return &AgentProcess{Name: "scanner", UID: 2009, StartedAt: started, Config: config.AgentConfig{Backend: "copilot", LaunchCmd: launchCmd}}
	}

	if ok, _ := shouldResumeAfterCrash(mk(&old, ""), "copilot", now); !ok {
		t.Error("long-lived copilot run must resume")
	}
	if ok, why := shouldResumeAfterCrash(mk(&young, ""), "copilot", now); ok || !strings.Contains(why, "too short") {
		t.Errorf("run shorter than crashResumeMinUptime must start fresh, got ok=%v why=%q", ok, why)
	}
	if ok, why := shouldResumeAfterCrash(mk(nil, ""), "copilot", now); ok || why != "no previous run" {
		t.Errorf("no StartedAt must start fresh, got ok=%v why=%q", ok, why)
	}
	if ok, why := shouldResumeAfterCrash(mk(&old, "my-cli --weird"), "copilot", now); ok || why != "custom launch_cmd" {
		t.Errorf("custom launch_cmd must start fresh, got ok=%v why=%q", ok, why)
	}
	if ok, _ := shouldResumeAfterCrash(mk(&old, ""), "codex", now); ok {
		t.Error("backend without a scoped resume flag must start fresh")
	}
	t.Setenv(crashResumeEnv, "0")
	if ok, why := shouldResumeAfterCrash(mk(&old, ""), "copilot", now); ok || !strings.Contains(why, crashResumeEnv) {
		t.Errorf("%s=0 must disable resume, got ok=%v why=%q", crashResumeEnv, ok, why)
	}
}

// RestartAfterCrash must record the crash reason and arm the resume intent
// consumed by the next launch (fields checked directly; the launch itself
// needs tmux and is covered by the live pane tests).
func TestRestartAfterCrash_ArmsResumeIntent(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	agent := m.agents["scanner"]
	started := time.Now().Add(-time.Hour)
	agent.StartedAt = &started
	agent.UID = 2009
	m.mu.Unlock()

	// Not running, so restartWithReason relaunches through launchInTmux, which
	// fails fast here without tmux. The intent must already be armed by then.
	_ = m.RestartAfterCrash(context.Background(), "scanner")

	m.mu.RLock()
	defer m.mu.RUnlock()
	if agent.LastRestartReason != crashRestartReason {
		t.Errorf("LastRestartReason = %q, want %q", agent.LastRestartReason, crashRestartReason)
	}
}

func TestRestartAfterCrash_FreshWhenRunTooShort(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	agent := m.agents["scanner"]
	started := time.Now().Add(-5 * time.Second)
	agent.StartedAt = &started
	m.mu.Unlock()

	m.mu.Lock()
	backend := agent.effectiveBackend()
	resume, _ := shouldResumeAfterCrash(agent, backend, time.Now())
	m.mu.Unlock()
	if resume {
		t.Fatal("a run that died within crashResumeMinUptime must not be resumed")
	}
}
