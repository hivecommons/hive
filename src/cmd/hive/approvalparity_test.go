package main

// Host-side migration parity for the approval desk's plan kinds. The desk
// lives inside the Crux boundary and reaches the ACMM packs and
// PlanningConfig only through toolapprove.PlanPolicy, so the tests that pin
// it against the REAL legacy predicates — config.PlanAutoApproveForLevel,
// PlanningConfig.PlanFromLabelEnabled and
// AutoMergeConfig.SelfAuthoredAutoMergeAllowed — live here, next to the
// wiring (planPolicyFromConfig) that installs them. See the addendum quoted
// in pkg/toolapprove/parity_test.go: whatever a level auto-permits today, the
// desk auto-permits on day one.

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/crux/toolapprove"
)

func parityDesk(t *testing.T, cfg *config.Config) *toolapprove.Desk {
	t.Helper()
	desk := toolapprove.NewDesk(nil, nil)
	desk.SetPlanPolicy(planPolicyFromConfig(cfg))
	return desk
}

func TestBuildApprovalDeskInstallsPlanPolicy(t *testing.T) {
	cfg := &config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{
		ToolApproval: config.ToolApprovalConfig{Enabled: true, InboxPath: t.TempDir() + "/inbox.json"},
	}}
	desk, _ := buildApprovalDesk(cfg, nil)
	if desk == nil {
		t.Fatal("enabled config produced no desk")
	}
	req := toolapprove.Request{Kind: toolapprove.KindPlanApproval, Tool: toolapprove.ToolRequest{Tool: "plan"}, Agent: toolapprove.AgentIdentity{Name: "architect"}}
	for level := 0; level <= 7; level++ {
		want := config.PlanAutoApproveForLevel(level)
		got := desk.Resolve(context.Background(), req, level).Decision == toolapprove.DecisionAutoApprove
		if got != want {
			t.Errorf("L%d: buildApprovalDesk desk allows=%v, PlanAutoApproveForLevel=%v", level, got, want)
		}
	}
	if d, i := buildApprovalDesk(nil, nil); d != nil || i != nil {
		t.Error("nil config must yield no desk")
	}
}

func TestPlanApprovalParityAcrossLevels(t *testing.T) {
	desk := parityDesk(t, &config.Config{})
	req := toolapprove.Request{Kind: toolapprove.KindPlanApproval, Tool: toolapprove.ToolRequest{Tool: "plan"}, Agent: toolapprove.AgentIdentity{Name: "architect"}}
	for level := 0; level <= 7; level++ {
		legacyAllows := config.PlanAutoApproveForLevel(level)
		v := desk.Resolve(context.Background(), req, level)
		if deskAllows := v.Decision == toolapprove.DecisionAutoApprove; deskAllows != legacyAllows {
			t.Errorf("L%d plan approval: legacy PlanAutoApproveForLevel=%v, desk allows=%v (decision %s)", level, legacyAllows, deskAllows, v.Decision)
		}
	}
}

func TestPlanFromLabelParityAcrossLevels(t *testing.T) {
	// The zero PlanningConfig carries the same defaults the loader applies
	// when the block is absent — an unconfigured hive today.
	desk := parityDesk(t, &config.Config{})
	req := toolapprove.Request{Kind: toolapprove.KindPlanFromLabel, Tool: toolapprove.ToolRequest{Tool: "plan"}, Agent: toolapprove.AgentIdentity{Name: "architect"}}
	for level := 0; level <= 7; level++ {
		legacyAllows := config.PlanningConfig{}.PlanFromLabelEnabled(level)
		v := desk.Resolve(context.Background(), req, level)
		if deskAllows := v.Decision == toolapprove.DecisionAutoApprove; deskAllows != legacyAllows {
			t.Errorf("L%d plan-from-label: legacy PlanFromLabelEnabled=%v, desk allows=%v (decision %s)", level, legacyAllows, deskAllows, v.Decision)
		}
	}
}

func TestSelfMergeParityAgainstAutoMergeConfig(t *testing.T) {
	desk := parityDesk(t, &config.Config{})
	enabled := true
	amc := config.AutoMergeConfig{SelfAuthored: &enabled}
	req := toolapprove.Request{Kind: toolapprove.KindSelfMerge, Repo: "hivecommons/hive", Number: 1, Author: "hive-app[bot]", ChecksGreen: true, Tool: toolapprove.ToolRequest{Tool: "hive-merge"}}
	for level := 0; level <= 7; level++ {
		l := level
		legacyAllows := amc.SelfAuthoredAutoMergeAllowed(&l)
		v := desk.Resolve(context.Background(), req, level)
		if deskAllows := v.Decision == toolapprove.DecisionAutoApprove; deskAllows != legacyAllows {
			t.Errorf("L%d self-merge: legacy gate allows=%v, desk allows=%v (decision %s)", level, legacyAllows, deskAllows, v.Decision)
		}
	}
}

// TestNoConservativeDowngradeAtHighAutonomy: at L6, a hive with the desk
// enabled, NO rules and the real packs wired must auto-permit a plan exactly
// as the decomposition path does today.
func TestNoConservativeDowngradeAtHighAutonomy(t *testing.T) {
	if !config.PlanAutoApproveForLevel(6) {
		t.Skip("L6 pack no longer auto-approves plans; parity test below covers the mapping")
	}
	desk := parityDesk(t, &config.Config{})
	req := toolapprove.Request{Kind: toolapprove.KindPlanApproval, Tool: toolapprove.ToolRequest{Tool: "plan"}, Agent: toolapprove.AgentIdentity{Name: "architect"}}
	if v := desk.Resolve(context.Background(), req, 6); v.Decision != toolapprove.DecisionAutoApprove {
		t.Errorf("L6 plan approval resolved to %s, want auto-approve (rationale: %s)", v.Decision, v.Rationale)
	}
}
