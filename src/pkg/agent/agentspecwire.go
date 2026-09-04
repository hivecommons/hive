package agent

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/skillreg"
)

func (m *Manager) applyAgentSpec(agent *AgentProcess) error {
	cfg := agent.Config
	if strings.TrimSpace(agent.baseConfig.AgentSpec) != "" {
		cfg = agent.baseConfig
	}
	cfg, prompt, applied, err := applyAgentSpecConfig(cfg)
	if err != nil {
		return err
	}
	if !applied {
		agent.clearAgentSpecPromptIfSpecRemoved(cfg)
		return nil
	}
	agent.Config = cfg
	prompt = strings.TrimSpace(prompt)
	if strings.TrimSpace(agent.BootstrapOverride) == "" || strings.TrimSpace(agent.BootstrapOverride) == agent.agentSpecPrompt {
		agent.BootstrapOverride = prompt
	}
	agent.agentSpecPrompt = prompt
	if m != nil && m.logger != nil {
		m.logger.Info("loaded agent spec", "agent", agent.Name, "spec", cfg.AgentSpec)
	}
	return nil
}

func (agent *AgentProcess) clearAgentSpecPromptIfSpecRemoved(cfg config.AgentConfig) {
	if strings.TrimSpace(cfg.AgentSpec) != "" {
		return
	}
	if agent.agentSpecPrompt != "" && strings.TrimSpace(agent.BootstrapOverride) == agent.agentSpecPrompt {
		agent.BootstrapOverride = ""
	}
	agent.agentSpecPrompt = ""
}

func applyAgentSpecConfig(cfg config.AgentConfig) (config.AgentConfig, string, bool, error) {
	if strings.TrimSpace(cfg.AgentSpec) == "" {
		return cfg, "", false, nil
	}
	spec, err := skillreg.LoadAgentSpec(cfg.AgentSpec)
	if err != nil {
		return cfg, "", false, err
	}
	cfg.Backend = spec.Backend()
	cfg.Model = spec.Model()
	cfg.Mode = agentSpecMode(spec.Mode())
	cfg.LaunchCmd = ""
	cfg.Tools = nil
	cfg.Skills = nil
	if spec.LaunchCommand != "" {
		cfg.LaunchCmd = spec.LaunchCommand
	}
	if len(spec.Skills) > 0 {
		cfg.Skills = append([]string(nil), spec.Skills...)
	}
	if spec.Tools != nil {
		cfg.Tools = &config.ToolsConfig{
			Preset: spec.Tools.Preset,
			Rules:  make([]config.ToolRule, 0, len(spec.Tools.Rules)),
		}
		for _, rule := range spec.Tools.Rules {
			cfg.Tools.Rules = append(cfg.Tools.Rules, config.ToolRule{
				Pattern: rule.Pattern,
				Action:  rule.Action,
				Reason:  rule.Reason,
			})
		}
	}
	return cfg, spec.Prompt, true, nil
}

func agentSpecMode(mode skillreg.AgentMode) string {
	switch mode {
	case skillreg.ModeObserve:
		return "ADVISORY"
	case skillreg.ModeAutonomous:
		return "ISSUES_PRS_MERGE"
	case skillreg.ModeSuggest:
		return "ISSUES_AND_PRS"
	default:
		return fmt.Sprint(mode)
	}
}
