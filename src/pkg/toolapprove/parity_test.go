package toolapprove

// Migration parity: whatever a given ACMM level auto-permits TODAY, the desk
// auto-permits on day one.
//
// This is the constraint the maintainer's throughput addendum makes
// non-negotiable — "New restrictions arrive only as explicit, audited policy
// changes — never as conservative shipped defaults that silently downgrade
// every high-autonomy hive on upgrade day."
//
// The tests compare the desk against the LEGACY predicates themselves, not
// against a hardcoded table, so a future change to a threshold moves both
// sides together and these keep passing — they pin EQUIVALENCE, which is what
// must not break. The self-merge gate's threshold (SelfMergeMinACMMLevel)
// lives in the configuration leaf and is pinned here; the plan gates live
// behind Hive's ACMM packs and PlanningConfig, outside the Crux boundary, so
// the desk reaches them through PlanPolicy and cmd/hive/approvalparity_test.go
// pins the desk against the real predicates at every level. Here the plan
// kinds are pinned to follow whatever PlanPolicy answers, and to fail closed
// without one.

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/crux/config"
)

// TestSelfMergeParityAcrossLevels pins the desk against the legacy
// self-merge threshold (SelfMergeMinACMMLevel, the constant
// AutoMergeConfig.SelfAuthoredAutoMergeAllowed compares against) at every
// level.
func TestSelfMergeParityAcrossLevels(t *testing.T) {
	// A desk with NO operator rules is the day-one configuration: a hive that
	// enables the feature and writes no rules must behave exactly as before.
	desk := NewDesk(nil, passingScanner{})

	req := Request{
		Kind:        KindSelfMerge,
		Repo:        "hivecommons/hive",
		Number:      1,
		Author:      "hive-app[bot]",
		ChecksGreen: true,
		Tool:        ToolRequest{Tool: "hive-merge"},
	}

	for level := 0; level <= 7; level++ {
		legacyAllows := level >= config.SelfMergeMinACMMLevel
		v := desk.Resolve(context.Background(), req, level)
		deskAllows := v.Decision == DecisionAutoApprove

		if deskAllows != legacyAllows {
			t.Errorf("L%d self-merge: legacy gate allows=%v, desk allows=%v (decision %s, rationale %q) — "+
				"migration must map current behavior 1:1",
				level, legacyAllows, deskAllows, v.Decision, v.Rationale)
		}
	}
}

// stepPolicy is a stand-in for the host's PlanPolicy: plans auto-approve
// from the given level up, label planning from one level higher.
type stepPolicy struct{ from int }

func (p stepPolicy) PlanAutoApprove(l int) bool { return l >= p.from }
func (p stepPolicy) PlanFromLabel(l int) bool   { return l >= p.from+1 }

// TestPlanApprovalParityAcrossLevels pins that the desk's plan-approval
// verdict is exactly the PlanPolicy's answer at every level.
func TestPlanApprovalParityAcrossLevels(t *testing.T) {
	desk := NewDesk(nil, passingScanner{})
	policy := stepPolicy{from: 4}
	desk.SetPlanPolicy(policy)
	req := Request{Kind: KindPlanApproval, Tool: ToolRequest{Tool: "plan"}, Agent: AgentIdentity{Name: "architect"}}

	for level := 0; level <= 7; level++ {
		legacyAllows := policy.PlanAutoApprove(level)
		v := desk.Resolve(context.Background(), req, level)
		deskAllows := v.Decision == DecisionAutoApprove

		if deskAllows != legacyAllows {
			t.Errorf("L%d plan approval: PlanPolicy=%v, desk allows=%v (decision %s) — "+
				"the decomposition path must see identical behavior",
				level, legacyAllows, deskAllows, v.Decision)
		}
	}
}

// TestPlanFromLabelParityAcrossLevels pins that the desk's plan-from-label
// verdict is exactly the PlanPolicy's answer at every level.
func TestPlanFromLabelParityAcrossLevels(t *testing.T) {
	desk := NewDesk(nil, passingScanner{})
	policy := stepPolicy{from: 4}
	desk.SetPlanPolicy(policy)
	req := Request{Kind: KindPlanFromLabel, Tool: ToolRequest{Tool: "plan"}, Agent: AgentIdentity{Name: "architect"}}

	for level := 0; level <= 7; level++ {
		legacyAllows := policy.PlanFromLabel(level)
		v := desk.Resolve(context.Background(), req, level)
		deskAllows := v.Decision == DecisionAutoApprove

		if deskAllows != legacyAllows {
			t.Errorf("L%d plan-from-label: PlanPolicy=%v, desk allows=%v (decision %s)",
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
//
// The plan kinds depend on the host's PlanPolicy; the leaf half of this test
// wires a policy that mirrors today's L6 pack, and cmd/hive's
// approvalparity_test.go runs the same assertion against the real packs.
func TestNoConservativeDowngradeAtHighAutonomy(t *testing.T) {
	desk := NewDesk(nil, passingScanner{})
	desk.SetPlanPolicy(stepPolicy{from: 6})

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

// TestPlanKindsFailClosedWithoutPolicy pins that a desk the host never gave a
// PlanPolicy sends the plan kinds to the operator lane at every level rather
// than guessing the pack's answer.
func TestPlanKindsFailClosedWithoutPolicy(t *testing.T) {
	desk := NewDesk(nil, passingScanner{})
	for _, kind := range []string{KindPlanApproval, KindPlanFromLabel} {
		for level := 0; level <= 7; level++ {
			v := desk.Resolve(context.Background(), Request{Kind: kind, Tool: ToolRequest{Tool: "plan"}, Agent: AgentIdentity{Name: "architect"}}, level)
			if v.Decision != DecisionOperatorApprove {
				t.Errorf("L%d %s without a PlanPolicy resolved to %s, want operator-approve (fail-closed)", level, kind, v.Decision)
			}
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
