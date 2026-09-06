package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// A tmux server inherits the environment of the process that starts it, and
// copies that GLOBAL environment into every pane it forks. The hive process
// legitimately holds credentials in its environment (the Linear work-source
// key, the Linear OAuth app secrets, the full App installation token), so an
// agent's tmux server carries them globally — and `set-environment -u`, which
// the creation-time strip used, only deletes the SESSION entry: the global
// value shows straight through. Observed live: an agent with the sanctioned
// OAuth LINEAR_ACCESS_TOKEN in its session env used the operator's personal
// LINEAR_API_KEY from the global env instead, and every issue it filed was
// created by the operator.
//
// This drives the REAL applySessionEnv against a REAL tmux server started
// with leaked credentials in its environment, then forks a pane and reads
// what that pane actually sees. It first proves the leak is real on this
// tmux (the control), then that the strip closes it and the sanctioned pair
// survives.
func TestApplySessionEnv_RemovesInheritedCredentialsFromPanes(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	socket := fmt.Sprintf("hleak%d", os.Getpid())
	const session = "hive-leak"
	dir := t.TempDir()

	// A dedicated server, so ITS global environment is exactly this process
	// env plus the planted credentials — the package-wide test server was
	// started elsewhere with whatever env that had.
	start := exec.Command("tmux", "-L", socket, "new-session", "-d", "-s", session, "-c", dir, "sleep 60")
	start.Env = append(os.Environ(),
		"LINEAR_API_KEY=lin_api_leaked",
		"LINEAR_CLIENT_SECRET=client_secret_leaked",
		"LINEAR_WEBHOOK_SECRET=webhook_secret_leaked",
		"HIVE_GITHUB_TOKEN=ghs_full_token_leaked",
		"GITHUB_TOKEN=ghs_inherited_leaked",
	)
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("starting tmux server: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })

	// paneEnv forks a fresh pane in the session and returns the environment
	// that pane's shell was given — the only thing that matters to a CLI.
	paneEnv := func(tag string) map[string]string {
		t.Helper()
		out := filepath.Join(dir, tag+".env")
		if err := exec.Command("tmux", "-L", socket, "new-window", "-t", session, "sh -c 'env > "+out+"'").Run(); err != nil {
			t.Fatalf("new-window: %v", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			data, err := os.ReadFile(out)
			if err == nil && len(data) > 0 {
				env := map[string]string{}
				for _, line := range strings.Split(string(data), "\n") {
					if k, v, ok := strings.Cut(line, "="); ok {
						env[k] = v
					}
				}
				return env
			}
			if time.Now().After(deadline) {
				t.Fatalf("pane never wrote its environment to %s", out)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// CONTROL: before the strip, the planted credentials reach a pane. If this
	// ever fails, the leak this test guards against no longer exists on this
	// tmux and the assertions below prove nothing.
	before := paneEnv("before")
	if before["LINEAR_API_KEY"] != "lin_api_leaked" || before["LINEAR_CLIENT_SECRET"] != "client_secret_leaked" {
		t.Fatalf("control: expected the planted credentials in a fresh pane, got LINEAR_API_KEY=%q LINEAR_CLIENT_SECRET=%q", before["LINEAR_API_KEY"], before["LINEAR_CLIENT_SECRET"])
	}

	// An ISSUES_ONLY agent on this socket whose sanctioned Linear credential
	// is the OAuth token. It may hold LINEAR_ACCESS_TOKEN and nothing else
	// Linear-related; it cannot push, so no GitHub token either.
	m := NewManager(map[string]config.AgentConfig{
		"leak": {Backend: "claude", Mode: "ISSUES_ONLY"},
	}, discardLogger(), ProjectContext{ACMMLevel: 3})
	m.SetLinearCredentialResolver(func() LinearCredential { return LinearCredential{AccessToken: "lin_oauth_sanctioned"} })
	m.mu.Lock()
	agent := m.agents["leak"]
	agent.UID = 0
	agent.tmuxSocket = socket
	agent.tmuxSession = session
	m.mu.Unlock()

	m.applySessionEnv(agent)

	after := paneEnv("after")
	for _, k := range []string{"LINEAR_API_KEY", "LINEAR_CLIENT_SECRET", "LINEAR_WEBHOOK_SECRET", "HIVE_GITHUB_TOKEN", "GITHUB_TOKEN"} {
		if v, ok := after[k]; ok {
			t.Errorf("%s=%q still reaches a pane after applySessionEnv — the inherited credential was not removed", k, v)
		}
	}
	if after["LINEAR_ACCESS_TOKEN"] != "lin_oauth_sanctioned" {
		t.Errorf("the sanctioned LINEAR_ACCESS_TOKEN must survive the strip, got %q", after["LINEAR_ACCESS_TOKEN"])
	}

	// A downgrade below the ISSUES_ONLY floor must take the sanctioned token
	// away again on the next refresh tick — with a removal that also hides
	// any globally inherited value, not just the session entry.
	m.mu.Lock()
	agent.Config.Mode = "ADVISORY"
	m.mu.Unlock()
	for _, args := range m.linearRefreshTmuxArgs(agent) {
		if err := m.tmuxCmd(agent, args...).Run(); err != nil {
			t.Fatalf("refresh-tick %v: %v", args, err)
		}
	}
	downgraded := paneEnv("downgraded")
	for _, k := range []string{"LINEAR_ACCESS_TOKEN", "LINEAR_API_KEY"} {
		if v, ok := downgraded[k]; ok {
			t.Errorf("%s=%q reaches a pane after the downgrade tick", k, v)
		}
	}
}
