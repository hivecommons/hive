package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/hivemcp"
	"github.com/kubestellar/hive/v2/pkg/logscrub"
)

func runMCPServer() int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := hivemcp.Serve(ctx, os.Stdin, os.Stdout, func(callCtx context.Context, name string, arguments map[string]any) (any, error) {
		return runMCPTool(callCtx, name, arguments)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "Hive MCP server:", err)
		return 1
	}
	return 0
}

func mcpCLIArgs(name string, arguments map[string]any) ([]string, error) {
	stateDir := stringArgument(arguments, "state_dir", defaultIntegratedStateDir())
	var args []string
	switch name {
	case "hive_setup_plan", "hive_setup_apply":
		args = []string{"setup", "--json"}
		if explicitState, ok := arguments["state_dir"].(string); ok && strings.TrimSpace(explicitState) != "" {
			args = append(args, "--state-dir", explicitState)
		}
		for _, argument := range []struct {
			name string
			flag string
		}{{"repo", "--repo"}, {"coverage", "--coverage"}, {"automation", "--automation"}, {"provider", "--provider"}, {"runtime", "--runtime"}} {
			if value, ok := arguments[argument.name].(string); ok && strings.TrimSpace(value) != "" {
				args = append(args, argument.flag, value)
			}
		}
		for _, argument := range []struct {
			name string
			flag string
		}{{"max_active_issues", "--max-active-issues"}, {"max_repair_attempts", "--max-repair-attempts"}} {
			if value, ok := arguments[argument.name]; ok {
				args = append(args, argument.flag, fmt.Sprint(value))
			}
		}
		if value, ok := arguments["run_interval_seconds"]; ok {
			args = append(args, "--run-interval", fmt.Sprint(value)+"s")
		}
		for _, value := range stringSliceArgument(arguments, "auto_merge_paths") {
			args = append(args, "--auto-merge-path", value)
		}
		for _, value := range stringSliceArgument(arguments, "auto_merge_risks") {
			args = append(args, "--auto-merge-risk", value)
		}
		if name == "hive_setup_plan" {
			args = append(args, "--plan")
		} else {
			start := true
			if value, ok := arguments["start"].(bool); ok {
				start = value
			}
			if start {
				args = append(args, "--start")
			}
		}
		if value, ok := arguments["visual_hive"].(bool); ok {
			args = append(args, fmt.Sprintf("--visual-hive=%t", value))
		}
	case "hive_doctor":
		args = []string{"doctor", "--state-dir", stateDir, "--json"}
	case "hive_status":
		args = []string{"status", "--state-dir", stateDir, "--json"}
	case "hive_run":
		timeoutSeconds := integerArgument(arguments, "timeout_seconds", 2700)
		args = []string{"run", "--state-dir", stateDir, "--timeout", fmt.Sprintf("%ds", timeoutSeconds), "--json"}
	case "hive_start":
		args = []string{"start", "--state-dir", stateDir, "--interval", fmt.Sprintf("%ds", integerArgument(arguments, "interval_seconds", 900)), "--json"}
	case "hive_stop":
		args = []string{"stop", "--state-dir", stateDir, "--json"}
	case "hive_plan_merge_approval":
		args = []string{"approve-merge", "--state-dir", stateDir, "--pr", fmt.Sprint(integerArgument(arguments, "pr_number", 0)), "--head", stringArgument(arguments, "head_sha", ""), "--plan", "--json"}
	case "hive_approve_merge":
		args = []string{"approve-merge", "--state-dir", stateDir, "--pr", fmt.Sprint(integerArgument(arguments, "pr_number", 0)), "--head", stringArgument(arguments, "head_sha", ""), "--base", stringArgument(arguments, "base_sha", ""), "--diff-digest", stringArgument(arguments, "diff_digest", ""), "--reason", stringArgument(arguments, "reason", ""), "--json"}
	case "hive_plan_baseline_approval":
		args = []string{"approve-baseline", "--state-dir", stateDir, "--plan", "--json"}
	case "hive_approve_baseline":
		args = []string{"approve-baseline", "--state-dir", stateDir,
			"--repo-id", stringArgument(arguments, "repository_id", ""), "--run-id", fmt.Sprint(integerArgument(arguments, "capture_run_id", 0)),
			"--artifact-id", fmt.Sprint(integerArgument(arguments, "artifact_id", 0)), "--pr", fmt.Sprint(integerArgument(arguments, "pr_number", 0)),
			"--head", stringArgument(arguments, "head_sha", ""), "--base", stringArgument(arguments, "base_sha", ""),
			"--diff-digest", stringArgument(arguments, "diff_digest", ""), "--candidate-digest", stringArgument(arguments, "candidate_digest", ""),
			"--actor-id", fmt.Sprint(integerArgument(arguments, "actor_id", 0)), "--plan-digest", stringArgument(arguments, "plan_digest", ""),
			"--reason", stringArgument(arguments, "reason", ""), "--json"}
	case "hive_revoke_merge_approval":
		args = []string{"revoke-merge-approval", "--state-dir", stateDir, "--reason", stringArgument(arguments, "reason", ""), "--json"}
	case "hive_retry_repair":
		args = []string{"retry-repair", "--state-dir", stateDir, "--finding", stringArgument(arguments, "finding", ""), "--recurrence", fmt.Sprint(integerArgument(arguments, "recurrence", -1)), "--attempt", fmt.Sprint(integerArgument(arguments, "attempt", 0)), "--failure-class", stringArgument(arguments, "failure_class", ""), "--failure-id", stringArgument(arguments, "failure_id", ""), "--reason", stringArgument(arguments, "reason", ""), "--json"}
	case "hive_plan_dispatch_recovery":
		args = []string{"recover-dispatch", "--state-dir", stateDir, "--action", stringArgument(arguments, "action", ""), "--correlation", stringArgument(arguments, "correlation", ""), "--plan", "--json"}
	case "hive_recover_dispatch":
		args = []string{"recover-dispatch", "--state-dir", stateDir, "--action", stringArgument(arguments, "action", ""), "--correlation", stringArgument(arguments, "correlation", ""), "--request-digest", stringArgument(arguments, "request_digest", ""), "--plan-digest", stringArgument(arguments, "plan_digest", ""), "--planned-at", stringArgument(arguments, "planned_at", ""), "--reason", stringArgument(arguments, "reason", ""), "--json"}
	case "hive_transfer_setup_authorizer":
		if value, ok := arguments["cancel"].(bool); ok && value {
			args = []string{"transfer-setup-authorizer", "--state-dir", stateDir, "--cancel", "--json"}
		} else {
			args = []string{"transfer-setup-authorizer", "--state-dir", stateDir, "--new-authorizer", stringArgument(arguments, "new_authorizer", ""), "--reason", stringArgument(arguments, "reason", ""), "--json"}
		}
	case "hive_set_coverage":
		args = []string{"set-coverage", "--state-dir", stateDir, "--value", stringArgument(arguments, "value", ""), "--json"}
	case "hive_set_automation":
		args = []string{"set-automation", "--state-dir", stateDir, "--value", stringArgument(arguments, "value", ""), "--json"}
	case "hive_set_issue_limit":
		args = []string{"set-issue-limit", "--state-dir", stateDir, "--value", fmt.Sprint(integerArgument(arguments, "value", 0)), "--json"}
	case "hive_set_retry_limit":
		args = []string{"set-retry-limit", "--state-dir", stateDir, "--value", fmt.Sprint(integerArgument(arguments, "value", 0)), "--json"}
	case "hive_pause":
		args = []string{"pause", "--state-dir", stateDir, "--json"}
	case "hive_resume":
		args = []string{"resume", "--state-dir", stateDir, "--json"}
	case "hive_upgrade":
		args = []string{"upgrade", "--state-dir", stateDir, "--version", stringArgument(arguments, "value", ""), "--json"}
	case "hive_rollback":
		args = []string{"rollback", "--state-dir", stateDir, "--version", stringArgument(arguments, "value", ""), "--json"}
	case "hive_uninstall":
		args = []string{"uninstall", "--state-dir", stateDir, "--json"}
		if value, ok := arguments["cancel"].(bool); ok && value {
			if deleteState, _ := arguments["delete_state"].(bool); deleteState {
				return nil, fmt.Errorf("hive_uninstall cancel cannot be combined with delete_state")
			}
			args = append(args, "--cancel")
			break
		}
		if value, ok := arguments["delete_state"].(bool); ok && value {
			args = append(args, "--delete-state")
		}
	default:
		return nil, fmt.Errorf("unknown Hive MCP tool %s", name)
	}
	return args, nil
}

func stringSliceArgument(arguments map[string]any, name string) []string {
	raw, exists := arguments[name]
	if !exists {
		return nil
	}
	result := []string{}
	switch values := raw.(type) {
	case []string:
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				result = append(result, value)
			}
		}
	case []any:
		for _, rawValue := range values {
			if value, ok := rawValue.(string); ok && strings.TrimSpace(value) != "" {
				result = append(result, value)
			}
		}
	}
	return result
}

func runMCPTool(ctx context.Context, name string, arguments map[string]any) (any, error) {
	args, err := mcpCLIArgs(name, arguments)
	if err != nil {
		return nil, err
	}
	deadline := 30 * time.Minute
	if name == "hive_run" {
		// The MCP supervisor must always outlive the CLI's bounded production
		// run so it can return the CLI's structured result instead of killing a
		// healthy 45-minute default run at minute 30.
		deadline = time.Duration(integerArgument(arguments, "timeout_seconds", 2700))*time.Second + 5*time.Minute
	}
	requestCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	return runCLIJSON(requestCtx, args)
}

func runCLIJSON(ctx context.Context, args []string) (map[string]any, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, executable, args...)
	command.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	runErr := command.Run()
	return decodeCLIResult(args, stdout.Bytes(), stderr.Bytes(), runErr)
}

func decodeCLIResult(args []string, stdout, stderr []byte, runErr error) (map[string]any, error) {
	var value map[string]any
	decodeErr := json.Unmarshal(stdout, &value)
	if decodeErr == nil {
		if runErr != nil {
			exitCode := -1
			var exitErr *exec.ExitError
			if errors.As(runErr, &exitErr) {
				exitCode = exitErr.ExitCode()
			}
			metadata := map[string]any{"exit_code": exitCode, "failed": true}
			if diagnostic := boundedMCPDiagnostic(stderr); diagnostic != "" {
				metadata["diagnostic"] = diagnostic
			}
			value["hive_cli"] = metadata
		}
		return value, nil
	}
	if runErr != nil {
		return nil, fmt.Errorf("%s failed: %w: %s", strings.Join(args, " "), runErr, strings.TrimSpace(string(stderr)))
	}
	return nil, fmt.Errorf("%s returned invalid JSON: %w", strings.Join(args, " "), decodeErr)
}

func boundedMCPDiagnostic(value []byte) string {
	diagnostic := strings.TrimSpace(logscrub.Scrub(string(value)))
	const maxDiagnosticRunes = 2048
	runes := []rune(diagnostic)
	if len(runes) > maxDiagnosticRunes {
		diagnostic = string(runes[:maxDiagnosticRunes]) + "…"
	}
	return diagnostic
}

func stringArgument(arguments map[string]any, name, fallback string) string {
	if value, ok := arguments[name].(string); ok && strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func booleanArgument(arguments map[string]any, name string, fallback bool) bool {
	if value, ok := arguments[name].(bool); ok {
		return value
	}
	return fallback
}

func integerArgument(arguments map[string]any, name string, fallback int) int {
	if value, ok := arguments[name].(float64); ok {
		return int(value)
	}
	if value, ok := arguments[name].(int); ok {
		return value
	}
	return fallback
}
