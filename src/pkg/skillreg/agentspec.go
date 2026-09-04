package skillreg

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type AgentMode string

const (
	ModeObserve    AgentMode = "observe"
	ModeSuggest    AgentMode = "suggest"
	ModeAutonomous AgentMode = "autonomous"

	DefaultMode = ModeSuggest
)

type AgentSpec interface {
	AgentName() string
	Backend() string
	Model() string
	Mode() AgentMode
	DefaultSkills() []string
}

type SpecToolRule struct {
	Pattern string `yaml:"pattern" json:"pattern"`
	Action  string `yaml:"action" json:"action"`
	Reason  string `yaml:"reason,omitempty" json:"reason,omitempty"`
}

type SpecTools struct {
	Preset string         `yaml:"preset,omitempty" json:"preset,omitempty"`
	Rules  []SpecToolRule `yaml:"rules,omitempty" json:"rules,omitempty"`
}

type SpecData struct {
	Name          string     `yaml:"name" json:"name"`
	BackendID     string     `yaml:"backend" json:"backend"`
	ModelID       string     `yaml:"model" json:"model"`
	OperatingMode AgentMode  `yaml:"mode" json:"mode"`
	LaunchCommand string     `yaml:"launch_cmd,omitempty" json:"launch_cmd,omitempty"`
	Prompt        string     `yaml:"prompt,omitempty" json:"prompt,omitempty"`
	Tools         *SpecTools `yaml:"tools,omitempty" json:"tools,omitempty"`
	Skills        []string   `yaml:"skills,omitempty" json:"skills,omitempty"`
}

var _ AgentSpec = (*SpecData)(nil)

func (s *SpecData) AgentName() string { return s.Name }
func (s *SpecData) Backend() string   { return s.BackendID }
func (s *SpecData) Model() string     { return s.ModelID }

func (s *SpecData) Mode() AgentMode {
	if s.OperatingMode == "" {
		return DefaultMode
	}
	return s.OperatingMode
}

func (s *SpecData) DefaultSkills() []string { return s.Skills }

var validAgentSpecModes = map[AgentMode]struct{}{
	ModeObserve:    {},
	ModeSuggest:    {},
	ModeAutonomous: {},
}

func ParseAgentSpec(data []byte) (*SpecData, error) {
	var spec SpecData
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("skillreg: malformed agent spec: %w", err)
	}
	spec.Name = strings.TrimSpace(spec.Name)
	spec.BackendID = strings.TrimSpace(spec.BackendID)
	spec.ModelID = strings.TrimSpace(spec.ModelID)
	spec.LaunchCommand = strings.TrimSpace(spec.LaunchCommand)
	spec.Prompt = strings.TrimSpace(spec.Prompt)

	if spec.Name == "" {
		return nil, fmt.Errorf("skillreg: agent spec missing required field: name")
	}
	if spec.BackendID == "" {
		return nil, fmt.Errorf("skillreg: agent spec missing required field: backend")
	}
	if spec.ModelID == "" {
		return nil, fmt.Errorf("skillreg: agent spec missing required field: model")
	}
	if spec.OperatingMode == "" {
		spec.OperatingMode = DefaultMode
	}
	if _, ok := validAgentSpecModes[spec.OperatingMode]; !ok {
		return nil, fmt.Errorf("skillreg: agent spec has unknown mode %q", spec.OperatingMode)
	}
	if err := validateSpecTools(spec.Tools); err != nil {
		return nil, err
	}
	return &spec, nil
}

func validateSpecTools(tools *SpecTools) error {
	if tools == nil {
		return nil
	}
	validPresets := map[string]bool{"": true, "advisory": true, "issues-only": true, "issues-prs": true, "full": true}
	if !validPresets[tools.Preset] {
		return fmt.Errorf("skillreg: agent spec tools.preset %q is invalid", tools.Preset)
	}
	validActions := map[string]bool{"allow": true, "deny": true}
	for i, rule := range tools.Rules {
		if strings.TrimSpace(rule.Pattern) == "" {
			return fmt.Errorf("skillreg: agent spec tools.rules[%d]: pattern is required", i)
		}
		if !validActions[rule.Action] {
			return fmt.Errorf("skillreg: agent spec tools.rules[%d]: action must be allow or deny, got %q", i, rule.Action)
		}
	}
	return nil
}

func LoadAgentSpec(path string) (*SpecData, error) {
	resolved, err := ResolveAgentSpecPath(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(resolved) // #nosec G304 -- operator-configured local BYO-agent spec path.
	if err != nil {
		return nil, fmt.Errorf("skillreg: cannot read agent spec %q: %w", resolved, err)
	}
	return ParseAgentSpec(data)
}

func ResolveAgentSpecPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("skillreg: agent spec path is empty")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("skillreg: cannot stat agent spec %q: %w", path, err)
	}
	if !info.IsDir() {
		return path, nil
	}
	for _, name := range []string{"agent.yaml", "agent.yml", "agent-spec.yaml", "agent-spec.yml", "spec.yaml", "spec.yml"} {
		candidate := filepath.Join(path, name)
		if st, statErr := os.Stat(candidate); statErr == nil && !st.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("skillreg: no agent spec file found in %q", path)
}
