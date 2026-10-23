package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/defsrc"
)

// definitionNamePattern is the dashboard import handler's agent-name rule
// (kickTemplatePattern on `<name>.md`): lower-case words joined by hyphens
// or underscores, so a definition cannot name a path.
var definitionNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// maxDefinitionNameLen matches the dashboard import limit.
const maxDefinitionNameLen = 64

// Defaults a portable AgentDefinition is merged over; the same values the
// dashboard's agentConfigFromDefinition fills in for an import.
const (
	defaultDefinitionBackend         = "copilot"
	defaultDefinitionRestartStrategy = "immediate"
)

// loadConfigDefinitions returns the enabled agents of a hive.yaml and the
// ProjectContext derived from it, exactly as cmd/hive hands them to
// agent.NewManager. An empty path yields an empty runtime.
func loadConfigDefinitions(path string) (map[string]config.AgentConfig, agent.ProjectContext, error) {
	if path == "" {
		return map[string]config.AgentConfig{}, agent.ProjectContext{}, nil
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, agent.ProjectContext{}, err
	}
	return cfg.EnabledAgents(), projectContextFromConfig(cfg), nil
}

// loadDefinitionFile reads one agent definition from path and returns the
// agent's name and config with hive's per-agent defaults applied. Two formats
// are accepted, both already hive's:
//
//   - a portable AgentDefinition (`kind: AgentDefinition`, the dashboard's
//     import/export schema, src/examples/agents/*.yaml), named by
//     metadata.name and merged through defsrc.MergeAllowedFields so a
//     definition can set exactly the fields an import can;
//   - a bare per-agent overlay (the agents_dir file format,
//     config.LoadAgentOverrides), named by the file's base name.
func loadDefinitionFile(path string) (string, config.AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", config.AgentConfig{}, fmt.Errorf("reading definition %s: %w", path, err)
	}
	var name string
	var ac config.AgentConfig
	if isPortableDefinition(data) {
		def, err := defsrc.ParseDefinition(string(data))
		if err != nil {
			return "", config.AgentConfig{}, fmt.Errorf("definition %s: %w", path, err)
		}
		name = strings.ToLower(strings.ReplaceAll(def.Metadata.Name, " ", "-"))
		ac = defsrc.MergeAllowedFields(config.AgentConfig{
			Backend:         defaultDefinitionBackend,
			RestartStrategy: defaultDefinitionRestartStrategy,
			Enabled:         true,
		}, def)
	} else {
		name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		overlays, err := config.LoadAgentOverrides(filepath.Dir(path))
		if err != nil {
			return "", config.AgentConfig{}, err
		}
		got, ok := overlays[name]
		if !ok {
			return "", config.AgentConfig{}, fmt.Errorf("definition %s: expected a .yaml per-agent file or kind: AgentDefinition", path)
		}
		ac = got
	}
	if err := validateDefinitionName(name); err != nil {
		return "", config.AgentConfig{}, fmt.Errorf("definition %s: %w", path, err)
	}
	// Route through a Config so ApplyAgentDefaults fills ID, role, replicas and
	// the other per-agent defaults the same way a hive.yaml load does.
	cfg := &config.Config{}
	cfg.Agents = map[string]config.AgentConfig{name: ac}
	cfg.ApplyAgentDefaults(name)
	return name, cfg.Agents[name], nil
}

// isPortableDefinition reports whether data declares the AgentDefinition kind
// without committing to a full parse.
func isPortableDefinition(data []byte) bool {
	var probe struct {
		Kind string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return false
	}
	return probe.Kind == defsrc.DefinitionKind
}

func validateDefinitionName(name string) error {
	if name == "" {
		return fmt.Errorf("agent name is empty")
	}
	if len(name) > maxDefinitionNameLen {
		return fmt.Errorf("agent name %q is longer than %d characters", name, maxDefinitionNameLen)
	}
	if !definitionNamePattern.MatchString(name) {
		return fmt.Errorf("agent name %q contains invalid characters", name)
	}
	return nil
}

// isDefinitionPath reports whether a `run` argument names a file rather
// than an agent already in --config. Only .yaml counts, because that is the
// only extension config.LoadAgentOverrides reads.
func isDefinitionPath(arg string) bool {
	return strings.ToLower(filepath.Ext(arg)) == ".yaml"
}
