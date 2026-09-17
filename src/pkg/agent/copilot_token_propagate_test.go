package agent

import (
	"reflect"
	"sort"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// copilotAgent is a running copilot agent with a tmux session, which is the
// only shape propagateCopilotToken considers.
func copilotAgent(name, session, backendAuth string, state ProcessState) *AgentProcess {
	return &AgentProcess{
		Name:        name,
		Config:      config.AgentConfig{Backend: "copilot"},
		tmuxSession: session,
		State:       state,
		BackendAuth: BackendAuthState{Status: backendAuth},
	}
}

func planFor(t *testing.T, m *Manager) map[string]copilotTokenPropagation {
	t.Helper()
	m.mu.RLock()
	plan := m.planCopilotTokenPropagationLocked()
	m.mu.RUnlock()
	byName := make(map[string]copilotTokenPropagation, len(plan))
	for _, p := range plan {
		byName[p.Name] = p
	}
	return byName
}

// A real token is SET into the session; no token is REMOVED with -r, not -u.
// -u would leave the hive process env's COPILOT_GITHUB_TOKEN showing through
// to the next pane, so a dashboard logout would not actually log anything out.
func TestCopilotTokenRefreshTmuxArgs(t *testing.T) {
	got := copilotTokenRefreshTmuxArgs("hive-scanner", "gho_fresh")
	want := []string{"set-environment", "-t", "hive-scanner", "COPILOT_GITHUB_TOKEN", "gho_fresh"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("set: got %v, want %v", got, want)
	}

	wantRemove := []string{"set-environment", "-t", "hive-scanner", "-r", "COPILOT_GITHUB_TOKEN"}
	for _, empty := range []string{"", "   "} {
		if got := copilotTokenRefreshTmuxArgs("hive-scanner", empty); !reflect.DeepEqual(got, wantRemove) {
			t.Errorf("empty token %q: got %v, want %v", empty, got, wantRemove)
		}
	}
}

// The #6500 case: the operator ran /login inside an agent terminal, the token
// was promoted, and every pane is still sitting on the token GitHub rejected.
// Those panes — and only those — must be relaunched onto the new credential.
func TestPlanCopilotTokenPropagation_RelaunchesOnlyRejectedPanes(t *testing.T) {
	m := testManager(5)
	m.copilotAuthToken = "gho_fresh"
	m.agents["scanner"] = copilotAgent("scanner", "hive-scanner", BackendAuthUnlicensed, StateRunning)
	m.agents["quality"] = copilotAgent("quality", "hive-quality", BackendAuthTokenExpired, StateRunning)
	m.agents["guide"] = copilotAgent("guide", "hive-guide", BackendAuthOK, StateRunning)
	// Never classified: read as ok (BackendAuthState's zero value).
	m.agents["docs"] = copilotAgent("docs", "hive-docs", "", StateRunning)
	// Transient failures are not credential failures; a new token cannot help.
	m.agents["builder"] = copilotAgent("builder", "hive-builder", BackendAuthQuota, StateRunning)
	m.agents["relay"] = copilotAgent("relay", "hive-relay", BackendAuthUnreachable, StateRunning)
	// Not running: the pane holds no CLI to move onto the new token, and the
	// relaunch on resume reads the token from applySecretEnv anyway.
	m.agents["paused"] = copilotAgent("paused", "hive-paused", BackendAuthUnlicensed, StatePaused)

	plan := planFor(t, m)

	var relaunched []string
	for name, p := range plan {
		if p.Relaunch {
			relaunched = append(relaunched, name)
		}
		want := []string{"set-environment", "-t", p.Agent.tmuxSession, "COPILOT_GITHUB_TOKEN", "gho_fresh"}
		if !reflect.DeepEqual(p.TmuxArgs, want) {
			t.Errorf("%s: tmux args = %v, want %v", name, p.TmuxArgs, want)
		}
	}
	sort.Strings(relaunched)
	if want := []string{"quality", "scanner"}; !reflect.DeepEqual(relaunched, want) {
		t.Errorf("relaunched = %v, want %v", relaunched, want)
	}
	// Every copilot agent with a session still gets the env push, so nothing
	// forked from a healthy session later inherits the superseded token.
	if len(plan) != 7 {
		t.Errorf("planned for %d agents, want all 7", len(plan))
	}
}

// A bare 403 verdict (BackendAuthForbidden — cause undetermined but still an
// upstream rejection) is a hard-auth state a token swap can plausibly clear, so
// a running pane carrying it must be relaunched onto the fresh token exactly
// like an unlicensed or token-expired pane (#6500). Reporting the 403 honestly
// instead of as a false expiry must not cost the agent its recovery.
func TestPlanCopilotTokenPropagation_RelaunchesForbiddenPane(t *testing.T) {
	m := testManager(5)
	m.copilotAuthToken = "gho_fresh"
	m.agents["scanner"] = copilotAgent("scanner", "hive-scanner", BackendAuthForbidden, StateRunning)
	m.agents["guide"] = copilotAgent("guide", "hive-guide", BackendAuthOK, StateRunning)

	plan := planFor(t, m)
	if !plan["scanner"].Relaunch {
		t.Errorf("forbidden pane not relaunched onto fresh token: %+v", plan["scanner"])
	}
	if plan["guide"].Relaunch {
		t.Errorf("healthy pane must not be relaunched: %+v", plan["guide"])
	}
}

// Agents on another backend, and agents with no session yet, are not ours to
// touch — a claude agent's pane has no COPILOT_GITHUB_TOKEN to refresh and a
// session-less agent gets the current token at creation.
func TestPlanCopilotTokenPropagation_SkipsForeignAndSessionless(t *testing.T) {
	m := testManager(5)
	m.copilotAuthToken = "gho_fresh"
	m.agents["scanner"] = copilotAgent("scanner", "hive-scanner", BackendAuthUnlicensed, StateRunning)

	claude := copilotAgent("claude", "hive-claude", BackendAuthUnlicensed, StateRunning)
	claude.Config.Backend = "claude"
	m.agents["claude"] = claude
	m.agents["unlaunched"] = copilotAgent("unlaunched", "", BackendAuthUnlicensed, StateRunning)

	plan := planFor(t, m)
	if len(plan) != 1 {
		t.Fatalf("plan covers %d agents, want only scanner: %v", len(plan), plan)
	}
	if _, ok := plan["scanner"]; !ok {
		t.Errorf("scanner missing from plan: %v", plan)
	}
}

// A logout (empty token) still strips the stale credential from every session,
// but must never relaunch: bringing the CLI back with no token at all replaces
// a licence error with a /login prompt and helps nobody.
func TestPlanCopilotTokenPropagation_LogoutStripsButNeverRelaunches(t *testing.T) {
	m := testManager(5)
	m.copilotAuthToken = ""
	m.agents["scanner"] = copilotAgent("scanner", "hive-scanner", BackendAuthUnlicensed, StateRunning)

	p := planFor(t, m)["scanner"]
	if p.Relaunch {
		t.Error("logout must not relaunch agents")
	}
	want := []string{"set-environment", "-t", "hive-scanner", "-r", "COPILOT_GITHUB_TOKEN"}
	if !reflect.DeepEqual(p.TmuxArgs, want) {
		t.Errorf("tmux args = %v, want %v", p.TmuxArgs, want)
	}
}

// setCopilotToken's bool is what gates propagation. Re-asserting the same
// token — the reconciler's ordinary outcome — must report no change, or every
// tick would churn the fleet's sessions.
func TestSetCopilotTokenReportsValueChange(t *testing.T) {
	m := testManager(5)
	if !m.setCopilotToken("gho_a", true, CopilotTokenSourceDashboardLogin) {
		t.Error("first install must report a change")
	}
	if got := m.CopilotTokenSource(); got != CopilotTokenSourceDashboardLogin {
		t.Errorf("CopilotTokenSource = %q, want %q", got, CopilotTokenSourceDashboardLogin)
	}
	if m.setCopilotToken("gho_a", true, CopilotTokenSourceDashboardLogin) {
		t.Error("re-asserting the same token must report no change")
	}
	// Whitespace is not a value change: the durable file is read trimmed and
	// the CLI config is not, so the same token routinely arrives both ways.
	if m.setCopilotToken(" gho_a\n", false, CopilotTokenSourceCLIConfig) {
		t.Error("whitespace-only difference must report no change")
	}
	// The source follows the latest installer even when the value did not
	// change: the same token re-asserted out of the CLI config IS now the CLI
	// config's token, and the notice must say so.
	if got := m.CopilotTokenSource(); got != CopilotTokenSourceCLIConfig {
		t.Errorf("CopilotTokenSource after re-assert = %q, want %q", got, CopilotTokenSourceCLIConfig)
	}
	if !m.setCopilotToken("gho_b", true, CopilotTokenSourceDashboardLogin) {
		t.Error("a different token must report a change")
	}
	if !m.setCopilotToken("", true, CopilotTokenSourceDashboardLogin) {
		t.Error("logout must report a change")
	}
	if m.copilotAuthTokenAuthoritative {
		t.Error("an empty token must never be authoritative (#6500)")
	}
	// A logout must not leave the picker blaming a login that no longer
	// exists (#7302).
	if got := m.CopilotTokenSource(); got != "" {
		t.Errorf("CopilotTokenSource after logout = %q, want empty", got)
	}
}

// The relaunch predicate on its own: only the two hard-auth verdicts, and only
// for copilot agents.
func TestCopilotSessionCarriesRejectedToken(t *testing.T) {
	cases := map[string]bool{
		BackendAuthUnlicensed:   true,
		BackendAuthTokenExpired: true,
		BackendAuthOK:           false,
		BackendAuthQuota:        false,
		BackendAuthUnreachable:  false,
		"":                      false,
	}
	for status, want := range cases {
		a := copilotAgent("a", "s", status, StateRunning)
		if got := copilotSessionCarriesRejectedToken(a); got != want {
			t.Errorf("status %q: got %v, want %v", status, got, want)
		}
	}
	other := copilotAgent("a", "s", BackendAuthUnlicensed, StateRunning)
	other.Config.Backend = "claude"
	if copilotSessionCarriesRejectedToken(other) {
		t.Error("a non-copilot agent is never a copilot-token relaunch candidate")
	}
	if copilotSessionCarriesRejectedToken(nil) {
		t.Error("nil agent must be false")
	}
}
