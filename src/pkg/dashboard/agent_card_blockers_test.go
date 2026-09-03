package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// Agent-card blockers (#5594). An operator met three separate reasons an agent
// would not run — session down, paused, and no cadence configured at all — one
// click at a time, and after clearing two of them the agent still never ran.
// These tests pin the two halves of the fix: the backend flag that names the
// third reason per agent, and the card structure that shows all three at once.

func TestBuildAgents_NoCadence(t *testing.T) {
	kicked := time.Now().Add(-time.Hour)
	statuses := map[string]*agent.AgentProcess{
		"scheduled":   newBlockerProc("scheduled", config.AgentConfig{Backend: "claude", Enabled: true}, nil),
		"unscheduled": newBlockerProc("unscheduled", config.AgentConfig{Backend: "claude", Enabled: true}, nil),
		"kickedonce":  newBlockerProc("kickedonce", config.AgentConfig{Backend: "claude", Enabled: true}, &kicked),
		"switchedoff": newBlockerProc("switchedoff", config.AgentConfig{Backend: "claude"}, nil),
		"manual":      newBlockerProc("manual", config.AgentConfig{Backend: "claude", Enabled: true, OnDemand: true}, nil),
	}
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scheduled":   {Backend: "claude", Enabled: true},
			"unscheduled": {Backend: "claude", Enabled: true},
			"kickedonce":  {Backend: "claude", Enabled: true},
			"switchedoff": {Backend: "claude"},
			"manual":      {Backend: "claude", Enabled: true, OnDemand: true},
		},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{
			"idle": {Cadences: map[string]config.Cadence{"scheduled": "30m"}},
		}},
	}

	byName := map[string]FrontendAgent{}
	for _, a := range buildAgents(statuses, cfg, governor.State{Mode: governor.ModeIdle}) {
		byName[a.Name] = a
	}

	want := map[string]bool{
		"scheduled":   false,
		"unscheduled": true,
		"kickedonce":  false,
		"switchedoff": false,
		"manual":      false,
	}
	for name, wantFlag := range want {
		a, ok := byName[name]
		if !ok {
			t.Fatalf("agent %q missing from buildAgents output", name)
		}
		if a.NoCadence != wantFlag {
			t.Errorf("%s: NoCadence = %v, want %v", name, a.NoCadence, wantFlag)
		}
	}
}

func newBlockerProc(name string, cfg config.AgentConfig, lastKick *time.Time) *agent.AgentProcess {
	return &agent.AgentProcess{
		Name:         name,
		Config:       cfg,
		State:        agent.StateRunning,
		LastKick:     lastKick,
		OutputBuffer: agent.NewRingBuffer(10),
	}
}

func TestBuildAgents_NextKickIn(t *testing.T) {
	last := time.Now().Add(-20 * time.Minute)
	statuses := map[string]*agent.AgentProcess{
		"scheduled": newBlockerProc("scheduled", config.AgentConfig{Backend: "claude", Enabled: true}, &last),
		"stopped":   newBlockerProc("stopped", config.AgentConfig{Backend: "claude", Enabled: true}, &last),
	}
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scheduled": {Backend: "claude", Enabled: true},
			"stopped":   {Backend: "claude", Enabled: true},
		},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{
			"idle": {Cadences: map[string]config.Cadence{"scheduled": "30m", "stopped": "pause"}},
		}},
	}
	byName := map[string]FrontendAgent{}
	for _, a := range buildAgents(statuses, cfg, governor.State{Mode: governor.ModeIdle}) {
		byName[a.Name] = a
	}
	if got := byName["scheduled"].NextKickIn; got != "10m" {
		t.Errorf("scheduled NextKickIn = %q, want %q", got, "10m")
	}
	if got := byName["stopped"].NextKickIn; got != "" {
		t.Errorf("paused-cadence NextKickIn = %q, want empty", got)
	}
	if byName["stopped"].NextKick != "" {
		t.Errorf("NextKickIn and NextKick must be empty together, got NextKick=%q", byName["stopped"].NextKick)
	}
}

func TestAgentCardBlockerHelpersPresent(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"function agentSchedulingBlockers(a) {",
		"function agentToggleTitle(a, isPaused, isOff) {",
		"function neverScheduledChipHtml(a, configTarget) {",
		"function agentBlockerLineHtml(a) {",
		"if (a.noCadence === true) out.push('never scheduled');",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}
}

func TestAgentCardRendersAllBlockersAtOnce(t *testing.T) {
	html := indexHTML(t)
	if strings.Count(html, "${agentBlockerLineHtml(a)}") != 2 {
		t.Error("the blockers line must render on BOTH the agent card and the ops-center detail panel")
	}
	if !strings.Contains(html, "${toggleBtn}${neverScheduledChipHtml(a, configTarget)}") {
		t.Error("the never-scheduled chip must sit beside the card's action button")
	}
	if !strings.Contains(html, "blockersEl.innerHTML = agentBlockerLineHtml(a) + neverScheduledChipHtml(a, a.name);") {
		t.Error("the detail panel's poll fast path must refresh the blockers line, or it goes stale")
	}
}

func TestAgentCardResumeLeads(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "const toggleBtn = canToggle ? (isPaused && canPauseToggle") {
		t.Error("a paused agent's card must offer resume FIRST — the cadence gear must not take the slot")
	}
	if strings.Contains(html, "const toggleBtn = canToggle ? (isOff") {
		t.Error("isOff still leads the card toggle, so a pause can hide behind it")
	}
	if !strings.Contains(html, "return `${base}. Resuming alone is NOT enough: ${rest.join('; ')}.`;") {
		t.Error("the resume tooltip must name the blockers resuming will NOT clear")
	}
}
