package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/cline"
	"github.com/hivecommons/hive/crux/config"
)

func clineLaunchCmd() string {
	return fmt.Sprintf("export PS1=%s; echo %s", shellQuote(cline.ReadyMarker+"> "), shellQuote(cline.ReadyMarker))
}

func clineTurnCommand(binary, model, effort, promptFile string) string {
	args := []string{shellQuote(binary), cline.Subcommand, "--model", shellQuote(model), "--prompt-file", shellQuote(promptFile)}
	if effort != "" {
		args = append(args, "--thinking", shellQuote(effort))
	}
	return fmt.Sprintf("printf '\\n%s\\n'; %s; echo %s", cline.RunningMarker, strings.Join(args, " "), cline.ReadyMarker)
}

// clineHasOperatorConnections reports whether the operator declared any
// connection for the agent. The one withTaskMCPConnection adds for the hive's
// own task MCP endpoint (taskMCPURL) is not operator policy: only Claude Code
// is handed it at launch, so launching Cline without it drops nothing. That
// connection is mcp, named hive-task, at that endpoint, with nothing else
// set; a hive-task aimed elsewhere, or carrying its own auth, env name or
// options, is the operator's.
func clineHasOperatorConnections(conns []config.ConnectionConfig, taskMCPURL string) bool {
	taskMCPURL = strings.TrimSpace(taskMCPURL)
	for _, conn := range conns {
		injected := taskMCPURL != "" && conn.Type == "mcp" && conn.Name == "hive-task" &&
			conn.Auth == nil && conn.EnvName == "" && len(conn.Options) == 0 &&
			sameTaskMCPEndpoint(conn.URI, taskMCPURL)
		if !injected {
			return true
		}
	}
	return false
}

func validateClineDefinition(agent *AgentProcess, taskMCPURL string) error {
	if strings.TrimSpace(agent.Config.LaunchCmd) != "" {
		return fmt.Errorf("Cline launch_cmd overrides are unsupported: use the canonical bounded cline-turn runner")
	}
	if agent.Config.Tools != nil || clineHasOperatorConnections(agent.Config.Connections, taskMCPURL) {
		return fmt.Errorf("Cline cannot enforce Hive tools/connections configuration; refusing launch rather than dropping policy")
	}
	model := agent.effectiveModel()
	if _, _, err := cline.Selection(model); err != nil {
		return err
	}
	return config.ValidateReasoningEffort("cline", agent.Config.ReasoningEffort)
}

func validateClineLaunch(ctx context.Context, binary string, agent *AgentProcess, taskMCPURL string) error {
	if err := validateClineDefinition(agent, taskMCPURL); err != nil {
		return err
	}
	if agent.UID <= 0 && os.Getenv("HIVE_CLINE_DANGEROUSLY_RUN_UNCONFINED") != "1" {
		return fmt.Errorf("Cline has no OS sandbox: configure an isolated per-UID worker, or explicitly set HIVE_CLINE_DANGEROUSLY_RUN_UNCONFINED=1 on a disposable externally isolated host")
	}
	if err := cline.Probe(ctx, binary); err != nil {
		return err
	}
	source := os.Getenv("HIVE_CLINE_CONFIG_DIR")
	if source == "" {
		source = filepath.Join(sharedAgentHome, ".cline")
	}
	return cline.CheckConfig(source, agent.effectiveModel())
}

func (m *Manager) deliverClineKickLocked(agent *AgentProcess, message, trigger string) error {
	message = kickMessageWithSuffixes(message, false, resolveExplainMode(agent.Config, m.explainModeDefault()))
	promptFile := filepath.Join(agentStateDir, fmt.Sprintf(".hive-cline-prompt-%s.txt", agent.Name))
	if err := writeAgentStateFile(promptFile, []byte(message)); err != nil {
		return m.kickDeliveryFailedLocked(agent, "write Cline prompt", err)
	}
	cmd := clineTurnCommand(agyTurnRunnerBinary(), agent.effectiveModel(), agent.Config.ReasoningEffort, promptFile)
	if err := m.tmuxSendKeysForAgent(agent, "C-u"); err != nil {
		return m.kickDeliveryFailedLocked(agent, "clear input", err)
	}
	if err := m.tmuxSendLiteralForAgent(agent, cmd); err != nil {
		return m.kickDeliveryFailedLocked(agent, "send Cline turn", err)
	}
	time.Sleep(textToEnterDelay)
	if err := m.tmuxSendEntersForAgent(agent); err != nil {
		return m.kickDeliveryFailedLocked(agent, "submit Cline turn", err)
	}
	m.recordDeliveredKickLocked(agent, message, trigger)
	return nil
}
