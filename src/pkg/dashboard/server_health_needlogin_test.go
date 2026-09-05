package dashboard

// Health-check classification of unauthenticated agents. An agent whose CLI
// has no credentials is not crashed — it is waiting for a human to click
// Login on the agent panel — so the agents health check must report it in a
// separate "need login" bucket instead of lumping it in with "down". These
// tests drive the real HealthSummary path through a real agent.Manager plus
// the same SetBackendAuthProvider seam production wires in main.go.

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/tokens"
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

func healthCheckOf(t *testing.T, s *Server, name string) (status, detail string) {
	t.Helper()
	for _, c := range healthChecksOf(t, s) {
		if c.Name == name {
			return c.Status, c.Detail
		}
	}
	t.Fatalf("no %q check in HealthSummary", name)
	return "", ""
}

func TestHealthSummary_QuietByDesignAgentsAreIdleNotDown(t *testing.T) {
	deps := testDeps(t)
	deps.Config.Agents = map[string]config.AgentConfig{
		"reviewer": {Backend: "copilot", Enabled: true, OnDemand: true},
		"nightly":  {Backend: "copilot", Enabled: true},
		"paused":   {Backend: "copilot", Enabled: true, Paused: true},
		"crashed":  {Backend: "copilot", Enabled: true},
	}
	deps.Config.Governor = config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle": {Cadences: map[string]config.Cadence{
			"nightly": "pause",
			"paused":  "15m",
			"crashed": "15m",
		}},
	}}
	deps.Governor = governor.New(deps.Config.Governor, deps.Config.Agents, deps.Logger)
	deps.AgentMgr = agent.NewManager(deps.Config.Agents, deps.Logger, agent.ProjectContext{})
	healthAgentStatuses = func() map[string]*agent.AgentProcess {
		return map[string]*agent.AgentProcess{
			"reviewer": {State: agent.StateStopped, Config: deps.Config.Agents["reviewer"]},
			"nightly":  {State: agent.StateStopped, Config: deps.Config.Agents["nightly"]},
			"paused":   {State: agent.StatePaused, Paused: true, Config: deps.Config.Agents["paused"]},
			"crashed":  {State: agent.StateStopped, Config: deps.Config.Agents["crashed"]},
		}
	}
	t.Cleanup(func() { healthAgentStatuses = nil })
	SetBackendAuthProvider(func(string) (bool, bool) { return true, true })
	t.Cleanup(func() { SetBackendAuthProvider(nil) })

	s := NewServer(0, deps.Logger)
	s.RegisterAPI(deps)
	s.startedAt = time.Now().Add(-2 * healthBootGrace)

	status, detail := agentsCheckOf(t, s)
	if status != "fail" {
		t.Fatalf("agents status = %q, want fail for genuinely crashed expected-active agent", status)
	}
	for _, want := range []string{"1 paused", "2 idle: nightly (off-schedule), reviewer (on-demand)", "1 down: crashed"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("agents detail = %q, want segment %q", detail, want)
		}
	}
}

func scannedTokenCollector(t *testing.T, dir string, live bool) *tokens.Collector {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	c := tokens.NewCollector(dir, logger)
	c.SetPersistPath(filepath.Join(t.TempDir(), "token-summary.json"))
	if live {
		c.SetCopilotLiveCapture(time.Now().UnixMilli())
	}
	stop := make(chan struct{})
	close(stop)
	c.Start(stop)
	return c
}

func tokenHealthServer(t *testing.T, agentCfgs map[string]config.AgentConfig, statuses map[string]*agent.AgentProcess, collector *tokens.Collector) *Server {
	t.Helper()
	deps := testDeps(t)
	deps.Config.Agents = agentCfgs
	idleMode := config.ModeConfig{Cadences: map[string]config.Cadence{}}
	for name, ac := range agentCfgs {
		if !ac.OnDemand {
			idleMode.Cadences[name] = "15m"
		}
	}
	deps.Config.Governor = config.GovernorConfig{Modes: map[string]config.ModeConfig{"idle": idleMode}}
	deps.Governor = governor.New(deps.Config.Governor, deps.Config.Agents, deps.Logger)
	deps.AgentMgr = agent.NewManager(deps.Config.Agents, deps.Logger, agent.ProjectContext{})
	deps.Tokens = collector
	healthAgentStatuses = func() map[string]*agent.AgentProcess { return statuses }
	t.Cleanup(func() { healthAgentStatuses = nil })
	s := NewServer(0, deps.Logger)
	s.RegisterAPI(deps)
	s.startedAt = time.Now().Add(-2 * healthBootGrace)
	return s
}

func TestHealthSummary_ZeroTokensQuietCasesAreSkipped(t *testing.T) {
	collector := scannedTokenCollector(t, t.TempDir(), false)
	pausedCfg := config.AgentConfig{Backend: "copilot", Enabled: true, Paused: true}
	pausedSrv := tokenHealthServer(t,
		map[string]config.AgentConfig{"scanner": pausedCfg},
		map[string]*agent.AgentProcess{"scanner": {State: agent.StatePaused, Paused: true, Config: pausedCfg}},
		collector)
	if st, detail := healthCheckOf(t, pausedSrv, "tokens"); st != "skip" || detail != "zero consumed — all agents paused" {
		t.Fatalf("paused tokens check = %q/%q, want skip/all paused", st, detail)
	}

	idleCollector := scannedTokenCollector(t, t.TempDir(), false)
	idleCfg := config.AgentConfig{Backend: "copilot", Enabled: true, OnDemand: true}
	idleSrv := tokenHealthServer(t,
		map[string]config.AgentConfig{"reviewer": idleCfg},
		map[string]*agent.AgentProcess{"reviewer": {State: agent.StateStopped, Config: idleCfg}},
		idleCollector)
	if st, detail := healthCheckOf(t, idleSrv, "tokens"); st != "skip" || !strings.Contains(detail, "no agents due") {
		t.Fatalf("no-due tokens check = %q/%q, want skip/no agents due", st, detail)
	}

	runningCollector := scannedTokenCollector(t, t.TempDir(), false)
	runningCfg := config.AgentConfig{Backend: "copilot", Enabled: true, OnDemand: true}
	runningSrv := tokenHealthServer(t,
		map[string]config.AgentConfig{"reviewer": runningCfg},
		map[string]*agent.AgentProcess{"reviewer": {State: agent.StateRunning, Config: runningCfg}},
		runningCollector)
	if st, detail := healthCheckOf(t, runningSrv, "tokens"); st != "warn" || !strings.Contains(detail, "metering disabled") {
		t.Fatalf("running on-demand tokens check = %q/%q, want warn/metering cause", st, detail)
	}
}

func TestHealthSummary_ZeroTokensReportsCause(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(`{"role":"user","message":"scanner waiting"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write session fixture: %v", err)
	}
	collector := scannedTokenCollector(t, dir, true)
	scannerCfg := config.AgentConfig{Backend: "copilot", Enabled: true}
	s := tokenHealthServer(t,
		map[string]config.AgentConfig{"scanner": scannerCfg},
		map[string]*agent.AgentProcess{"scanner": {State: agent.StateRunning, Config: scannerCfg}},
		collector)
	if st, detail := healthCheckOf(t, s, "tokens"); st != "warn" || !strings.Contains(detail, "no model calls recorded") {
		t.Fatalf("tokens check = %q/%q, want warn with zero-token cause", st, detail)
	}
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
	// Push startedAt past the boot-grace window so non-running / need-login
	// agents are classified and degrade instead of being suppressed as
	// still-coming-up transients. (Grace is exercised separately below.)
	s.startedAt = time.Now().Add(-2 * healthBootGrace)

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
	s.startedAt = time.Now().Add(-2 * healthBootGrace) // past boot grace

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
	// Bob API-key rejection is a credential/login problem even while the key file exists.
	p = &agent.AgentProcess{Config: config.AgentConfig{Backend: "bob"}, StartFailureClass: string(agent.StartFailureCredentialRejected)}
	if !agentCLIUnauthenticated(p, authFor(map[string][2]bool{"bob": {true, true}})) {
		t.Fatal("Bob rejected API key must count as need-login instead of down")
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
	s.startedAt = time.Now().Add(-2 * healthBootGrace) // past boot grace

	status, detail := agentsCheckOf(t, s)
	if want := "1 running, 1 needs login: wedged"; detail != want {
		t.Fatalf("agents detail = %q, want %q", detail, want)
	}
	if status != "fail" {
		t.Fatalf("agents status = %q, want fail — a wedged agent must surface", status)
	}
}

// summaryOf runs HealthSummary and returns the top-level "status" verdict and
// the "fails" count. testDeps wires no GitHub auth, so overall alone is polluted
// by an unrelated github_auth failure — the fails DELTA between in-grace and
// past-grace is the clean signal for the agents contribution these tests probe.
func summaryOf(t *testing.T, s *Server) (status string, fails int) {
	t.Helper()
	raw, err := json.Marshal(s.HealthSummary())
	if err != nil {
		t.Fatalf("marshal HealthSummary: %v", err)
	}
	var parsed struct {
		Status string `json:"status"`
		Fails  int    `json:"fails"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal HealthSummary: %v", err)
	}
	return parsed.Status, parsed.Fails
}

// needLoginServer wires a single non-running, unauthenticated ("need login")
// agent — the exact boot-transient condition Mike Spreitzer saw flip a freshly
// rolled hive to "Degraded — agents need login". Returns the server so the
// caller can position startedAt inside or outside the boot-grace window.
func needLoginServer(t *testing.T) *Server {
	t.Helper()
	deps := testDeps(t)
	deps.AgentMgr = agent.NewManager(map[string]config.AgentConfig{
		"supervisor": {Backend: "bob", Enabled: true},
	}, deps.Logger, agent.ProjectContext{})
	SetBackendAuthProvider(func(backend string) (available, known bool) {
		return false, backend == "bob" // known-unauthenticated → need login
	})
	t.Cleanup(func() { SetBackendAuthProvider(nil) })
	s := NewServer(0, deps.Logger)
	s.RegisterAPI(deps)
	s.MarkReady()
	return s
}

// TestHealthBootGrace_NeedLoginWithinGraceNotDegraded proves the fix: a
// need-login agent seen WITHIN the boot-grace window (fresh start / pod roll,
// agent CLI still re-authenticating) does NOT degrade the hive — the agents
// check passes and overall stays "ok" — and the suppression is observable via
// an "within boot grace" detail carrying the process age.
func TestHealthBootGrace_NeedLoginWithinGraceNotDegraded(t *testing.T) {
	// Two identical fleets differing only in process age. testDeps wires other
	// failing checks (no GitHub auth, etc.), so the fails DELTA — not the
	// absolute count — is the clean signal that the agents path is what grace
	// suppresses.
	within := needLoginServer(t)
	within.startedAt = time.Now() // squarely inside the boot-grace window
	past := needLoginServer(t)
	past.startedAt = time.Now().Add(-2 * healthBootGrace) // window elapsed

	status, detail := agentsCheckOf(t, within)
	if status != "pass" {
		t.Fatalf("agents status = %q within boot grace, want pass (transient)", status)
	}
	if !strings.Contains(detail, "within boot grace") {
		t.Fatalf("agents detail = %q, want it to explain the boot-grace suppression", detail)
	}
	if !strings.Contains(detail, "age=") {
		t.Fatalf("agents detail = %q, want an age= so operators can see the window", detail)
	}
	// The graced agents check contributes NO fail: past-grace has exactly one
	// more fail (the now-degrading agents check) than the in-grace fleet.
	_, withinFails := summaryOf(t, within)
	_, pastFails := summaryOf(t, past)
	if pastFails != withinFails+1 {
		t.Fatalf("fails within grace = %d, past grace = %d — grace must suppress exactly the one agents fail", withinFails, pastFails)
	}
}

// TestHealthBootGrace_NeedLoginPastGraceDegraded proves grace is bounded: once
// the boot-grace window elapses, the SAME need-login condition flips to a real
// "fail" verdict so a stuck-at-login agent is not masked forever.
func TestHealthBootGrace_NeedLoginPastGraceDegraded(t *testing.T) {
	s := needLoginServer(t)
	s.startedAt = time.Now().Add(-2 * healthBootGrace) // window has elapsed

	status, detail := agentsCheckOf(t, s)
	if status != "fail" {
		t.Fatalf("agents status = %q past boot grace, want fail (persisting need-login)", status)
	}
	if strings.Contains(detail, "within boot grace") {
		t.Fatalf("agents detail = %q must NOT claim boot grace past the window", detail)
	}
	// A persisting need-login past the window degrades the hive.
	if overall, _ := summaryOf(t, s); overall == "ok" {
		t.Fatalf("overall = %q past boot grace, want a degraded/critical verdict", overall)
	}
}

// TestHealthBootGrace_StaleHeartbeatDegradesEvenWithinGrace proves the grace is
// scoped tightly to the agents-coming-up transient and does NOT blanket-suppress
// genuinely-persistent failures. A GitHub-auth failure (a real, non-transient
// problem) must degrade the hive EVEN inside the boot-grace window — boot grace
// only silences the need-login/not-yet-running agents path, never a real
// dependency failure.
func TestHealthBootGrace_PersistentFailureDegradesEvenWithinGrace(t *testing.T) {
	deps := testDeps(t)
	deps.AgentMgr = agent.NewManager(map[string]config.AgentConfig{
		"supervisor": {Backend: "bob", Enabled: true},
	}, deps.Logger, agent.ProjectContext{})
	SetBackendAuthProvider(func(backend string) (available, known bool) {
		return false, backend == "bob"
	})
	t.Cleanup(func() { SetBackendAuthProvider(nil) })
	// Force a genuine, non-transient dependency failure: no GitHub auth wired
	// at all → the github_auth check fails regardless of boot age.
	deps.GHAppAuth = nil
	deps.GHClient = nil

	s := NewServer(0, deps.Logger)
	s.RegisterAPI(deps)
	s.MarkReady()
	s.startedAt = time.Now() // inside the boot-grace window

	// The agents check itself is graced (passes)...
	if status, _ := agentsCheckOf(t, s); status != "pass" {
		t.Fatalf("agents status = %q within grace, want pass (agents path is graced)", status)
	}
	// ...but the persistent github_auth failure still degrades overall: grace
	// must not mask a real dependency failure just because the pod is young.
	if overall, fails := summaryOf(t, s); overall == "ok" || fails == 0 {
		t.Fatalf("overall = %q, fails = %d — a persistent github_auth failure must degrade EVEN within boot grace", overall, fails)
	}
}
