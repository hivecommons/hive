package agent

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/crux/cline"
	"github.com/hivecommons/hive/crux/config"
)

func validateClineSandbox(agent *AgentProcess, runtime string, job config.SandboxJobConfig) error {
	if err := validateClineDefinition(agent); err != nil {
		return err
	}
	if runtime != config.SandboxRuntimeJob {
		_, err := sandboxCommand(configSnapshot{Backend: "cline"}, sandboxPromptRelPath)
		return err
	}
	for _, name := range job.EnvFromSecrets {
		if strings.TrimSpace(name) != "" {
			return nil
		}
	}
	return fmt.Errorf("Cline sandbox Job requires job.env_from_secrets naming a dedicated provider Secret containing %s", cline.ProviderSecretEnv)
}

func sandboxKickCommand(spec SandboxKickSpec, promptRel string) ([]string, error) {
	cfg := spec.AgentConfig
	if cfg.Backend != "cline" || spec.Runtime != config.SandboxRuntimeJob {
		return sandboxCommand(cfg, promptRel)
	}
	if strings.TrimSpace(cfg.LaunchCmd) != "" {
		return nil, fmt.Errorf("Cline launch_cmd overrides are unsupported: use the canonical bounded cline-turn runner")
	}
	if _, _, err := cline.Selection(cfg.Model); err != nil {
		return nil, err
	}
	if err := config.ValidateReasoningEffort("cline", cfg.ReasoningEffort); err != nil {
		return nil, err
	}
	args := []string{"crux", cline.Subcommand, "--provider-secret", "--model", shellQuote(cfg.Model),
		"--prompt-file", shellQuote(promptRel)}
	if cfg.ReasoningEffort != "" {
		args = append(args, "--thinking", shellQuote(cfg.ReasoningEffort))
	}
	if spec.Timeout > 0 {
		args = append(args, "--timeout", shellQuote(spec.Timeout.String()))
	}
	return []string{"sh", "-lc", "exec " + strings.Join(args, " ")}, nil
}
