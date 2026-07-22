package main

import (
	"reflect"
	"testing"

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
