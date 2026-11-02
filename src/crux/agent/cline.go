package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/cline"
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

func validateClineLaunch(ctx context.Context, binary string, agent *AgentProcess) error {
	if strings.TrimSpace(agent.Config.LaunchCmd) != "" {
		return fmt.Errorf("Cline launch_cmd overrides are unsupported: use the canonical bounded cline-turn runner")
	}
	if agent.Config.Tools != nil || len(agent.Config.Connections) != 0 {
		return fmt.Errorf("Cline cannot enforce Hive tools/connections configuration; refusing launch rather than dropping policy")
	}
	model := agent.effectiveModel()
	if _, _, err := cline.Selection(model); err != nil {
		return err
	}
	if agent.UID <= 0 && os.Getenv("HIVE_CLINE_DANGEROUSLY_RUN_UNCONFINED") != "1" {
		return fmt.Errorf("Cline has no OS sandbox: configure an isolated per-UID worker, or explicitly set HIVE_CLINE_DANGEROUSLY_RUN_UNCONFINED=1 on a disposable externally isolated host")
	}
	return cline.Probe(ctx, binary)
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
