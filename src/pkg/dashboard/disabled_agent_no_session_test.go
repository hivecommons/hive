package dashboard

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// A disabled agent still has a RUNTIME entry: ApplyPack adds every agent in the
// ACMM level's pack to the manager and gates only the Start on enabled
// (api_packs.go). Everything below covers what that costs when nothing carries
// the enabled flag forward — a card that offers a terminal and a full log for an
// agent that has never had a tmux session, and two health checks that score a
// deliberately switched-off agent as a failure.
//
// Observed live on hive-wild-mole (ACMM L3): guide, brainstorm and ci-maintainer
// sat at state=stopped, restarts=0, and /api/agents/guide/log answered
// "capturing pane for guide: exit status 1" while the card still linked to both.

func TestBuildAgentsCarriesEnabledFlag(t *testing.T) {
	statuses := map[string]*agent.AgentProcess{
		"scanner": {
			Name:         "scanner",
			Config:       config.AgentConfig{Backend: "claude", Enabled: true},
			State:        agent.StateRunning,
			OutputBuffer: agent.NewRingBuffer(10),
		},
		"guide": {
			Name:         "guide",
			Config:       config.AgentConfig{Backend: "copilot", Enabled: false},
			State:        agent.StateStopped,
			OutputBuffer: agent.NewRingBuffer(10),
		},
	}
	cfg := &config.Config{Agents: map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Enabled: true, SortOrder: 10},
		"guide":   {Backend: "copilot", Enabled: false, SortOrder: 20},
	}}

	byName := map[string]FrontendAgent{}
	for _, a := range buildAgents(statuses, cfg, governor.State{Mode: governor.ModeIdle}) {
		byName[a.Name] = a
	}

	if !byName["scanner"].Enabled {
		t.Error("scanner.Enabled = false, want true")
	}
	if byName["guide"].Enabled {
		t.Error("guide.Enabled = true, want false — a disabled agent must be distinguishable from a crashed one")
	}
	// State alone cannot carry this: a stopped-because-disabled agent and a
	// stopped-because-it-died agent are the same string here, which is why the
	// flag has to travel separately.
	if byName["guide"].State != string(agent.StateStopped) {
		t.Fatalf("guide.State = %q, want stopped (precondition)", byName["guide"].State)
	}
}

func TestBuildAgentsPrefersLiveConfigForEnabled(t *testing.T) {
	// proc.Config is the manager's COPY, refreshed only on UpdateConfig or a
	// reconcile. An operator who just disabled the agent from the config dialog
	// has written cfg.Agents and nothing else, so the live config must win.
	statuses := map[string]*agent.AgentProcess{
		"guide": {
			Name:         "guide",
			Config:       config.AgentConfig{Backend: "copilot", Enabled: true},
			State:        agent.StateStopped,
			OutputBuffer: agent.NewRingBuffer(10),
		},
	}
	cfg := &config.Config{Agents: map[string]config.AgentConfig{
		"guide": {Backend: "copilot", Enabled: false},
	}}

	agents := buildAgents(statuses, cfg, governor.State{Mode: governor.ModeIdle})
	if len(agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(agents))
	}
	if agents[0].Enabled {
		t.Error("Enabled = true, want false — the stale process copy outranked the live config")
	}
}

func TestAgentDisabledInConfigFallsBackToProcessCopy(t *testing.T) {
	proc := &agent.AgentProcess{Name: "guide", Config: config.AgentConfig{Enabled: false}}

	// Absent from the live config (an agent that exists only in the roster):
	// the process copy is all there is.
	cfg := &config.Config{Agents: map[string]config.AgentConfig{}}
	if !agentDisabledInConfig(cfg, "guide", proc) {
		t.Error("want disabled when the live config has no entry and the process copy says disabled")
	}
	// Present and enabled: the live config wins over the process copy.
	cfg.Agents["guide"] = config.AgentConfig{Enabled: true}
	if agentDisabledInConfig(cfg, "guide", proc) {
		t.Error("want enabled — the live config entry must outrank the process copy")
	}
	// Nothing to read at all is not a claim that the agent is off.
	if agentDisabledInConfig(nil, "guide", nil) {
		t.Error("want false with no config and no process — absence is not evidence of disabled")
	}
}

func TestHealthDeepSkipsDisabledAgentInsteadOfFailing(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Agents["guide"] = config.AgentConfig{Backend: "copilot", Enabled: false}
	deps.AgentMgr.AddAgent("guide", deps.Config.Agents["guide"])

	rec := doGet(s, "/api/health/deep")
	if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("health deep: unexpected %d", rec.Code)
	}
	checks, ok := decodeJSON(t, rec)["checks"].(map[string]any)
	if !ok {
		t.Fatalf("no checks in body: %s", rec.Body.String())
	}
	agents, ok := checks["agents"].(map[string]any)
	if !ok {
		t.Fatalf("no agents check in body: %s", rec.Body.String())
	}
	guide, ok := agents["guide"].(map[string]any)
	if !ok {
		t.Fatalf("guide missing from agent checks: %#v", agents)
	}
	// "skip" is the verdict the paused branch already uses for a deliberate
	// off-state. "fail" here is a health failure the operator can never clear
	// without turning on an agent they chose to turn off.
	if guide["status"] != "skip" {
		t.Errorf("guide status = %#v, want skip", guide["status"])
	}
	if guide["disabled"] != true {
		t.Errorf("guide disabled = %#v, want true", guide["disabled"])
	}
}

func TestDisabledAgentSessionActionsAreInert(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		// The flag has to reach the browser before anything can act on it.
		`function agentIsDisabled(a) {`,
		// state is checked too: an agent disabled while still up keeps its
		// session until the next reconcile, and its terminal still works.
		`return agentIsDisabled(a) && a.state !== 'running';`,
		// Both dead-end actions routed through the same gate, on both the
		// agent card and the operations-center detail panel.
		`${sessionChip(a, '▶ terminal',`,
		`${sessionChip(a, '📄 full log',`,
		`${sessionChip(a, '⬇ log',`,
		// The card says WHY it is grey rather than leaving "stopped" to be
		// read as a crash.
		`<span class="status-badge disabled"`,
		`.status-badge.disabled { --pill-c: var(--muted); }`,
		`.terminal-link.inert {`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html missing wiring: %s", snippet)
		}
	}
	// The inert chip must not be a link — that is the whole point.
	if strings.Contains(html, `class="terminal-link inert" href`) {
		t.Error("inert chip rendered as a link")
	}
}
