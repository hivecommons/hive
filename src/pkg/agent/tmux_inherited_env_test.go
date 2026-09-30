package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/config"
)

// envWithout returns the current process environment with each of the given
// keys removed. These tests start a REAL tmux server with `append(os.Environ(),
// "KEY=planted-value", ...)`: if the process already carries a real value for
// one of those keys (as it does when this package's tests run inside a live
// hive agent's own tmux pane, per AGENTS.md — the agent's env legitimately
// holds HIVE_ID/HIVE_SHA and, for push-capable agents, a real GitHub token),
// the duplicate-key entry that wins is unspecified: the planted leak the test
// is built to detect and strip might never appear, or a real credential could
// leak into the assertions instead of the inert planted one. Scrubbing first
// makes what actually reaches the tmux server's global environment fully
// test-controlled regardless of the host this runs on (#9820).
func envWithout(keys ...string) []string {
	drop := make(map[string]bool, len(keys))
	for _, k := range keys {
		drop[k] = true
	}
	environ := os.Environ()
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if k, _, ok := strings.Cut(kv, "="); ok && drop[k] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

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
	start.Env = append(envWithout(
		"LINEAR_API_KEY", "LINEAR_CLIENT_SECRET", "LINEAR_WEBHOOK_SECRET",
		"HIVE_GITHUB_TOKEN", "GITHUB_TOKEN", "HIVE_ID", "HIVE_SHA",
	),
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

	// paneEnvTimeout bounds how long a fresh pane gets to write its environment.
	const paneEnvTimeout = 5 * time.Second

	// paneEnv forks a fresh pane in the session and returns the environment
	// that pane's shell was given — the only thing that matters to a CLI.
	paneEnv := func(tag string) map[string]string {
		t.Helper()
		out := filepath.Join(dir, tag+".env")
		if err := exec.Command("tmux", "-L", socket, "new-window", "-t", session, "sh -c 'env > "+out+"'").Run(); err != nil {
			t.Fatalf("new-window: %v", err)
		}
		return testutil.EventuallyValue(t, paneEnvTimeout, func() (map[string]string, bool) {
			data, err := os.ReadFile(out)
			if err != nil || len(data) == 0 {
				return nil, false
			}
			env := map[string]string{}
			for _, line := range strings.Split(string(data), "\n") {
				if k, v, ok := strings.Cut(line, "="); ok {
					env[k] = v
				}
			}
			return env, true
		}, "pane never wrote its environment to %s", out)
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

// With proxy-side GitHub auth injection on (#9586), no agent may hold a
// usable GitHub credential in any env var — push-capable agents included. A
// GH_TOKEN inherited from the hive process would outrank the placeholder
// GITHUB_TOKEN in gh, so it must be removed for every agent. With injection
// off, push-capable agents keep today's behavior.
func TestStripsInheritedGitHubTokens(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"pusher":  {Backend: "claude", Mode: "ISSUES_AND_PRS"},
		"filer":   {Backend: "claude", Mode: "ISSUES_ONLY"},
		"watcher": {Backend: "claude", Mode: "ADVISORY"},
	}, discardLogger(), ProjectContext{ACMMLevel: 3})

	cases := []struct {
		inject string
		agent  string
		want   bool
	}{
		{"", "pusher", false},
		{"false", "pusher", false},
		{"TRUE", "pusher", false},
		{"true", "pusher", true},
		{"", "filer", true},
		{"", "watcher", true},
		{"true", "filer", true},
		{"true", "watcher", true},
	}
	for _, tc := range cases {
		t.Run(tc.agent+"/"+tc.inject, func(t *testing.T) {
			t.Setenv(config.ProxyInjectGHAuthEnv, tc.inject)
			if got := m.stripsInheritedGitHubTokens(m.agents[tc.agent]); got != tc.want {
				t.Errorf("stripsInheritedGitHubTokens(%s) with %s=%q = %v, want %v", tc.agent, config.ProxyInjectGHAuthEnv, tc.inject, got, tc.want)
			}
		})
	}
}

// Drives the real applySessionEnv against a real tmux server started with
// real-looking GitHub tokens in its environment, for a push-capable agent under
// injection: no inherited GitHub token may reach a pane (#9586).
func TestApplySessionEnv_InjectionRemovesInheritedGitHubTokensForPushAgents(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv(config.ProxyInjectGHAuthEnv, config.ProxyInjectGHAuthOnValue)
	socket := fmt.Sprintf("hinject%d", os.Getpid())
	const session = "hive-inject"
	dir := t.TempDir()

	start := exec.Command("tmux", "-L", socket, "new-session", "-d", "-s", session, "-c", dir, "sleep 60")
	start.Env = append(envWithout(append(append([]string{}, githubTokenEnvVars...), "HIVE_ID", "HIVE_SHA")...),
		"GH_TOKEN=ghp_inherited_leaked",
		"GITHUB_TOKEN=ghs_inherited_leaked",
		"GH_ENTERPRISE_TOKEN=ghp_enterprise_leaked",
		"GITHUB_ENTERPRISE_TOKEN=ghp_enterprise_leaked",
	)
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("starting tmux server: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })

	out := filepath.Join(dir, "after.env")
	m := NewManager(map[string]config.AgentConfig{
		"pusher": {Backend: "claude", Mode: "ISSUES_AND_PRS"},
	}, discardLogger(), ProjectContext{ACMMLevel: 3})
	m.mu.Lock()
	agent := m.agents["pusher"]
	agent.UID = 0
	agent.tmuxSocket = socket
	agent.tmuxSession = session
	m.mu.Unlock()

	m.applySessionEnv(agent)

	if err := exec.Command("tmux", "-L", socket, "new-window", "-t", session, "sh -c 'env > "+out+"'").Run(); err != nil {
		t.Fatalf("new-window: %v", err)
	}
	env := testutil.EventuallyValue(t, 5*time.Second, func() (map[string]string, bool) {
		data, err := os.ReadFile(out)
		if err != nil || len(data) == 0 {
			return nil, false
		}
		env := map[string]string{}
		for _, line := range strings.Split(string(data), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				env[k] = v
			}
		}
		return env, true
	}, "pane never wrote its environment to %s", out)
	for _, k := range githubTokenEnvVars {
		if v, ok := env[k]; ok {
			t.Errorf("%s=%q reaches a push-capable agent's pane under proxy injection", k, v)
		}
	}
}
