package main

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/hub"
)

func TestHeartbeatKickIntervalOnlyForGovernorKickedAgents(t *testing.T) {
	govState := governor.State{Cadences: map[string]governor.AgentCadence{
		"quality": {Agent: "quality", Interval: 2 * time.Hour},
	}}
	proc := &agent.AgentProcess{Name: "quality", Config: config.AgentConfig{}}
	if got := hub.HeartbeatKickInterval(govState, "quality", proc, nil); got != 2*time.Hour {
		t.Fatalf("implicit kick channel interval = %v, want 2h", got)
	}

	proc.Config.Channels = []config.ChannelConfig{{Type: "webhook"}}
	if got := hub.HeartbeatKickInterval(govState, "quality", proc, nil); got != 0 {
		t.Fatalf("non-kick channel interval = %v, want 0", got)
	}

	proc.Config.Channels = []config.ChannelConfig{{Type: "kick"}}
	if got := hub.HeartbeatKickInterval(govState, "quality", proc, map[string]bool{"quality": true}); got != 0 {
		t.Fatalf("pack on-demand interval = %v, want 0", got)
	}
}
