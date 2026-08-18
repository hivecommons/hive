package skillreg

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// # Agent SDK contract (BYO-agent)
//
// AgentSpec is the minimal, stable contract a third party declares to plug a
// custom ("bring-your-own") agent into Hive. It names the agent, the backend
// that runs it, the model to use, the operating mode/policy, and the skills to
// load by default. It is deliberately small: everything a catalog needs to list
// and launch a custom agent, and nothing that couples callers to Hive internals.
//
// The Go interface AgentSpec is the contract; SpecData is the plain, YAML-backed
// value that satisfies it, so a declaration on disk and a programmatic
// implementation are interchangeable.

// AgentMode is an agent's operating policy — how much autonomy it is granted.
type AgentMode string

const (
	// ModeObserve means the agent only reports; it takes no repository actions.
	ModeObserve AgentMode = "observe"
	// ModeSuggest means the agent proposes changes (e.g. draft PRs) for review.
	ModeSuggest AgentMode = "suggest"
	// ModeAutonomous means the agent may act (e.g. open and merge PRs) within
	// its configured guardrails.
	ModeAutonomous AgentMode = "autonomous"

	// DefaultMode is applied when a spec omits mode.
	DefaultMode = ModeSuggest
)

// AgentSpec is the BYO-agent contract. Any type providing these accessors can be
// registered as a custom agent. Accessors (not fields) keep the contract stable
// while allowing alternate backing implementations.
type AgentSpec interface {
	// AgentName is the unique identifier for the agent.
	AgentName() string
	// Backend names the execution backend (e.g. "claude-code", "codex", a
	// custom runner). Opaque to Hive; interpreted by the launcher.
	Backend() string
	// Model is the model identifier the backend should use.
	Model() string
	// Mode is the operating policy (observe/suggest/autonomous).
	Mode() AgentMode
	// DefaultSkills lists skill names to resolve into the agent's context by
	// default. Resolved against a Registry via ResolveRequested.
	DefaultSkills() []string
}

// SpecData is the concrete, YAML-backed AgentSpec. It is the wire and disk
// format for declaring a custom agent.
type SpecData struct {
	Name          string    `yaml:"name"`
	BackendID     string    `yaml:"backend"`
	ModelID       string    `yaml:"model"`
	OperatingMode AgentMode `yaml:"mode"`
	Skills        []string  `yaml:"skills"`
}

// Compile-time assertion that SpecData satisfies the AgentSpec contract.
var _ AgentSpec = (*SpecData)(nil)

// AgentName implements AgentSpec.
func (s *SpecData) AgentName() string { return s.Name }

// Backend implements AgentSpec.
func (s *SpecData) Backend() string { return s.BackendID }

// Model implements AgentSpec.
func (s *SpecData) Model() string { return s.ModelID }

// Mode implements AgentSpec. It returns DefaultMode when unset.
func (s *SpecData) Mode() AgentMode {
	if s.OperatingMode == "" {
		return DefaultMode
	}
	return s.OperatingMode
}

// DefaultSkills implements AgentSpec.
func (s *SpecData) DefaultSkills() []string { return s.Skills }

// validModes is the set of accepted operating modes.
var validModes = map[AgentMode]struct{}{
	ModeObserve:    {},
	ModeSuggest:    {},
	ModeAutonomous: {},
}

// ParseAgentSpec parses an AgentSpec from YAML bytes. Unlike skill loading,
// which is best-effort, a spec is a contract: a malformed document, a missing
// name/backend/model, or an unknown mode is rejected with an error so a
// misconfigured agent never launches silently.
func ParseAgentSpec(data []byte) (*SpecData, error) {
	var spec SpecData
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("skillreg: malformed agent spec: %w", err)
	}
	spec.Name = strings.TrimSpace(spec.Name)
	spec.BackendID = strings.TrimSpace(spec.BackendID)
	spec.ModelID = strings.TrimSpace(spec.ModelID)

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
	if _, ok := validModes[spec.OperatingMode]; !ok {
		return nil, fmt.Errorf("skillreg: agent spec has unknown mode %q", spec.OperatingMode)
	}
	return &spec, nil
}

// LoadAgentSpec reads and parses an AgentSpec from a YAML file at path.
func LoadAgentSpec(path string) (*SpecData, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("skillreg: cannot read agent spec %q: %w", path, err)
	}
	return ParseAgentSpec(data)
}
