// Package defs is the Crux module's own agent-definition loader: the
// cli.DefinitionLoader the module's `crux` binary (crux/cmd/crux) ships with,
// so a checkout of the module alone can run agents without Hive's config
// loader or pkg/defsrc (hivecommons/hive#14138).
//
// It reads plain YAML from a directory or a file (`--defs`, `$CRUX_DEFS`):
//
//   - a portable AgentDefinition (`kind: AgentDefinition`, metadata.name,
//     spec.*), the dashboard's import/export schema, merged over the same
//     defaults an import gets through the same field allow-list;
//   - a bare per-agent overlay: the AgentConfig YAML itself, named by the
//     file's base name.
//
// Either way the result is a config.AgentConfig with the per-agent defaults
// (id, role, replicas, enabled, clear_on_kick) applied. Where definitions come
// from in a Hive — hive.yaml, repo-tree definition sources, the dashboard
// overlay — stays with the host binary in the root module (cmd/crux); this
// package knows only files.
package defs

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
)

// DefinitionKind is the required `kind` of a portable AgentDefinition
// document; it mirrors the dashboard's export kind so an export round-trips.
const DefinitionKind = "AgentDefinition"

// Defaults a definition is merged over: the values the dashboard's import
// fills in and cmd/crux's hiveLoader applies.
const (
	// DefaultBackend is the backend a portable definition that names none
	// runs on.
	DefaultBackend = "copilot"
	// DefaultRestartStrategy is the restart strategy of a portable definition
	// that names none.
	DefaultRestartStrategy = "immediate"
	// DefaultReplicas is the replica count of an agent that names none.
	DefaultReplicas = 1
	// MaxNameLen bounds an agent name, as the dashboard import does.
	MaxNameLen = 64
	// Ext is the only file extension the directory walk reads, matching the
	// per-agent overlay loader and `crux run`'s definition-path rule.
	Ext = ".yaml"
)

// namePattern is the dashboard import's agent-name rule: lower-case words
// joined by hyphens or underscores, so a definition cannot name a path.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Loader implements cli.DefinitionLoader over files. The zero value is ready
// to use.
type Loader struct{}

// LoadConfig returns the agents defined at path, which may be a directory of
// definition files or a single file, and the zero ProjectContext: this loader
// knows no org, repos or policies. An empty path yields an empty runtime.
func (Loader) LoadConfig(path string) (map[string]config.AgentConfig, crux.ProjectContext, error) {
	if path == "" {
		return map[string]config.AgentConfig{}, crux.ProjectContext{}, nil
	}
	defs, err := Load(path)
	if err != nil {
		return nil, crux.ProjectContext{}, err
	}
	return defs, crux.ProjectContext{}, nil
}

// LoadDefinitionFile reads one definition file; see LoadFile.
func (Loader) LoadDefinitionFile(path string) (string, config.AgentConfig, error) {
	return LoadFile(path)
}

// Load reads every definition at path: the file itself, or each `.yaml`
// directly inside it when path is a directory. Two files defining the same
// agent name are an error rather than a silent last-wins.
func Load(path string) (map[string]config.AgentConfig, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("reading definitions %s: %w", path, err)
	}
	defs := map[string]config.AgentConfig{}
	if !info.IsDir() {
		name, ac, err := LoadFile(path)
		if err != nil {
			return nil, err
		}
		defs[name] = ac
		return defs, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("reading definitions %s: %w", path, err)
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), Ext) {
			continue
		}
		files = append(files, filepath.Join(path, entry.Name()))
	}
	sort.Strings(files)
	from := map[string]string{}
	for _, file := range files {
		name, ac, err := LoadFile(file)
		if err != nil {
			return nil, err
		}
		if prev, dup := from[name]; dup {
			return nil, fmt.Errorf("definition %s: agent %q is already defined by %s", file, name, prev)
		}
		from[name] = file
		defs[name] = ac
	}
	return defs, nil
}

// LoadFile reads one agent definition and returns the agent's name and its
// config with ApplyDefaults applied. A `kind: AgentDefinition` document is
// named by metadata.name; anything else is read as the AgentConfig YAML
// itself and named by the file's base name.
func LoadFile(path string) (string, config.AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", config.AgentConfig{}, fmt.Errorf("reading definition %s: %w", path, err)
	}
	var name string
	var ac config.AgentConfig
	if isPortable(data) {
		def, err := ParseDefinition(data)
		if err != nil {
			return "", config.AgentConfig{}, fmt.Errorf("definition %s: %w", path, err)
		}
		name = def.Name()
		ac = def.Merge(config.AgentConfig{
			Backend:         DefaultBackend,
			RestartStrategy: DefaultRestartStrategy,
			Enabled:         true,
		})
	} else {
		name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		if err := yaml.Unmarshal(data, &ac); err != nil {
			return "", config.AgentConfig{}, fmt.Errorf("definition %s: %w", path, err)
		}
		// The per-agent overlay loader marks its entries managed; keep the
		// same shape so a file moved between a Hive and a bare crux behaves
		// the same.
		ac.Managed = true
	}
	if err := ValidateName(name); err != nil {
		return "", config.AgentConfig{}, fmt.Errorf("definition %s: %w", path, err)
	}
	ac.SetLoadState(name, path)
	ApplyDefaults(name, &ac)
	return name, ac, nil
}

// ApplyDefaults fills the per-agent defaults a hive.yaml load applies to
// every agent: id and role from the name, one replica, enabled and
// clear_on_kick unless the YAML said otherwise. Hive's table of well-known
// agent names (emoji, colour, keywords) is deliberately not here; that is
// presentation the host owns.
func ApplyDefaults(name string, ac *config.AgentConfig) {
	if ac.ID == "" {
		ac.ID = name
	}
	if ac.Role == "" {
		ac.Role = name
	}
	if ac.Replicas == 0 {
		ac.Replicas = DefaultReplicas
	}
	if !ac.Enabled && !ac.EnabledExplicitlySet() {
		ac.Enabled = true
	}
	if !ac.ClearOnKickExplicitlySet() {
		ac.ClearOnKick = true
	}
}

// ValidateName enforces the dashboard import's agent-name rule.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("agent name is empty")
	}
	if len(name) > MaxNameLen {
		return fmt.Errorf("agent name %q is longer than %d characters", name, MaxNameLen)
	}
	if !namePattern.MatchString(name) {
		return fmt.Errorf("agent name %q contains invalid characters", name)
	}
	return nil
}

// isPortable reports whether data declares the AgentDefinition kind without
// committing to a full parse.
func isPortable(data []byte) bool {
	var probe struct {
		Kind string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return false
	}
	return probe.Kind == DefinitionKind
}
