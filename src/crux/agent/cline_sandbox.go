package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hivecommons/hive/crux/cline"
	"github.com/hivecommons/hive/crux/config"
)

func validateClineSandbox(agent *AgentProcess, runtime string, job config.SandboxJobConfig, taskMCPURL string) error {
	if err := validateClineDefinition(agent, taskMCPURL); err != nil {
		return err
	}
	if runtime != config.SandboxRuntimeJob {
		if !filepath.IsAbs(os.Getenv("HIVE_CLINE_CONFIG_DIR")) {
			return fmt.Errorf("Cline Podman requires an explicit absolute HIVE_CLINE_CONFIG_DIR for dedicated operator credentials")
		}
		return nil
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
	if cfg.Backend != "cline" {
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
	args := []string{"crux", cline.Subcommand}
	if spec.Runtime == config.SandboxRuntimeJob {
		args = append(args, "--provider-secret")
	} else {
		args = append(args, "--provider-file", cline.ProviderMountPath, "--state-dir", cline.ProviderStatePath)
	}
	args = append(args, "--model", cfg.Model, "--prompt-file", promptRel)
	if cfg.ReasoningEffort != "" {
		args = append(args, "--thinking", cfg.ReasoningEffort)
	}
	if spec.Timeout > 0 {
		args = append(args, "--timeout", spec.Timeout.String())
	}
	return args, nil
}
