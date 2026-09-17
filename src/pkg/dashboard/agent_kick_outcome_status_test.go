package dashboard

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
)

// #7421: an agent sitting at its idle prompt after a fruitless kick reported
// state=running / busy=working — indistinguishable from an agent mid-task.
// These tests pin the dashboard half: a kicked turn the manager has seen END
// is idle whatever the process state says, a stand-down surfaces as BLOCKED
// with its reason, and a clarifying question is named on the card.

func outcomeProc(kickAt time.Time, outcome agent.KickOutcome) *agent.AgentProcess {
	k := kickAt
	return &agent.AgentProcess{
		Name:         "quality",
		Config:       config.AgentConfig{Backend: "claude", Enabled: true},
		State:        agent.StateRunning,
		OutputBuffer: agent.NewRingBuffer(10),
		LastKick:     &k,
		KickOutcome:  outcome,
	}
}

func TestBuildAgents_FinishedTurnIsNotWorking(t *testing.T) {
	kickAt := time.Now().Add(-10 * time.Minute)

	// Turn still running (no verdict yet): the historical reading stands.
	a := buildOneAgent(t, outcomeProc(kickAt, agent.KickOutcome{}))
	if a.Busy != "working" || a.KickOutcome != "" {
		t.Fatalf("running turn: busy=%q outcome=%q, want working and no outcome", a.Busy, a.KickOutcome)
	}

	// Turn ended on a clarifying question: idle, and the card says why.
	q := agent.KickOutcome{Kind: agent.KickOutcomeQuestion, Reason: "What should I focus on?", At: kickAt.Add(time.Minute), KickAt: kickAt}
	a = buildOneAgent(t, outcomeProc(kickAt, q))
	if a.Busy != "idle" {
		t.Errorf("question-ended turn: busy=%q, want idle — a pane at its prompt is not evidence of work (#7421)", a.Busy)
	}
	if a.KickOutcome != agent.KickOutcomeQuestion || a.KickOutcomeReason != "What should I focus on?" {
		t.Errorf("kickOutcome = %q/%q, want question with its reason", a.KickOutcome, a.KickOutcomeReason)
	}
	if a.StructuredStatus == "BLOCKED" {
		t.Error("a clarifying question is a defect, not a legitimate block")
	}

	// A verdict for an OLDER kick does not describe the current turn.
	stale := agent.KickOutcome{Kind: agent.KickOutcomeEnded, At: kickAt.Add(-time.Hour), KickAt: kickAt.Add(-2 * time.Hour)}
	a = buildOneAgent(t, outcomeProc(kickAt, stale))
	if a.Busy != "working" || a.KickOutcome != "" {
		t.Errorf("older verdict leaked onto the current turn: busy=%q outcome=%q", a.Busy, a.KickOutcome)
	}
}

func TestBuildAgents_StandDownIsBlockedWithReason(t *testing.T) {
	kickAt := time.Now().Add(-10 * time.Minute)
	sd := agent.KickOutcome{Kind: agent.KickOutcomeStandDown, Reason: "● STAND DOWN.", At: kickAt.Add(time.Minute), KickAt: kickAt}
	a := buildOneAgent(t, outcomeProc(kickAt, sd))
	if a.StructuredStatus != "BLOCKED" {
		t.Fatalf("StructuredStatus = %q, want BLOCKED for a policy stand-down", a.StructuredStatus)
	}
	if !strings.Contains(a.StatusEvidence, "policy stand-down") || !strings.Contains(a.StatusEvidence, "STAND DOWN") {
		t.Errorf("StatusEvidence = %q, want the stand-down named with its reason", a.StatusEvidence)
	}
	if a.Busy != "idle" {
		t.Errorf("busy = %q, want idle", a.Busy)
	}
}

// TestKickOutcomeBadgeWiredInDashboard: the card renders the question / no-op
// verdicts (the stand-down rides the existing BLOCKED badge).
func TestKickOutcomeBadgeWiredInDashboard(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"function kickOutcomeBadgeHtml(a)",
		"${kickOutcomeBadgeHtml(a)}",
		"a.kickOutcome === 'question'",
		"asked for direction",
		"a.kickOutcome === 'no-op'",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html is missing %q", want)
		}
	}
}

// TestQualityTemplateForbidsAskingTheOperator: the quality hold-gated template
// lacked the queue-directed language scanner's has, and never said "do not
// ask the operator" — which a weaker model read as permission to end its turn
// with "What should I focus on?". Both shipped copies must carry the rule.
func TestQualityTemplateForbidsAskingTheOperator(t *testing.T) {
	for _, p := range []string{"../../policies/quality-holdgated.md", "../policies/defaults/quality-holdgated.md"} {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		s := string(raw)
		for _, want := range []string{
			"never ask the operator what to do",
			"implementation queue, not just a diagnosis queue",
			"recorded as a failed kick",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("%s is missing %q", p, want)
			}
		}
	}
}
