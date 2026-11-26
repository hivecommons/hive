package agent

// Per-backend launch command and effort-flag builders (omp, opencode, codex,
// claude, agy). Moved out of manager_launch.go, which is held to a line-count
// ratchet, so a new backend's builder does not have to fit under its cap.

import (
	"fmt"
	"os"
	"strings"

	"github.com/hivecommons/hive/crux/config"
)

const ompDefaultApprovalMode = "yolo"

// opencodeLaunchCmd starts opencode's TUI for a hive agent. --standalone gives
// each agent its own private server instead of sharing one background service
// across agent UIDs; --auto approves permissions that are not explicitly
// denied, because nobody is attached to an unattended pane to answer a prompt
// (the pod and its egress proxy are the boundary, as for claude and codex). The
// TUI has no --model flag: the model rides in OPENCODE_CONFIG_CONTENT, set by
// agentEnvPairs.
func opencodeLaunchCmd(binary string) string {
	return binary + " --standalone --auto"
}

func ompLaunchCmd(binary, model, effort string) string {
	approval := strings.TrimSpace(os.Getenv("HIVE_OMP_APPROVAL_MODE"))
	if approval == "" {
		approval = ompDefaultApprovalMode
	}
	cmd := fmt.Sprintf("%s --approval-mode %s", binary, approval) + config.ModelFlag(model)
	if effort != "" {
		cmd += fmt.Sprintf(" --thinking %s", effort)
	}
	return cmd
}

// codexEffortFlag renders the codex reasoning-effort config-key argument for
// a configured effort, or "" when unset. The `-c model_reasoning_effort="<v>"`
// spelling (a Codex CONFIG KEY, not a flag) is the one the scripted launch
// paths already use — bin/agent-launch.sh and the contributor relay — and the
// three spellings must not drift apart.
func codexEffortFlag(effort string) string {
	if effort == "" {
		return ""
	}
	return fmt.Sprintf(" -c model_reasoning_effort=%q", effort)
}

// claudeEffortFlag renders Claude Code's `--effort <v>` argument for a
// configured reasoning effort, or "" when unset or not one of the levels
// claude accepts (config.ReasoningEffortsByBackend["claude"]). Absent means
// Claude Code keeps its own default effort; an unknown value is dropped
// rather than passed, so a stale or mistyped stored effort can never turn
// into a launch that fails to parse its flags (hivecommons/hive#8377). Set
// time already rejects unknown values (config.ValidateReasoningEffort), so
// this is the launch-side half of the same rule, mirrored in
// ResolveReasoningEffort for the attribution trail.
func claudeEffortFlag(effort string) string {
	if !config.ValidEffort(config.ClaudeBackend, effort) {
		return ""
	}
	return fmt.Sprintf(" --effort %s", effort)
}

// agyLaunchEffort is config.AgyLaunchEffort: the --effort agy is launched
// with for a model and configured effort.
func agyLaunchEffort(model, effort string) string {
	return config.AgyLaunchEffort(model, effort)
}
