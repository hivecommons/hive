package toolapprove

// Migration parity: whatever a given ACMM level auto-permits TODAY, the desk
// auto-permits on day one.
//
// This is the constraint the maintainer's throughput addendum makes
// non-negotiable — "New restrictions arrive only as explicit, audited policy
// changes — never as conservative shipped defaults that silently downgrade
// every high-autonomy hive on upgrade day."
//
// The tests compare the desk against the LEGACY predicates themselves (the same
// config functions the production gates call), not against a hardcoded table.
// A future change to a threshold therefore moves both sides together and these
// keep passing — they pin EQUIVALENCE, which is what must not break.

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestSelfMergeParityAcrossLevels pins the desk against
// AutoMergeConfig.SelfAuthoredAutoMergeAllowed at every level.
func TestSelfMergeParityAcrossLevels(t *testing.T) {
	// A desk with NO operator rules is the day-one configuration: a hive that
	// enables the feature and writes no rules must behave exactly as before.
	desk := NewDesk(nil, passingScanner{})
	amc := config.AutoMergeConfig{SelfAuthored: boolPtr(true)}

	req := Request{
		Kind:        KindSelfMerge,
		Repo:        "hivecommons/hive",
		Number:      1,
		Author:      "hive-app[bot]",
		ChecksGreen: true,
		Tool:        ToolRequest{Tool: "hive-merge"},
	}

	for level := 0; level <= 7; level++ {
		legacyAllows := LegacyGateSelfMerge(amc, level)
		v := desk.Resolve(context.Background(), req, level)
		deskAllows := v.Decision == DecisionAutoApprove

		if deskAllows != legacyAllows {
			t.Errorf("L%d self-merge: legacy gate allows=%v, desk allows=%v (decision %s, rationale %q) — "+
				"migration must map current behavior 1:1",
				level, legacyAllows, deskAllows, v.Decision, v.Rationale)
		}
	}
}

// TestPlanApprovalParityAcrossLevels pins the desk against
// config.PlanAutoApproveForLevel.
func TestPlanApprovalParityAcrossLevels(t *testing.T) {
	desk := NewDesk(nil, passingScanner{})
	req := Request{Kind: KindPlanApproval, Tool: ToolRequest{Tool: "plan"}, Agent: AgentIdentity{Name: "architect"}}

	for level := 0; level <= 7; level++ {
		legacyAllows := LegacyGatePlanAutoApprove(level)
		v := desk.Resolve(context.Background(), req, level)
		deskAllows := v.Decision == DecisionAutoApprove

		if deskAllows != legacyAllows {
			t.Errorf("L%d plan approval: legacy PlanAutoApproveForLevel=%v, desk allows=%v (decision %s) — "+
				"the decomposition path must see identical behavior",
				level, legacyAllows, deskAllows, v.Decision)
		}
	}
}

// TestPlanFromLabelParityAcrossLevels pins the desk against
// PlanningConfig.PlanFromLabelEnabled with the default (absent-block) config.
func TestPlanFromLabelParityAcrossLevels(t *testing.T) {
	desk := NewDesk(nil, passingScanner{})
	req := Request{Kind: KindPlanFromLabel, Tool: ToolRequest{Tool: "plan"}, Agent: AgentIdentity{Name: "architect"}}

	for level := 0; level <= 7; level++ {
		legacyAllows := LegacyGatePlanFromLabel(config.PlanningConfig{}, level)
		v := desk.Resolve(context.Background(), req, level)
		deskAllows := v.Decision == DecisionAutoApprove

		if deskAllows != legacyAllows {
			t.Errorf("L%d plan-from-label: legacy PlanFromLabelEnabled=%v, desk allows=%v (decision %s)",
				level, legacyAllows, deskAllows, v.Decision)
		}
	}
}

// TestNoConservativeDowngradeAtHighAutonomy is the addendum's requirement 3
// stated as a single assertion: at L6, a hive with the desk enabled and NO
// rules configured must auto-permit everything it auto-permitted before.
//
// This is the test that would fail if someone shipped a "safe" default that
// routed high-autonomy traffic through the operator lane.
func TestNoConservativeDowngradeAtHighAutonomy(t *testing.T) {
	desk := NewDesk(nil, passingScanner{})

	// Every request an L6 hive resolves today without a human.
	for _, req := range []Request{
		{Kind: KindAgentTool, Tool: ToolRequest{Tool: "read", Arguments: map[string]any{"file_path": "a.go"}}, Agent: AgentIdentity{Name: "r"}},
		{Kind: KindAgentTool, Tool: ToolRequest{Tool: "write", Arguments: map[string]any{"file_path": "a.go", "content": "package a"}}, Agent: AgentIdentity{Name: "w"}},
		{Kind: KindAgentTool, Tool: ToolRequest{Tool: "bash", Arguments: map[string]any{"command": "go build ./..."}}, Agent: AgentIdentity{Name: "b"}},
		{Kind: KindSelfMerge, Repo: "hivecommons/hive", Number: 1, Author: "hive-app[bot]", ChecksGreen: true, Tool: ToolRequest{Tool: "hive-merge"}},
		{Kind: KindPlanApproval, Tool: ToolRequest{Tool: "plan"}, Agent: AgentIdentity{Name: "architect"}},
	} {
		v := desk.Resolve(context.Background(), req, 6)
		if v.Decision != DecisionAutoApprove {
			t.Errorf("L6 %s/%s resolved to %s, want auto-approve — enabling the desk with no rules "+
				"must not downgrade a high-autonomy hive (rationale: %s)",
				req.Kind, req.Tool.Tool, v.Decision, v.Rationale)
		}
	}
}

// TestUnknownKindFailsClosed pins that a request kind the desk does not know
// goes to the operator lane rather than auto-resolving.
func TestUnknownKindFailsClosed(t *testing.T) {
	desk := NewDesk(nil, passingScanner{})
	v := desk.Resolve(context.Background(), Request{Kind: "something-new", Tool: ToolRequest{Tool: "x"}}, 6)
	if v.Decision != DecisionOperatorApprove {
		t.Errorf("unknown request kind resolved to %s, want operator-approve (fail-closed)", v.Decision)
	}
}

// TestLegacyQueuedMergeGate documents the identity-based gate's mapping.
func TestLegacyQueuedMergeGate(t *testing.T) {
	if !LegacyGateQueuedMerge(true) {
		t.Error("a trusted-merger approval should permit a queued merge")
	}
	if LegacyGateQueuedMerge(false) {
		t.Error("an untrusted queuer must not permit a queued merge")
	}
}

func boolPtr(b bool) *bool { return &b }
