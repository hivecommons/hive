package dashboard

// Health-check classification of unauthenticated agents. An agent whose CLI
// has no credentials is not crashed — it is waiting for a human to click
// Login on the agent panel — so the agents health check must report it in a
// separate "need login" bucket instead of lumping it in with "down". These
// tests drive the real HealthSummary path through a real agent.Manager plus
// the same SetBackendAuthProvider seam production wires in main.go.

import (
	"encoding/json"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/config"
)

// healthChecksOf runs HealthSummary and decodes its checks through JSON (the
// check struct is a function-local type, so tests read it the same way the
// wire does).
func healthChecksOf(t *testing.T, s *Server) []struct{ Name, Status, Detail string } {
	t.Helper()
	raw, err := json.Marshal(s.HealthSummary())
	if err != nil {
		t.Fatalf("marshal HealthSummary: %v", err)
	}
	var parsed struct {
		Checks []struct{ Name, Status, Detail string } `json:"checks"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal HealthSummary: %v", err)
	}
	return parsed.Checks
}

func agentsCheckOf(t *testing.T, s *Server) (status, detail string) {
	t.Helper()
	for _, c := range healthChecksOf(t, s) {
		if c.Name == "agents" {
			return c.Status, c.Detail
		}
	}
	t.Fatalf("no 'agents' check in HealthSummary")
	return "", ""
}

// TestHealthSummary_NeedLoginBucket locks in the full classification: an
// authenticated crashed agent stays in "down", unauthenticated non-running
// agents land in "need login" with their (sorted) names, both buckets coexist
// on one detail line, and the check still fails so the hub row surfaces it.
func TestHealthSummary_NeedLoginBucket(t *testing.T) {
	deps := testDeps(t)
	agentCfgs := map[string]config.AgentConfig{
		"crashed": {Backend: "claude", Enabled: true}, // authenticated, not running → down
		"needy-b": {Backend: "bob", Enabled: true},    // unauthenticated → need login
		"needy-a": {Backend: "bob", Enabled: true},    // unauthenticated → need login
	}
	deps.AgentMgr = agent.NewManager(agentCfgs, deps.Logger, agent.ProjectContext{})

	SetBackendAuthProvider(func(backend string) (available, known bool) {
		switch backend {
		case "claude":
			return true, true // credentials present
		case "bob":
			return false, true // known unauthenticated — the Login-button state
		}
		return false, false
	})
	t.Cleanup(func() { SetBackendAuthProvider(nil) })

	s := NewServer(0, deps.Logger)
	s.RegisterAPI(deps)
	// No MarkReady: readyAt stays zero so the health grace window is not
	// active and non-running agents are classified instead of skipped.

	status, detail := agentsCheckOf(t, s)

	// Exact line: names sorted, down and need-login coexisting, and the
	// unauthenticated agents NOT counted as down.
	want := "0 running, 1 down: crashed, 2 need login: needy-a, needy-b"
	if detail != want {
		t.Fatalf("agents detail = %q, want %q", detail, want)
	}
	// Agents that cannot work still fail the check — need-login must not
	// silently soften the hub row.
	if status != "fail" {
		t.Fatalf("agents status = %q, want fail (agents needing login cannot work)", status)
	}
}

// TestHealthSummary_NeedLoginOnly checks the detail line when every
// non-running agent is merely unauthenticated: no "down" segment at all.
func TestHealthSummary_NeedLoginOnly(t *testing.T) {
	deps := testDeps(t)
	deps.AgentMgr = agent.NewManager(map[string]config.AgentConfig{
		"supervisor": {Backend: "bob", Enabled: true},
	}, deps.Logger, agent.ProjectContext{})

	SetBackendAuthProvider(func(backend string) (available, known bool) {
		return false, backend == "bob"
	})
	t.Cleanup(func() { SetBackendAuthProvider(nil) })

	s := NewServer(0, deps.Logger)
	s.RegisterAPI(deps)

	status, detail := agentsCheckOf(t, s)
	if want := "0 running, 1 needs login: supervisor"; detail != want {
		t.Fatalf("agents detail = %q, want %q", detail, want)
	}
	if status != "fail" {
		t.Fatalf("agents status = %q, want fail", status)
	}
}

// TestAgentCLIUnauthenticated covers the classifier's edges directly.
func TestAgentCLIUnauthenticated(t *testing.T) {
	authFor := func(m map[string][2]bool) func(string) (bool, bool) {
		return func(backend string) (bool, bool) {
			v := m[backend]
			return v[0], v[1]
		}
	}

	// Pane poller saw a login prompt → unauthenticated even with no probe.
	if !agentCLIUnauthenticated(&agent.AgentProcess{NeedsLogin: true}, nil) {
		t.Fatal("NeedsLogin=true must classify as unauthenticated")
	}
	// Unknown auth state must NOT reclassify a crashed agent out of "down".
	p := &agent.AgentProcess{Config: config.AgentConfig{Backend: "mystery"}}
	if agentCLIUnauthenticated(p, authFor(map[string][2]bool{"mystery": {false, false}})) {
		t.Fatal("unknown auth state must not count as need-login")
	}
	// Nil probe (never registered) → same conservatism.
	if agentCLIUnauthenticated(p, nil) {
		t.Fatal("nil auth probe must not count as need-login")
	}
	// BackendOverride wins over the configured backend, mirroring buildAgents.
	p = &agent.AgentProcess{
		Config:          config.AgentConfig{Backend: "claude"},
		BackendOverride: "bob",
	}
	fn := authFor(map[string][2]bool{"claude": {true, true}, "bob": {false, true}})
	if !agentCLIUnauthenticated(p, fn) {
		t.Fatal("BackendOverride's auth state must be the one consulted")
	}
}

// A RUNNING agent whose pane shows a login prompt (NeedsLogin) is alive but
// cannot work — it must land in "needs login", never in "running". Seen live:
// a copilot agent sat at "Please use /login to sign in" with a green dot and
// Health OK. Only the pane-poller signal moves a running agent — the
// probe-based inference stays non-running-only (a stale probe must not
// reclassify a working agent).
func TestHealthSummary_RunningAtLoginPromptNeedsLogin(t *testing.T) {
	deps := testDeps(t)
	agentCfgs := map[string]config.AgentConfig{
		"wedged": {Backend: "copilot", Enabled: true},
		"worker": {Backend: "copilot", Enabled: true},
	}
	deps.AgentMgr = agent.NewManager(agentCfgs, deps.Logger, agent.ProjectContext{})
	// The probe says copilot credentials exist — proving that the pane-poller
	// NeedsLogin signal alone moves a RUNNING agent into the bucket.
	SetBackendAuthProvider(func(backend string) (available, known bool) { return true, true })
	t.Cleanup(func() { SetBackendAuthProvider(nil) })

	healthAgentStatuses = func() map[string]*agent.AgentProcess {
		return map[string]*agent.AgentProcess{
			"wedged": {State: agent.StateRunning, NeedsLogin: true, Config: config.AgentConfig{Backend: "copilot", Enabled: true}},
			"worker": {State: agent.StateRunning, Config: config.AgentConfig{Backend: "copilot", Enabled: true}},
		}
	}
	t.Cleanup(func() { healthAgentStatuses = nil })

	s := NewServer(0, deps.Logger)
	s.RegisterAPI(deps)

	status, detail := agentsCheckOf(t, s)
	if want := "1 running, 1 needs login: wedged"; detail != want {
		t.Fatalf("agents detail = %q, want %q", detail, want)
	}
	if status != "fail" {
		t.Fatalf("agents status = %q, want fail — a wedged agent must surface", status)
	}
}
