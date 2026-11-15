package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/meetings/live"
)

type meetingResearchRuntime struct {
	agents   map[string]config.AgentConfig
	observer func(crux.KickEvent)
	queueErr error
	prompt   string
}

func (f *meetingResearchRuntime) AddAgent(name string, cfg config.AgentConfig) {
	f.agents[name] = cfg
}
func (f *meetingResearchRuntime) GetStatusFast(name string) (*crux.Process, error) {
	return &crux.Process{Name: name, State: crux.StateRunning}, nil
}
func (f *meetingResearchRuntime) Start(context.Context, string) error { return nil }
func (f *meetingResearchRuntime) KickStatus(string) (crux.KickStatus, bool) {
	return crux.KickStatus{}, false
}
func (f *meetingResearchRuntime) KickQueueDepth(string) int    { return 0 }
func (f *meetingResearchRuntime) InFlightKickID(string) string { return "" }
func (f *meetingResearchRuntime) AddKickObserver(fn func(crux.KickEvent)) func() {
	f.observer = fn
	return func() { f.observer = nil }
}
func (f *meetingResearchRuntime) EnqueueKick(agent, id, prompt string) (string, error) {
	if f.queueErr != nil {
		return "", f.queueErr
	}
	f.prompt = prompt
	for _, signal := range []string{crux.OutputSignalFinding, crux.OutputSignalComplete} {
		f.observer(crux.KickEvent{Agent: agent, KickID: id, Event: crux.KickEventOutputSignal, Signal: signal, Text: "public source"})
	}
	return id, nil
}

func TestWireMeetingsResearch(t *testing.T) {
	t.Setenv(config.MeetingsEnabledEnv, "")
	t.Setenv(config.MeetingsResearchPoolSizeEnv, "")
	rt := &meetingResearchRuntime{agents: map[string]config.AgentConfig{}}
	for _, cfg := range []*config.Config{nil, {}} {
		if wireMeetingsResearch(cfg, rt, nil) != nil || len(rt.agents) != 0 {
			t.Fatal("disabled meetings created research agents")
		}
	}
	cfg := &config.Config{Meetings: config.MeetingsConfig{Enabled: true, ResearchPoolSize: 3}}
	cfg.Agents = map[string]config.AgentConfig{
		"scanner": {ID: "scanner-id", Backend: "copilot", Model: "scanner-model", Mode: "FULL", LaunchCmd: "unsafe-custom-command"},
	}
	if wireMeetingsResearch(cfg, struct{}{}, nil) != nil {
		t.Fatal("unsupported runtime must leave research unavailable")
	}
	dispatcher := wireMeetingsResearch(cfg, rt, nil)
	if dispatcher == nil || len(rt.agents) != 3 {
		t.Fatalf("research pool not created: %+v", rt.agents)
	}
	for name, agent := range rt.agents {
		if agent.Mode != "ADVISORY" || agent.IncludeRepos == nil || *agent.IncludeRepos || !agent.OnDemand ||
			agent.ID != "" || agent.LaunchCmd != "" || agent.Backend != "copilot" || agent.Model != "scanner-model" || !agent.Enabled {
			t.Fatalf("unsafe or unusable research agent %s: %+v", name, agent)
		}
	}
	var signals []live.ResearchSignal
	remove := dispatcher.(live.ResearchObserver).ObserveResearch(func(sig live.ResearchSignal) { signals = append(signals, sig) })
	defer remove()
	kick, err := dispatcher.Dispatch(context.Background(), "find public sources", map[string]string{"meeting_id": "mtg-1"})
	if err != nil || kick == "" {
		t.Fatalf("dispatch: kick=%q err=%v", kick, err)
	}
	if !strings.Contains(rt.prompt, "find public sources") || len(signals) != 2 ||
		signals[0].Kind != live.SignalFinding || signals[1].Kind != live.SignalComplete ||
		signals[0].KickID != kick || signals[0].MeetingID != "mtg-1" || signals[1].Text != "public source" {
		t.Fatalf("research return missing: prompt=%q signals=%+v", rt.prompt, signals)
	}
	rt.queueErr = errors.New("queue full")
	if _, err := dispatcher.Dispatch(context.Background(), "another query", nil); err == nil {
		t.Fatal("queue failure must reach the live loop")
	}
}
