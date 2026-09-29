package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
)

func TestBuildAgents_BusyVisibilityFieldsAndBadge(t *testing.T) {
	busySince := time.Date(2026, 9, 29, 4, 16, 0, 0, time.UTC)
	lastActivity := busySince.Add(-47 * time.Minute)
	proc := &agent.AgentProcess{
		Name:                   "scanner",
		Config:                 config.AgentConfig{Backend: "copilot", Enabled: true},
		State:                  agent.StateRunning,
		OutputBuffer:           agent.NewRingBuffer(10),
		KicksUndeliverable:     5,
		BusySince:              busySince,
		LastTranscriptActivity: lastActivity,
		BusyCondition:          "busy-no-activity",
		BusyConditionMessage:   "pane shows Working but no transcript activity for 47m; 5 kicks undeliverable since 04:16Z",
	}

	got := buildOneAgent(t, proc)
	if got.KicksUndeliverable != 5 {
		t.Fatalf("kicksUndeliverable = %d, want 5", got.KicksUndeliverable)
	}
	if got.BusySince != busySince.Format(time.RFC3339) {
		t.Fatalf("busySince = %q, want %q", got.BusySince, busySince.Format(time.RFC3339))
	}
	if got.LastTranscriptActivity != lastActivity.Format(time.RFC3339) {
		t.Fatalf("lastTranscriptActivity = %q, want %q", got.LastTranscriptActivity, lastActivity.Format(time.RFC3339))
	}
	if got.Condition != "busy-no-activity" || !strings.Contains(got.ConditionMessage, "no transcript activity") {
		t.Fatalf("condition fields = %q/%q", got.Condition, got.ConditionMessage)
	}
	if got.StructuredStatus != "NEEDS_CONTEXT" || got.StatusEvidence != proc.BusyConditionMessage {
		t.Fatalf("structured status = %q/%q, want NEEDS_CONTEXT with busy evidence", got.StructuredStatus, got.StatusEvidence)
	}
}
