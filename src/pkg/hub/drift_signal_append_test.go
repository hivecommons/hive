package hub

import (
	"testing"
	"time"
)

// appendDriftSignal is the read-time appender the My Hives roster uses to fold
// the restart-storm rollup into a hive's DriftReport (saas.go). It must keep
// Count in lockstep with Signals and apply "worst wins" to WorstSeverity —
// never downgrading a report that already carries a worse signal.

func TestAppendDriftSignal_NilReportIsNoOp(t *testing.T) {
	// Must not panic: the caller passes &result[i].Drift, but the guard is the
	// contract for any future caller holding a nil report.
	appendDriftSignal(nil, DriftKindAgentRestartStorm, DriftCritical, "boom")
}

func TestAppendDriftSignal_AppendsAndStampsWorstSeverity(t *testing.T) {
	var report DriftReport
	appendDriftSignal(&report, DriftKindAgentRestartStorm, DriftCritical, "2 agent(s) restarted")

	if report.Count != 1 || len(report.Signals) != 1 {
		t.Fatalf("Count=%d Signals=%d, want 1/1", report.Count, len(report.Signals))
	}
	s := report.Signals[0]
	if s.Kind != DriftKindAgentRestartStorm || s.Severity != DriftCritical || s.Reason != "2 agent(s) restarted" {
		t.Fatalf("appended signal = %+v", s)
	}
	if report.WorstSeverity != DriftCritical {
		t.Fatalf("WorstSeverity = %q, want %q", report.WorstSeverity, DriftCritical)
	}
}

func TestAppendDriftSignal_EscalatesButNeverDowngrades(t *testing.T) {
	var report DriftReport

	appendDriftSignal(&report, "kind-a", DriftInfo, "informational")
	if report.WorstSeverity != DriftInfo {
		t.Fatalf("after info: WorstSeverity = %q, want %q", report.WorstSeverity, DriftInfo)
	}

	appendDriftSignal(&report, "kind-b", DriftCritical, "critical fault")
	if report.WorstSeverity != DriftCritical {
		t.Fatalf("after critical: WorstSeverity = %q, want %q", report.WorstSeverity, DriftCritical)
	}

	// A later, milder signal is still appended but must not lower the verdict.
	appendDriftSignal(&report, "kind-c", DriftWarn, "warning")
	if report.WorstSeverity != DriftCritical {
		t.Fatalf("warn after critical downgraded WorstSeverity to %q", report.WorstSeverity)
	}
	if report.Count != 3 || len(report.Signals) != 3 {
		t.Fatalf("Count=%d Signals=%d, want 3/3", report.Count, len(report.Signals))
	}
}

// applyAgentRestartResetBaselines edge branches: the happy path (fresh reset
// adjusts Last24h) is covered in agent_capability_test.go; these pin the
// guards around it.

func TestApplyAgentRestartResetBaselines_EmptyResetsIsNoOp(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	agents := []AgentSummary{{Name: "scanner", Restarts: AgentRestartTelemetry{Total: 5, Last24h: 4}}}
	applyAgentRestartResetBaselines(agents, nil, now)
	if agents[0].Restarts.Last24h != 4 || agents[0].Restarts.ResetAt != "" {
		t.Fatalf("nil resets mutated agent telemetry: %+v", agents[0].Restarts)
	}
}

func TestApplyAgentRestartResetBaselines_AgentWithoutResetUntouched(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	agents := []AgentSummary{{Name: "guide", Restarts: AgentRestartTelemetry{Total: 7, Last24h: 6}}}
	applyAgentRestartResetBaselines(agents, map[string]AgentRestartReset{
		"scanner": {ResetAt: now.Format(time.RFC3339), TotalBaseline: 1},
	}, now)
	if agents[0].Restarts.Last24h != 6 || agents[0].Restarts.ResetAt != "" {
		t.Fatalf("reset for another agent leaked onto guide: %+v", agents[0].Restarts)
	}
}

func TestApplyAgentRestartResetBaselines_UnparseableResetAtKeepsAttributionOnly(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	agents := []AgentSummary{{Name: "scanner", Restarts: AgentRestartTelemetry{Total: 9, Last24h: 8}}}
	applyAgentRestartResetBaselines(agents, map[string]AgentRestartReset{
		"scanner": {ResetAt: "not-a-timestamp", By: "andy", TotalBaseline: 3},
	}, now)
	r := agents[0].Restarts
	if r.ResetAt != "not-a-timestamp" || r.ResetBy != "andy" {
		t.Fatalf("attribution not stamped: %+v", r)
	}
	if r.Last24h != 8 {
		t.Fatalf("unparseable ResetAt must not adjust Last24h: got %d, want 8", r.Last24h)
	}
}

func TestApplyAgentRestartResetBaselines_ResetOlderThan24hExpires(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	agents := []AgentSummary{{Name: "scanner", Restarts: AgentRestartTelemetry{Total: 9, Last24h: 8}}}
	applyAgentRestartResetBaselines(agents, map[string]AgentRestartReset{
		"scanner": {ResetAt: now.Add(-25 * time.Hour).Format(time.RFC3339), By: "andy", TotalBaseline: 3},
	}, now)
	r := agents[0].Restarts
	if r.Last24h != 8 {
		t.Fatalf("stale reset (>24h) must not adjust Last24h: got %d, want 8", r.Last24h)
	}
	if r.ResetBy != "andy" {
		t.Fatalf("stale reset should still record attribution: %+v", r)
	}
}

func TestApplyAgentRestartResetBaselines_NegativeDeltaClampsToZero(t *testing.T) {
	// A baseline above the reported total (spoke restarted and re-counted from
	// zero) must clamp to 0, never go negative.
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	agents := []AgentSummary{{Name: "scanner", Restarts: AgentRestartTelemetry{Total: 2, Last24h: 2}}}
	applyAgentRestartResetBaselines(agents, map[string]AgentRestartReset{
		"scanner": {ResetAt: now.Add(-time.Hour).Format(time.RFC3339), TotalBaseline: 10},
	}, now)
	if got := agents[0].Restarts.Last24h; got != 0 {
		t.Fatalf("Last24h = %d, want clamp to 0", got)
	}
}

// commonProblemReason folds per-agent blocked reasons into the fleet rollup's
// single reason string (agent_capability.go): first reason wins, an identical
// second reason keeps it, and any disagreement collapses to "mixed".

func TestCommonProblemReason(t *testing.T) {
	cases := []struct {
		name, current, next, want string
	}{
		{"blank next keeps current", "provider quota", "", "provider quota"},
		{"whitespace next keeps current", "provider quota", "   ", "provider quota"},
		{"first reason adopted", "", "login required", "login required"},
		{"identical reasons stay", "login required", "login required", "login required"},
		{"disagreement collapses to mixed", "login required", "provider quota", "mixed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commonProblemReason(tc.current, tc.next); got != tc.want {
				t.Fatalf("commonProblemReason(%q, %q) = %q, want %q", tc.current, tc.next, got, tc.want)
			}
		})
	}
}
