package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/hub"
)

// quotaExhaustedAgentCount must count ONLY running, unpaused agents whose
// pane showed quota exhaustion — the summary-side twin of
// quotaExhaustedProcessCount. A paused or stopped agent out of quota is not an
// actionable heartbeat signal, and the state match must be case-insensitive
// because summaries carry whatever casing the producer used.
func TestQuotaExhaustedAgentCount(t *testing.T) {
	agents := []hub.AgentSummary{
		{Name: "counted", State: "running", QuotaExhausted: true},
		{Name: "mixed-case", State: "Running", QuotaExhausted: true},
		{Name: "paused-flag", State: "running", QuotaExhausted: true, Paused: true},
		{Name: "paused-state", State: "paused", QuotaExhausted: true},
		{Name: "stopped", State: "stopped", QuotaExhausted: true},
		{Name: "has-quota", State: "running", QuotaExhausted: false},
	}
	if got := hub.QuotaExhaustedAgentCount(agents); got != 2 {
		t.Errorf("quotaExhaustedAgentCount = %d, want 2 (only running+unpaused out-of-quota agents)", got)
	}
}

// A nil or empty summary slice must count zero — the heartbeat builder calls
// this before any agent has reported.
func TestQuotaExhaustedAgentCountEmpty(t *testing.T) {
	if got := hub.QuotaExhaustedAgentCount(nil); got != 0 {
		t.Errorf("hub.QuotaExhaustedAgentCount(nil) = %d, want 0", got)
	}
	if got := hub.QuotaExhaustedAgentCount([]hub.AgentSummary{}); got != 0 {
		t.Errorf("hub.QuotaExhaustedAgentCount(empty) = %d, want 0", got)
	}
}
