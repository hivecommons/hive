package main

import (
	"reflect"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/automation"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/repair"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

func TestExactWorkerCommandsUsesSafePreflightForCanonicalSelectorFinding(t *testing.T) {
	finding := visualhive.FindingLifecycle{
		IssueKind:         "selector_contract_failure",
		ValidationCommand: normalVisualExactHeadValidationCommand,
	}
	configured := [][]string{{"npm", "--prefix", "dashboard", "run", "test:ci"}}
	commands, err := exactWorkerCommands([]string{finding.ValidationCommand}, configured)
	if err != nil {
		t.Fatal(err)
	}
	want := []repair.Command{{Name: "git", Args: []string{"diff", "--check"}}}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("canonical exact-head validation local preflight = %+v, want %+v", commands, want)
	}
}

func TestExactWorkerCommandsRejectsUnconfiguredVisualHiveLookalikes(t *testing.T) {
	configured := [][]string{{"npm", "--prefix", "dashboard", "run", "test:ci"}}
	for _, command := range []string{
		"visual-hive run",
		"visual-hive run --ci ",
		"npx visual-hive run --ci",
		"visual-hive run --config visual-hive.config.yaml --ci",
		"visual-hive improve-coverage && visual-hive issues --write",
	} {
		t.Run(command, func(t *testing.T) {
			if _, err := exactWorkerCommands([]string{command}, configured); err == nil {
				t.Fatalf("unconfigured validation command %q did not fail closed", command)
			}
		})
	}
}

func TestNormalRepairRetirementRequiresCurrentL4RepairAuthority(t *testing.T) {
	config := integrated.Config{
		Repository: "owner/repo", Automation: integrated.AutomationRepairPR,
		ACMMLevel: 4, MaxRepairAttempts: 3,
	}
	finding := visualhive.FindingLifecycle{
		IssueKind: "visual_regression", OwningAgentHint: "hive/quality",
		ValidationCommand: "npm test", AffectedContracts: []string{"app"},
		RepairAttempts: 1,
	}
	decisions := normalRepairRetirementDecisions(config, 4, finding)
	if len(decisions) != 2 || decisions[0].Action != automation.ActionCloseRepairPR ||
		decisions[1].Action != automation.ActionDeleteRepairBranch ||
		!decisions[0].Allowed || !decisions[1].Allowed {
		t.Fatalf("L4 repair authority did not permit exact retirement: %+v", decisions)
	}
	config.Paused = true
	paused := normalRepairRetirementDecisions(config, 4, finding)
	if len(paused) != 2 || paused[0].Allowed || paused[1].Allowed {
		t.Fatalf("paused authority permitted remote retirement: %+v", paused)
	}
	config.Paused = false
	lowerNormalAuthority := normalRepairRetirementDecisions(config, 2, finding)
	if lowerNormalAuthority[0].Allowed || lowerNormalAuthority[1].Allowed {
		t.Fatalf("lower ordinary Hive ACMM permitted repair retirement: %+v", lowerNormalAuthority)
	}
}
