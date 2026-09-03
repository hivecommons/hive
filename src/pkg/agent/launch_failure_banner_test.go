package agent

// Tests for the loud-launch-failure fix: a launch that launchInTmux aborts
// before typing anything (missing backend binary, bob with no API key) must
// announce itself IN the agent's tmux pane instead of leaving a silent bare
// shell. Observed live: a hosted hive's "quality" agent switched to backend
// "bob" and restarted showed only an idle bash prompt in ttyd, with the only
// explanation in the hive log.
//
// backendBinary resolves through exec.LookPath, so both branches are driven
// deterministically here by pointing PATH at a temp dir: empty for the
// missing-binary branch, containing a stub `bob` executable for the
// missing-key branch. No tmux server is needed — the pane writes are
// best-effort and the composed banner is recorded on the agent.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestLaunchFailureBanner(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want []string
	}{
		{
			name: "plain message is prefixed and single-quoted",
			msg:  "backend bob did not launch",
			want: []string{"echo '" + launchFailurePrefix + "backend bob did not launch'"},
		},
		{
			name: "single quotes and newlines cannot break the quoting",
			msg:  "it's broken\nsecond line",
			want: []string{"echo '", "it s broken second line'"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := launchFailureBanner(tc.msg)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("launchFailureBanner(%q) = %q, want it to contain %q", tc.msg, got, w)
				}
			}
			if strings.Count(got, "'") != 2 {
				t.Errorf("launchFailureBanner(%q) = %q, want exactly the two wrapping single quotes", tc.msg, got)
			}
		})
	}
}

// TestMissingBinaryLaunchAnnouncesInPane pins the loud half of the
// missing-binary branch: the pane banner must name the attempted backend and
// that its binary was not found, and the agent must be parked failed with the
// reason surfaced to the dashboard via LastError.
func TestMissingBinaryLaunchAnnouncesInPane(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir: no bob anywhere on PATH

	m := &Manager{logger: discardLogger()}
	m.SetBobAPIKeyResolver(func() string { return "key-present-but-irrelevant" })

	agent := &AgentProcess{
		Name:   "quality",
		Config: config.AgentConfig{Backend: bobBackend},
	}
	if err := m.launchInTmux(t.Context(), agent); err != nil {
		t.Fatalf("launchInTmux returned error, want nil so one agent never aborts the fleet: %v", err)
	}
	if agent.State != StateFailed {
		t.Errorf("State = %q, want %q", agent.State, StateFailed)
	}
	banner := agent.lastLaunchFailureBanner
	if banner == "" {
		t.Fatal("lastLaunchFailureBanner is empty — the aborted launch left a silent bare shell, which is the exact bug this fix exists for")
	}
	for _, want := range []string{launchFailurePrefix, "bob", "not found", "hive image"} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner %q does not contain %q", banner, want)
		}
	}
	if agent.LastError == "" || !strings.Contains(agent.LastError, "not found") {
		t.Errorf("LastError = %q, want the missing-binary reason so the dashboard can show it", agent.LastError)
	}
}

func TestLaunchFailureMessage_UnknownBackendDoesNotSuggestImageUpgrade(t *testing.T) {
	m := &Manager{logger: discardLogger()}
	msg := m.backendLaunchFailureMessage("openrouter", os.ErrNotExist)
	for _, notWant := range []string{"hive image", "upgrade"} {
		if strings.Contains(msg, notWant) {
			t.Errorf("unknown-backend message %q must not suggest %q", msg, notWant)
		}
	}
	for _, want := range []string{"openrouter", "not a supported CLI", "configured model gateway"} {
		if !strings.Contains(msg, want) {
			t.Errorf("unknown-backend message %q should contain %q", msg, want)
		}
	}
}

func TestLaunchFailureMessage_ConfiguredGatewayNamesClaudeCLI(t *testing.T) {
	m := &Manager{logger: discardLogger()}
	m.SetGatewayBackendChecker(func(backend string) bool {
		return strings.EqualFold(backend, "openrouter")
	})
	msg := m.backendLaunchFailureMessage("OpenRouter", os.ErrNotExist)
	for _, want := range []string{"OpenRouter", "claude CLI", "hive image"} {
		if !strings.Contains(msg, want) {
			t.Errorf("gateway missing-cli message %q should contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "OpenRouter CLI") {
		t.Errorf("gateway missing-cli message %q must name claude, not an OpenRouter CLI", msg)
	}
}

// TestBobMissingKeyLaunchAnnouncesInPane pins the loud half of the missing-key
// branch: with the bob binary present but no key configured, the pane banner
// must name the missing credential (BOBSHELL_API_KEY — the name, never a
// value) and where to save it.
func TestBobMissingKeyLaunchAnnouncesInPane(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "bob")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing stub bob binary: %v", err)
	}
	t.Setenv("PATH", dir)

	m := &Manager{logger: discardLogger()}
	m.SetBobAPIKeyResolver(func() string { return "" })

	agent := &AgentProcess{
		Name:   "quality",
		Config: config.AgentConfig{Backend: bobBackend},
	}
	if err := m.launchInTmux(t.Context(), agent); err != nil {
		t.Fatalf("launchInTmux returned error, want nil so one agent never aborts the fleet: %v", err)
	}
	if agent.State != StateFailed {
		t.Errorf("State = %q, want %q", agent.State, StateFailed)
	}
	if !agent.awaitingBobKey {
		t.Error("awaitingBobKey = false, want true — the key-save relaunch cannot find this agent without the marker")
	}
	banner := agent.lastLaunchFailureBanner
	if banner == "" {
		t.Fatal("lastLaunchFailureBanner is empty — the aborted launch left a silent bare shell, which is the exact bug this fix exists for")
	}
	for _, want := range []string{launchFailurePrefix, config.BobAPIKeyEnvVar, "API key", "Governor"} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner %q does not contain %q", banner, want)
		}
	}
	if agent.LastError == "" || !strings.Contains(agent.LastError, config.BobAPIKeyEnvVar) {
		t.Errorf("LastError = %q, want the missing-key reason so the dashboard can show it", agent.LastError)
	}
}
