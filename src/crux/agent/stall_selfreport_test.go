package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
)

// A swallowed "continue" nudge feeds the agent_stall self-report with the
// kick/pane context the retired dashboard counter never had (#13470). Every
// nudge is observed (the host's tracker applies its streak before filing)
// and a credited recovery clears the condition.
func TestObserveStallNudgeFeedsSelfReportAndRecoveryClears(t *testing.T) {
	m := &Manager{}
	rec := recordConditions(t, m)
	kickAt := time.Date(2026, 10, 21, 11, 0, 0, 0, time.UTC)
	agent := &AgentProcess{Name: "scanner", Config: config.AgentConfig{Role: "scanner"}, lastInferKickAt: kickAt, lastInferKickPane: paneContentHash("frozen pane"), stallNudgeSent: true}
	pane := "line one\ntoken ghp_abcdefghijklmnopqrstuvwxyz123456\nfrozen pane"
	const nudges = 3
	for i := 0; i < nudges; i++ {
		agent.StallNudges++
		m.observeStallNudgeLocked(agent, pane, kickAt.Add(5*time.Minute))
	}
	ev := rec.active(crux.ClassAgentStall)
	if len(ev) != nudges || ev[0].Key != "scanner" {
		t.Fatalf("evidence = %#v", ev)
	}
	last := ev[nudges-1]
	for _, want := range []string{"continue nudge(s) sent without the pane moving", "last kick 2026-10-21T11:00:00Z", "3 continue nudge(s)"} {
		if !strings.Contains(last.Message, want) {
			t.Errorf("message %q missing %q", last.Message, want)
		}
	}
	for _, want := range []string{"agent=scanner", "lane=scanner", "stall_nudges=3", "pane tail", "frozen pane"} {
		if !strings.Contains(last.Diagnostics, want) {
			t.Errorf("diagnostics missing %q:\n%s", want, last.Diagnostics)
		}
	}
	if strings.Contains(last.Diagnostics, "ghp_abcdefghijklmnopqrstuvwxyz123456") {
		t.Fatalf("pane tail leaked a token:\n%s", last.Diagnostics)
	}
	// The pane moving before the next kick is the recovery that clears it.
	m.creditStallRecoveryLocked(agent, paneContentHash("moved on"))
	if ev := rec.active(crux.ClassAgentStall); len(ev) != 0 {
		t.Fatalf("recovery did not clear the stall: %#v", ev)
	}
	if agent.StallRecoveries != 1 {
		t.Fatalf("recoveries = %d", agent.StallRecoveries)
	}
}
