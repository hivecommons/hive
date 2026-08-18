package main

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestMCPSetupPreservesOmittedExistingPolicyAndExplicitIntegratedVisualHive(t *testing.T) {
	args, err := mcpCLIArgs("hive_setup_apply", map[string]any{"state_dir": "state", "visual_hive": true})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, absent := range []string{"--repo", "--coverage", "--automation", "--max-active-issues", "--max-repair-attempts", "--provider", "--auto-merge-path", "--auto-merge-risk"} {
		if strings.Contains(joined, absent) {
			t.Fatalf("omitted setup value was forced into MCP command: %s", joined)
		}
	}
	if !strings.Contains(joined, "--visual-hive=true") || !strings.Contains(joined, "--start") || !strings.Contains(joined, "--json") {
		t.Fatalf("integrated Visual Hive or apply behavior was lost: %s", joined)
	}
}

func TestMCPSetupAndUninstallPreserveOperatorOptions(t *testing.T) {
	setup, err := mcpCLIArgs("hive_setup_apply", map[string]any{"repo": "owner/repo", "runtime": "local", "start": false, "run_interval_seconds": 120})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(setup, " ")
	if strings.Contains(joined, "--start") || !strings.Contains(joined, "--runtime local") || !strings.Contains(joined, "--run-interval 120s") {
		t.Fatalf("MCP setup lost scheduler parity: %s", joined)
	}
	uninstall, err := mcpCLIArgs("hive_uninstall", map[string]any{"state_dir": "state", "delete_state": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(uninstall, " "), "--delete-state") {
		t.Fatalf("MCP uninstall lost explicit state-deletion parity: %v", uninstall)
	}
	if _, err := mcpCLIArgs("hive_uninstall", map[string]any{"state_dir": "state", "delete_state": true, "cancel": true}); err == nil {
		t.Fatal("MCP uninstall accepted cancellation combined with state deletion")
	}
}

func TestMCPProductionCommandParity(t *testing.T) {
	state := []string{"--state-dir", "state"}
	head, base, diff := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 64)
	correlation, requestDigest, planDigest := strings.Repeat("d", 64), strings.Repeat("e", 64), strings.Repeat("f", 64)
	tests := []struct {
		name      string
		arguments map[string]any
		want      []string
	}{
		{"hive_setup_plan", map[string]any{"state_dir": "state", "repo": "owner/repo", "coverage": "comprehensive", "automation": "auto-merge", "provider": "codex", "max_active_issues": float64(5), "max_repair_attempts": float64(4), "run_interval_seconds": float64(300), "auto_merge_paths": []any{"tests/**"}, "auto_merge_risks": []any{"automatic", "low"}, "visual_hive": true}, []string{"setup", "--json", "--state-dir", "state", "--repo", "owner/repo", "--coverage", "comprehensive", "--automation", "auto-merge", "--provider", "codex", "--max-active-issues", "5", "--max-repair-attempts", "4", "--run-interval", "300s", "--auto-merge-path", "tests/**", "--auto-merge-risk", "automatic", "--auto-merge-risk", "low", "--plan", "--visual-hive=true"}},
		{"hive_setup_apply", map[string]any{"state_dir": "state", "repo": "owner/repo", "coverage": "comprehensive", "automation": "auto-merge", "provider": "codex", "runtime": "local", "max_active_issues": float64(5), "max_repair_attempts": float64(4), "run_interval_seconds": float64(120), "start": true, "visual_hive": true}, []string{"setup", "--json", "--state-dir", "state", "--repo", "owner/repo", "--coverage", "comprehensive", "--automation", "auto-merge", "--provider", "codex", "--runtime", "local", "--max-active-issues", "5", "--max-repair-attempts", "4", "--run-interval", "120s", "--start", "--visual-hive=true"}},
		{"hive_doctor", map[string]any{"state_dir": "state"}, append([]string{"doctor"}, append(state, "--json")...)},
		{"hive_status", map[string]any{"state_dir": "state"}, append([]string{"status"}, append(state, "--json")...)},
		{"hive_run", map[string]any{"state_dir": "state", "timeout_seconds": float64(600)}, []string{"run", "--state-dir", "state", "--timeout", "600s", "--json"}},
		{"hive_start", map[string]any{"state_dir": "state", "interval_seconds": float64(120)}, []string{"start", "--state-dir", "state", "--interval", "120s", "--json"}},
		{"hive_stop", map[string]any{"state_dir": "state"}, []string{"stop", "--state-dir", "state", "--json"}},
		{"hive_plan_merge_approval", map[string]any{"state_dir": "state", "pr_number": float64(7), "head_sha": head}, []string{"approve-merge", "--state-dir", "state", "--pr", "7", "--head", head, "--plan", "--json"}},
		{"hive_approve_merge", map[string]any{"state_dir": "state", "pr_number": float64(7), "head_sha": head, "base_sha": base, "diff_digest": diff, "reason": "reviewed"}, []string{"approve-merge", "--state-dir", "state", "--pr", "7", "--head", head, "--base", base, "--diff-digest", diff, "--reason", "reviewed", "--json"}},
		{"hive_plan_baseline_approval", map[string]any{"state_dir": "state"}, []string{"approve-baseline", "--state-dir", "state", "--plan", "--json"}},
		{"hive_approve_baseline", map[string]any{"state_dir": "state", "repository_id": "123", "capture_run_id": float64(77), "artifact_id": float64(88), "pr_number": float64(9), "head_sha": head, "base_sha": base, "diff_digest": diff, "candidate_digest": requestDigest, "actor_id": float64(42), "plan_digest": planDigest, "reason": "reviewed hosted evidence"}, []string{"approve-baseline", "--state-dir", "state", "--repo-id", "123", "--run-id", "77", "--artifact-id", "88", "--pr", "9", "--head", head, "--base", base, "--diff-digest", diff, "--candidate-digest", requestDigest, "--actor-id", "42", "--plan-digest", planDigest, "--reason", "reviewed hosted evidence", "--json"}},
		{"hive_revoke_merge_approval", map[string]any{"state_dir": "state", "reason": "cancelled"}, []string{"revoke-merge-approval", "--state-dir", "state", "--reason", "cancelled", "--json"}},
		{"hive_retry_repair", map[string]any{"state_dir": "state", "finding": "fingerprint", "recurrence": float64(0), "attempt": float64(2), "failure_class": "infrastructure", "failure_id": "failure-1", "reason": "tool restored"}, []string{"retry-repair", "--state-dir", "state", "--finding", "fingerprint", "--recurrence", "0", "--attempt", "2", "--failure-class", "infrastructure", "--failure-id", "failure-1", "--reason", "tool restored", "--json"}},
		{"hive_plan_dispatch_recovery", map[string]any{"state_dir": "state", "action": "retry", "correlation": correlation}, []string{"recover-dispatch", "--state-dir", "state", "--action", "retry", "--correlation", correlation, "--plan", "--json"}},
		{"hive_recover_dispatch", map[string]any{"state_dir": "state", "action": "retry", "correlation": correlation, "request_digest": requestDigest, "plan_digest": planDigest, "planned_at": "2026-07-11T12:00:00Z", "reason": "transport verified"}, []string{"recover-dispatch", "--state-dir", "state", "--action", "retry", "--correlation", correlation, "--request-digest", requestDigest, "--plan-digest", planDigest, "--planned-at", "2026-07-11T12:00:00Z", "--reason", "transport verified", "--json"}},
		{"hive_transfer_setup_authorizer", map[string]any{"state_dir": "state", "new_authorizer": "next-operator", "reason": "planned handoff"}, []string{"transfer-setup-authorizer", "--state-dir", "state", "--new-authorizer", "next-operator", "--reason", "planned handoff", "--json"}},
		{"hive_transfer_setup_authorizer", map[string]any{"state_dir": "state", "cancel": true}, []string{"transfer-setup-authorizer", "--state-dir", "state", "--cancel", "--json"}},
		{"hive_set_coverage", map[string]any{"state_dir": "state", "value": "comprehensive"}, []string{"set-coverage", "--state-dir", "state", "--value", "comprehensive", "--json"}},
		{"hive_set_automation", map[string]any{"state_dir": "state", "value": "auto-merge"}, []string{"set-automation", "--state-dir", "state", "--value", "auto-merge", "--json"}},
		{"hive_set_issue_limit", map[string]any{"state_dir": "state", "value": float64(5)}, []string{"set-issue-limit", "--state-dir", "state", "--value", "5", "--json"}},
		{"hive_set_retry_limit", map[string]any{"state_dir": "state", "value": float64(4)}, []string{"set-retry-limit", "--state-dir", "state", "--value", "4", "--json"}},
		{"hive_pause", map[string]any{"state_dir": "state"}, []string{"pause", "--state-dir", "state", "--json"}},
		{"hive_resume", map[string]any{"state_dir": "state"}, []string{"resume", "--state-dir", "state", "--json"}},
		{"hive_upgrade", map[string]any{"state_dir": "state", "value": base}, []string{"upgrade", "--state-dir", "state", "--version", base, "--json"}},
		{"hive_rollback", map[string]any{"state_dir": "state", "value": head}, []string{"rollback", "--state-dir", "state", "--version", head, "--json"}},
		{"hive_uninstall", map[string]any{"state_dir": "state", "delete_state": true}, []string{"uninstall", "--state-dir", "state", "--json", "--delete-state"}},
		{"hive_uninstall", map[string]any{"state_dir": "state", "cancel": true}, []string{"uninstall", "--state-dir", "state", "--json", "--cancel"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args, err := mcpCLIArgs(test.name, test.arguments)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(args, test.want) {
				t.Fatalf("mapping = %#v, want %#v", args, test.want)
			}
		})
	}
}

func TestMCPProductionDefaultsAreStable(t *testing.T) {
	tests := []struct {
		name string
		want []string
	}{
		{"hive_setup_apply", []string{"setup", "--json", "--state-dir", "state", "--start"}},
		{"hive_run", []string{"run", "--state-dir", "state", "--timeout", "2700s", "--json"}},
		{"hive_start", []string{"start", "--state-dir", "state", "--interval", "900s", "--json"}},
	}
	for _, test := range tests {
		args, err := mcpCLIArgs(test.name, map[string]any{"state_dir": "state"})
		if err != nil || !reflect.DeepEqual(args, test.want) {
			t.Fatalf("%s defaults = %#v, %v; want %#v", test.name, args, err, test.want)
		}
	}
}

func TestMCPRejectsUnknownTool(t *testing.T) {
	if _, err := mcpCLIArgs("hive_not_a_tool", nil); err == nil || !strings.Contains(err.Error(), "unknown Hive MCP tool") {
		t.Fatalf("unknown MCP tool was not rejected clearly: %v", err)
	}
}

func TestMCPNonZeroStructuredCLIResultPreservesResultAndDiagnostic(t *testing.T) {
	result, err := decodeCLIResult([]string{"approve-merge"}, []byte(`{"error":"gate failed"}`), []byte("exact head is stale"), &exec.ExitError{})
	if err != nil || result["error"] != "gate failed" {
		t.Fatalf("structured CLI denial was not preserved: result=%v err=%v", result, err)
	}
	metadata, ok := result["hive_cli"].(map[string]any)
	if !ok || metadata["failed"] != true || metadata["diagnostic"] != "exact head is stale" {
		t.Fatalf("structured CLI denial lacks actionable metadata: %v", result)
	}
}

func TestMCPDispatchRecoveryPreservesExactPlanApplyBindings(t *testing.T) {
	correlation, requestDigest, planDigest := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	args, err := mcpCLIArgs("hive_recover_dispatch", map[string]any{
		"state_dir": "state", "action": "revoke", "correlation": correlation,
		"request_digest": requestDigest, "plan_digest": planDigest, "planned_at": "2026-07-11T12:00:00.123Z", "reason": "retire uncertain transport",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, binding := range []string{"recover-dispatch", "--action revoke", "--correlation " + correlation, "--request-digest " + requestDigest, "--plan-digest " + planDigest, "--planned-at 2026-07-11T12:00:00.123Z", "--reason retire uncertain transport", "--json"} {
		if !strings.Contains(joined, binding) {
			t.Errorf("MCP recovery command lost %q: %s", binding, joined)
		}
	}
}
