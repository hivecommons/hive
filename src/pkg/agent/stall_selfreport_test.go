package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/crux/config"
	"github.com/hivecommons/hive/pkg/fleetreport"
)

// A swallowed "continue" nudge feeds the agent_stall self-report with the
// kick/pane context the retired dashboard counter never had (#13470); the
// streak files once at the threshold and a credited recovery clears it.
func TestObserveStallNudgeFeedsSelfReportAndRecoveryClears(t *testing.T) {
	tr := fleetreport.NewRuntimeTracker(nil)
	fleetreport.SetDefaultTrackerForTest(t, tr)
	kickAt := time.Date(2026, 10, 21, 11, 0, 0, 0, time.UTC)
	agent := &AgentProcess{Name: "scanner", Config: config.AgentConfig{Role: "scanner"}, lastInferKickAt: kickAt, lastInferKickPane: paneContentHash("frozen pane"), stallNudgeSent: true}
	pane := "line one\ntoken ghp_abcdefghijklmnopqrstuvwxyz123456\nfrozen pane"
	for i := 0; i < fleetreport.AgentStallNudgeStreak-1; i++ {
		agent.StallNudges++
		observeStallNudgeLocked(agent, pane, kickAt.Add(5*time.Minute))
	}
	if ev := stallEvidence(tr); len(ev) != 0 {
		t.Fatalf("below the streak produced evidence: %#v", ev)
	}
	agent.StallNudges++
	observeStallNudgeLocked(agent, pane, kickAt.Add(5*time.Minute))
	ev := stallEvidence(tr)
	if len(ev) != 1 || ev[0].Agent != "scanner" || ev[0].Count != fleetreport.AgentStallNudgeStreak {
		t.Fatalf("evidence = %#v", ev)
	}
	for _, want := range []string{"continue nudge(s) sent without the pane moving", "last kick 2026-10-21T11:00:00Z"} {
		if !strings.Contains(ev[0].Message, want) {
			t.Errorf("message %q missing %q", ev[0].Message, want)
		}
	}
	for _, want := range []string{"agent=scanner", "lane=scanner", "stall_nudges=3", "pane tail", "frozen pane"} {
		if !strings.Contains(ev[0].Diagnostics, want) {
			t.Errorf("diagnostics missing %q:\n%s", want, ev[0].Diagnostics)
		}
	}
	if strings.Contains(ev[0].Diagnostics, "ghp_abcdefghijklmnopqrstuvwxyz123456") {
		t.Fatalf("pane tail leaked a token:\n%s", ev[0].Diagnostics)
	}
	// The pane moving before the next kick is the recovery that clears it.
	creditStallRecoveryLocked(agent, paneContentHash("moved on"))
	if ev := stallEvidence(tr); len(ev) != 0 {
		t.Fatalf("recovery did not clear the stall: %#v", ev)
	}
	if agent.StallRecoveries != 1 {
		t.Fatalf("recoveries = %d", agent.StallRecoveries)
	}
}

func stallEvidence(tr *fleetreport.RuntimeTracker) []fleetreport.Evidence {
	var out []fleetreport.Evidence
	for _, e := range tr.Evidence() {
		if e.Class == fleetreport.ClassAgentStall {
			out = append(out, e)
		}
	}
	return out
}
