package agent

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/crux/config"
)

// toolRulesToLaunchCmd builds a backend-specific CLI command from ToolsConfig.
// effort is the configured per-agent reasoning effort; only backends with an
// effort control consume it (see codexEffortFlag / agyLaunchEffort /
// claudeEffortFlag).
func toolRulesToLaunchCmd(binary, model, backend string, tools *config.ToolsConfig, isInference bool, effort string) string {
	return toolRulesToLaunchCmdWithBobSession(binary, model, backend, tools, isInference, effort, "")
}

func toolRulesToLaunchCmdWithBobSession(binary, model, backend string, tools *config.ToolsConfig, isInference bool, effort, bobLabel string) string {
	denies := tools.DenyPatterns()

	switch backend {
	case bobBackend:
		// bob has no deny-tool flag, so ToolsConfig cannot be expressed here.
		// It must still go through bobLaunchCmd rather than falling to the
		// default branch below, which would append `--model` and crash bob
		// with "Cannot read properties of undefined (reading 'maxTokens')".
		// A bob agent with tools configured therefore launches identically to
		// one without; the deny patterns are silently inapplicable, exactly as
		// they already were before this branch existed.
		return bobLaunchCmd(binary, bobLabel)
	case "claude":
		bareFlag := ""
		if isInference {
			bareFlag = fmt.Sprintf(" --bare --settings %s", claudeInferenceSettingsPath)
		}
		cmd := fmt.Sprintf("%s --model %s --dangerously-skip-permissions%s%s", binary, model, bareFlag, claudeEffortFlag(effort))
		for _, p := range denies {
			cmd += fmt.Sprintf(" --disallowed-tools '%s'", p)
		}
		return cmd
	case "copilot":
		// Never pass --enable-all-github-mcp-tools: Copilot CLI's built-in GitHub
		// MCP server is READ-ONLY BY DEFAULT (v0.0.350+), so the write tools are
		// never registered and read tools stay available. Enabling it would turn
		// writes on and let agents author as the login USER via the MCP.
		cmd := fmt.Sprintf("%s --model %s --no-auto-update --allow-all", binary, model)
		for _, p := range denies {
			// Translate claude-style `mcp__github__<tool>` deny patterns to
			// copilot's built-in server syntax `github-mcp-server(<tool>)`. The
			// server is named `github-mcp-server` (per `copilot --help`), NOT
			// `github` — the old `github(` name matched nothing (silent no-op).
			copilotPattern := strings.ReplaceAll(p, "mcp__github__", "github-mcp-server(")
			if strings.HasPrefix(copilotPattern, "github-mcp-server(") {
				copilotPattern += ")"
			}
			cmd += fmt.Sprintf(" --deny-tool='%s'", copilotPattern)
		}
		return cmd
	case codexBackend:
		// Codex has no deny-tool flag, so ToolsConfig cannot be expressed
		// here (same shape as bob above); model and reasoning effort still
		// apply, matching backendLaunchCmd. The hub tmux path is unattended
		// and has the pod/egress proxy as its boundary, so mirror Claude's
		// bypass posture rather than letting Codex prompt in an unwatched pane.
		cmd := codexUnattendedLaunchCmd(binary)
		if model != "" {
			cmd = fmt.Sprintf("%s --model %s", cmd, model)
		}
		return cmd + codexEffortFlag(effort)
	case "agy":
		// agy has no deny-tool flag, so ToolsConfig cannot be expressed here.
		// Keep launch-mode selection centralized with the default path rather
		// than falling through to the bare-binary default, which would drop
		// agy's unattended approval and model/effort contract.
		return agyLaunchCmd(binary, model, effort)
	case "omp":
		return ompLaunchCmd(binary, model, effort)
	default:
		cmd := binary
		if model != "" {
			cmd = fmt.Sprintf("%s --model %s", binary, model)
		}
		return cmd
	}
}

// backendLaunchCmd builds the per-backend CLI command used when an agent has no
// explicit ToolsConfig. It is the default-path counterpart to
// toolRulesToLaunchCmd and is deliberately pure — no Manager, no tmux, no
// process — so the flag contract each backend depends on can be asserted
// directly in tests instead of by polling a live pane for typed output.
// effort is the configured per-agent reasoning effort; only backends with an
// effort control consume it (see codexEffortFlag / agyLaunchEffort /
// claudeEffortFlag).
func backendLaunchCmd(binary, model, backend string, isInference bool, effort string) string {
	return backendLaunchCmdWithBobSession(binary, model, backend, isInference, effort, "")
}

func backendLaunchCmdWithBobSession(binary, model, backend string, isInference bool, effort, bobLabel string) string {
	var launchCmd string
	switch backend {
	case "claude":
		bareFlag := ""
		if isInference {
			bareFlag = fmt.Sprintf(" --bare --settings %s", claudeInferenceSettingsPath)
		}
		base := fmt.Sprintf("%s --model %s --dangerously-skip-permissions%s%s", binary, model, bareFlag, claudeEffortFlag(effort))
		// Deny ALL GitHub MCP write tools in EVERY mode: agents author via the
		// App-gated gh wrapper, never as the user via the MCP. Mode governs the
		// gh-wrapper/proxy layer only, not what the MCP may write.
		launchCmd = base + claudeGitHubWriteDenyFlags + claudeHostStateDenyFlags()
	case "copilot":
		// model arrives here already canonicalized by normalizeModelName
		// (CanonicalizeCopilotModel: separator drift like claude-fable.5 is
		// normalized to the CLI-accepted claude-fable-5, #4262) and is then
		// passed as-is to `copilot --model %s`. It may be a
		// concrete id OR the auto-selection sentinel "auto" (copilotAutoModel
		// in cli_models.go), which lets the Copilot CLI pick/adjust the model
		// per task. Nothing here assumes a concrete id, so the sentinel flows
		// through unchanged.
		// PRIMARY defense against authoring as the login USER via the MCP:
		// we do NOT pass --enable-all-github-mcp-tools. Copilot CLI's built-in
		// GitHub MCP server is READ-ONLY BY DEFAULT (v0.0.350+), so the write
		// tools (create_issue/create_pull_request/…) are never registered.
		// READ tools (get_issue/list/search) stay available in that read-only
		// default, so nothing here disables useful lookups. All GitHub writes
		// must go through the App-gated gh wrapper / hive-open-pr.
		// copilotGitHubWriteDenyFlags is applied as belt-and-suspenders (with
		// the CORRECT `github-mcp-server(` server name) on top of the read-only
		// default. This is identical across ModeIssuesAndPRs / ModeIssuesOnly /
		// advisory — the mode never changes what the MCP can write (it never
		// legitimately should), it only governs the separate, unchanged
		// gh-wrapper/proxy layer that still reads Mode for the App-gated writes.
		launchCmd = fmt.Sprintf("%s --model %s --no-auto-update --allow-all%s",
			binary, model, copilotGitHubWriteDenyFlags)
	case "gemini":
		launchCmd = fmt.Sprintf("%s --model %s", binary, model)
	case "agy":
		launchCmd = agyLaunchCmd(binary, model, effort)
	case "pi":
		// pi takes the model as a CLI flag, not a subcommand. Without
		// this case the launch command never receives the configured
		// model (previously it also hit the goose binary via the alias).
		launchCmd = fmt.Sprintf("%s --model %s", binary, model)
	case "goose":
		launchCmd = fmt.Sprintf("%s run -s", binary)
		if model != "" {
			launchCmd = fmt.Sprintf("%s --model %s", launchCmd, model)
		}
	case bobBackend:
		launchCmd = bobLaunchCmd(binary, bobLabel)
	case codexBackend:
		// Codex takes the model as a CLI flag like the others. Without this
		// case codex fell to the bare-binary default below, so a dashboard
		// model choice was silently dropped on this launch path (the codex
		// dropdown existed, the selection never reached the CLI). The hub
		// tmux path is unattended: unlike contributor-local mode, there is no
		// human watching the pane and workspace-write would also cut off the
		// proxy-backed gh/git network path, so use Codex's explicit full
		// bypass posture (the pod and egress proxy are the boundary). The
		// reasoning effort rides alongside as a config key — codex has no
		// dedicated flag for it — and is valid with OR without --model
		// (effort alone runs codex's default model at that effort, the same
		// contract as AGENT_REASONING_EFFORT on the relay path).
		launchCmd = codexUnattendedLaunchCmd(binary)
		if model != "" {
			launchCmd = fmt.Sprintf("%s --model %s", launchCmd, model)
		}
		launchCmd += codexEffortFlag(effort)
	case "omp":
		launchCmd = ompLaunchCmd(binary, model, effort)
	default:
		launchCmd = binary
	}
	return launchCmd
}

func codexUnattendedLaunchCmd(binary string) string {
	return binary + " --dangerously-bypass-approvals-and-sandbox -c features.daemon_auto_start=false"
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
