package main

import (
	"strings"
	"testing"
)

func TestParseRepairCommandsUsesStructuredArguments(t *testing.T) {
	commands, err := parseRepairCommands([]string{`["npm","run","typecheck"]`, `["npm","test","--","--runInBand"]`})
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 || commands[0].Name != "npm" || len(commands[0].Args) != 2 || commands[0].Args[1] != "typecheck" {
		t.Fatalf("unexpected commands: %+v", commands)
	}
}

func TestParseRepairCommandsRejectsShellString(t *testing.T) {
	if _, err := parseRepairCommands([]string{`npm test && curl example.test`}); err == nil {
		t.Fatal("expected unstructured shell command to be rejected")
	}
}

func TestValidateVisualFetchApplyIdentityRequiresFullSourceArtifact(t *testing.T) {
	producerCommit := strings.Repeat("a", 40)
	if err := validateVisualFetchApplyIdentity("owner/repo", 42, 99, 0, producerCommit); err == nil {
		t.Fatal("trusted fetch-apply accepted no full source artifact id")
	}
	if err := validateVisualFetchApplyIdentity("owner/repo", 42, 99, 98, ""); err == nil || !strings.Contains(err.Error(), "--visual-hive-ref") {
		t.Fatalf("trusted fetch-apply accepted no pinned producer commit: %v", err)
	}
	if err := validateVisualFetchApplyIdentity("owner/repo", 42, 99, 98, "visual-hive-v0.4.0"); err == nil || !strings.Contains(err.Error(), "--visual-hive-ref") {
		t.Fatalf("trusted fetch-apply accepted a symbolic producer ref: %v", err)
	}
	if err := validateVisualFetchApplyIdentity("owner/repo", 42, 99, 98, producerCommit); err != nil {
		t.Fatalf("complete trusted fetch identity was rejected: %v", err)
	}
}

func TestTrustedVisualFetchPinsManagedWorkflowIdentity(t *testing.T) {
	if trustedVisualHiveWorkflowName != "Hive Visual Hive Production" || trustedVisualHiveWorkflowPath != ".github/workflows/hive-visual-hive.yml" {
		t.Fatalf("trusted fetch-apply workflow identity drifted: %q %q", trustedVisualHiveWorkflowName, trustedVisualHiveWorkflowPath)
	}
}
