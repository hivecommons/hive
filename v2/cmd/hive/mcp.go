package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/hivemcp"
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

func runMCPTool(ctx context.Context, name string, arguments map[string]any) (any, error) {
	stateDir := stringArgument(arguments, "state_dir", defaultIntegratedStateDir())
	var args []string
	switch name {
	case "hive_setup_plan", "hive_setup_apply":
		args = []string{"setup", "--repo", stringArgument(arguments, "repo", ""), "--coverage", stringArgument(arguments, "coverage", ""), "--automation", stringArgument(arguments, "automation", ""), "--provider", stringArgument(arguments, "provider", "codex"), "--state-dir", stateDir, "--json"}
		args = append(args, "--max-active-issues", fmt.Sprint(integerArgument(arguments, "max_active_issues", 5)))
		if name == "hive_setup_plan" {
			args = append(args, "--plan")
		} else {
			args = append(args, "--start")
		}
		if booleanArgument(arguments, "visual_hive", true) {
			args = append(args, "--visual-hive")
		}
	case "hive_doctor":
		args = []string{"doctor", "--state-dir", stateDir, "--json"}
	case "hive_status":
		args = []string{"status", "--state-dir", stateDir, "--json"}
	case "hive_run":
		args = []string{"run", "--state-dir", stateDir, "--json"}
	case "hive_set_coverage":
		args = []string{"set-coverage", "--state-dir", stateDir, "--value", stringArgument(arguments, "value", ""), "--json"}
	case "hive_set_automation":
		args = []string{"set-automation", "--state-dir", stateDir, "--value", stringArgument(arguments, "value", ""), "--json"}
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
	default:
		return nil, fmt.Errorf("unknown Hive MCP tool %s", name)
	}
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
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
	var value map[string]any
	decodeErr := json.Unmarshal(stdout.Bytes(), &value)
	if decodeErr == nil {
		if runErr != nil {
			value["command_exit_error"] = runErr.Error()
		}
		return value, nil
	}
	if runErr != nil {
		return nil, fmt.Errorf("%s failed: %w: %s", strings.Join(args, " "), runErr, strings.TrimSpace(stderr.String()))
	}
	return nil, fmt.Errorf("%s returned invalid JSON: %w", strings.Join(args, " "), decodeErr)
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
