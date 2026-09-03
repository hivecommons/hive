package main

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/toolapprove"
)

func TestApprovalDeskAllowsLegacyOperationNilDesk(t *testing.T) {
	if !approvalDeskAllowsLegacyOperation(context.Background(), nil, &config.Config{}, toolapprove.KindPlanFromLabel, "plan", "architect", nil) {
		t.Fatal("nil desk must preserve the legacy allowed operation")
	}
}

func TestApprovalDeskAllowsLegacyOperationCanWithhold(t *testing.T) {
	rules, err := toolapprove.CompileRules([]toolapprove.Rule{{
		Name:   "human-plan-label",
		Expr:   `request.kind == "plan-from-label"`,
		Action: toolapprove.RuleActionOperatorApprove,
	}}, nil)
	if err != nil {
		t.Fatalf("CompileRules: %v", err)
	}
	desk := toolapprove.NewDesk(rules, nil)
	level := 6
	cfg := &config.Config{ACMMLevel: &level}

	if approvalDeskAllowsLegacyOperation(context.Background(), desk, cfg, toolapprove.KindPlanFromLabel, "plan", "architect", nil) {
		t.Fatal("operator-approve rule should withhold the legacy operation")
	}
}
