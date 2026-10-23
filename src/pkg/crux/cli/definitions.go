package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hivecommons/hive/pkg/crux"
	"github.com/hivecommons/hive/pkg/crux/config"
)

// DefinitionLoader turns the files a command names into agent definitions.
// Reading hive.yaml and the portable AgentDefinition format needs Hive's
// config loader and pkg/defsrc, both outside the Crux boundary, so the host
// (cmd/crux) supplies this and the CLI only programs against it — the same
// inversion as agent.SpanStarter (src/docs/design/crux.md, "Configuration
// leaf").
type DefinitionLoader interface {
	// LoadConfig returns the enabled agents of a hive.yaml and the
	// ProjectContext derived from it, exactly as cmd/hive hands them to
	// agent.NewManager. An empty path yields an empty runtime.
	LoadConfig(path string) (map[string]config.AgentConfig, crux.ProjectContext, error)
	// LoadDefinitionFile reads one agent definition from path and returns the
	// agent's name and config with hive's per-agent defaults applied.
	LoadDefinitionFile(path string) (string, config.AgentConfig, error)
}

// noLoader is the DefinitionLoader an Env without one gets: it still serves
// the empty runtime a bare `crux ps` observes, and names the missing wiring
// when a command actually needs a file read.
type noLoader struct{}

func (noLoader) LoadConfig(path string) (map[string]config.AgentConfig, crux.ProjectContext, error) {
	if path == "" {
		return map[string]config.AgentConfig{}, crux.ProjectContext{}, nil
	}
	return nil, crux.ProjectContext{}, errNoLoader(path)
}

func (noLoader) LoadDefinitionFile(path string) (string, config.AgentConfig, error) {
	return "", config.AgentConfig{}, errNoLoader(path)
}

func errNoLoader(path string) error {
	return fmt.Errorf("reading %s: this crux build has no definition loader", path)
}

// isDefinitionPath reports whether a `run` argument names a file rather
// than an agent already in --config. Only .yaml counts, because that is the
// only extension hive's per-agent overlay loader reads.
func isDefinitionPath(arg string) bool {
	return strings.ToLower(filepath.Ext(arg)) == ".yaml"
}
