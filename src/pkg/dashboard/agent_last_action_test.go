package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
)

// resetAgentLastActions isolates a test from records left by another.
func resetAgentLastActions(t *testing.T) {
	t.Helper()
	agentLastActionsMu.Lock()
	agentLastActions = map[string]agentLastAction{}
	agentLastActionsMu.Unlock()
	t.Cleanup(func() {
		agentLastActionsMu.Lock()
		agentLastActions = map[string]agentLastAction{}
		agentLastActionsMu.Unlock()
	})
}

func lastActionAgentByName(t *testing.T, statuses map[string]*agent.AgentProcess, name string) FrontendAgent {
	t.Helper()
	cfg := &config.Config{
		Agents:   map[string]config.AgentConfig{},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{}},
	}
	for n := range statuses {
		cfg.Agents[n] = config.AgentConfig{Backend: "claude", Enabled: true}
	}
	payload := BuildAgentOnlyStatus(governor.State{Mode: "IDLE"}, statuses, cfg)
	for _, a := range payload.Agents {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("agent %q missing from payload", name)
	return FrontendAgent{}
}

// TestStaleWorkingAgentGetsDoingAndSummaryUpdated is the #10927 regression: a
// working agent whose last hive-mediated write is older than the card's stale
// threshold (20 min) must carry doing and summaryUpdated, the two fields the
// "may be stuck" warning and the age badge read and nothing used to set.
func TestStaleWorkingAgentGetsDoingAndSummaryUpdated(t *testing.T) {
	resetAgentLastActions(t)
	kick := time.Now().Add(-45 * time.Minute)
	acted := time.Now().Add(-30 * time.Minute)
	RecordAgentLastAction(github.AuditRecord{
		Action: github.AuditActionAgentCommentCreated, Agent: "scanner", Repo: "console", Target: 123,
	}, acted)

	a := lastActionAgentByName(t, map[string]*agent.AgentProcess{
		"scanner": {State: agent.StateRunning, LastKick: &kick},
	}, "scanner")

	if a.Busy != "working" {
		t.Fatalf("busy = %q, want working", a.Busy)
	}
	if a.Doing != "console#123" {
		t.Errorf("doing = %q, want console#123", a.Doing)
	}
	updated, err := time.Parse(time.RFC3339, a.SummaryUpdated)
	if err != nil {
		t.Fatalf("summaryUpdated %q is not RFC3339: %v", a.SummaryUpdated, err)
	}
	if age := time.Since(updated); age <= 20*time.Minute {
		t.Errorf("summaryUpdated age = %v, want older than the 20 min stale threshold", age)
	}
	if a.LastAction == nil || !a.LastAction.Current || a.LastAction.Number != 123 || a.LastAction.Repo != "console" {
		t.Errorf("lastAction = %+v, want current console#123", a.LastAction)
	}
}

// A working agent that has not acted yet this round measures its age from the
// round's start, so a long silence still trips the stale warning.
func TestWorkingAgentWithoutActionUsesRoundStart(t *testing.T) {
	resetAgentLastActions(t)
	kick := time.Now().Add(-25 * time.Minute)
	RecordAgentLastAction(github.AuditRecord{
		Action: github.AuditActionAgentCommentCreated, Agent: "scanner", Repo: "console", Target: 7,
	}, kick.Add(-time.Hour))

	a := lastActionAgentByName(t, map[string]*agent.AgentProcess{
		"scanner": {State: agent.StateRunning, LastKick: &kick},
	}, "scanner")

	if a.Doing != "" {
		t.Errorf("doing = %q, want empty: the action predates this round", a.Doing)
	}
	if a.SummaryUpdated != kick.UTC().Format(time.RFC3339) {
		t.Errorf("summaryUpdated = %q, want the round start %q", a.SummaryUpdated, kick.UTC().Format(time.RFC3339))
	}
	if a.LastAction == nil || a.LastAction.Current {
		t.Errorf("lastAction = %+v, want a past (not current) record", a.LastAction)
	}
}

// An idle or paused agent never claims a current item.
func TestIdleAgentHasNoDoing(t *testing.T) {
	resetAgentLastActions(t)
	kick := time.Now().Add(-10 * time.Minute)
	RecordAgentLastAction(github.AuditRecord{
		Action: github.AuditActionAgentPRCreated, Agent: "scanner", Repo: "console", Target: 9,
	}, time.Now().Add(-5*time.Minute))

	a := lastActionAgentByName(t, map[string]*agent.AgentProcess{
		"scanner": {State: agent.StateRunning, LastKick: &kick, Paused: true},
	}, "scanner")
	if a.Doing != "" || a.SummaryUpdated != "" {
		t.Errorf("paused agent doing=%q summaryUpdated=%q, want both empty", a.Doing, a.SummaryUpdated)
	}
}

// Only writes the agent caused move its item: the governor's own approve or
// merge, number-less writes and the hive's own labels are ignored, and the
// latest agent action wins.
func TestRecordAgentLastActionFilters(t *testing.T) {
	resetAgentLastActions(t)
	now := time.Now()
	RecordAgentLastAction(github.AuditRecord{Action: github.AuditActionAgentCommentCreated, Agent: "scanner", Repo: "console", Target: 123}, now.Add(-3*time.Minute))
	RecordAgentLastAction(github.AuditRecord{Action: github.AuditActionAgentPRCreated, Agent: "scanner", Repo: "console", Target: 456}, now.Add(-2*time.Minute))
	for _, rec := range []github.AuditRecord{
		{Action: github.AuditActionPRMerged, Agent: github.AttributionAgentGovernor, Repo: "console", Target: 1},
		{Action: github.AuditActionAgentBranchPushed, Agent: "scanner", Repo: "console"},
		{Action: github.AuditActionHiveLabelApplied, Agent: "scanner", Repo: "console", Target: 2},
		{Action: github.AuditActionAgentWriteRefused, Agent: "scanner", Repo: "console", Target: 3},
		{Action: github.AuditActionAgentCommentCreated, Agent: "", Repo: "console", Target: 4},
	} {
		RecordAgentLastAction(rec, now)
	}
	// An out-of-order older delivery never rewinds the record.
	RecordAgentLastAction(github.AuditRecord{Action: github.AuditActionAgentCommentCreated, Agent: "scanner", Repo: "console", Target: 5}, now.Add(-10*time.Minute))

	la, ok := lookupAgentLastAction("scanner")
	if !ok || la.number != 456 || la.action != github.AuditActionAgentPRCreated {
		t.Fatalf("last action = %+v ok=%v, want console#456 agent_pr_created", la, ok)
	}
	if _, ok := lookupAgentLastAction(github.AttributionAgentGovernor); ok {
		t.Error("the governor's own writes must not create a record")
	}
}

// The card's stale warning and age badge read exactly these payload fields;
// if the browser stops reading them this fix silently stops mattering.
func TestAgentCardReadsDoingAndSummaryUpdated(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		"const isStale = a.summaryUpdated && !isPaused && a.busy === 'working'",
		"if (!a.doing && a.summaryUpdated) {",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html no longer contains %q", want)
		}
	}
}
