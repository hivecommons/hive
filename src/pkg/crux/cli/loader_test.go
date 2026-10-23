package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/hivecommons/hive/pkg/crux"
	"github.com/hivecommons/hive/pkg/crux/config"
)

// fakePolicyDir is what fakeLoader derives as the ProjectContext policy dir,
// standing in for cmd/hive's checkout default the real loader applies.
const fakePolicyDir = "/data/policies"

// fakeLoader is the test DefinitionLoader: enough of hive.yaml, the portable
// AgentDefinition and the per-agent overlay formats to drive the commands,
// without pkg/config or pkg/defsrc (both outside the Crux boundary; the real
// loader is cmd/crux's hiveLoader).
type fakeLoader struct{}

func (fakeLoader) LoadConfig(path string) (map[string]config.AgentConfig, crux.ProjectContext, error) {
	if path == "" {
		return map[string]config.AgentConfig{}, crux.ProjectContext{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, crux.ProjectContext{}, fmt.Errorf("reading config %s: %w", path, err)
	}
	var doc struct {
		Project struct {
			Org   string   `yaml:"org"`
			Repos []string `yaml:"repos"`
		} `yaml:"project"`
		Agents map[string]struct {
			Backend string `yaml:"backend"`
			Model   string `yaml:"model"`
			Enabled *bool  `yaml:"enabled"`
		} `yaml:"agents"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, crux.ProjectContext{}, fmt.Errorf("parsing config %s: %w", path, err)
	}
	defs := map[string]config.AgentConfig{}
	for name, a := range doc.Agents {
		if a.Enabled != nil && !*a.Enabled {
			continue
		}
		defs[name] = config.AgentConfig{ID: name, Backend: a.Backend, Model: a.Model, Enabled: true}
	}
	return defs, crux.ProjectContext{Org: doc.Project.Org, Repos: doc.Project.Repos, PolicyDir: fakePolicyDir}, nil
}

func (fakeLoader) LoadDefinitionFile(path string) (string, config.AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", config.AgentConfig{}, fmt.Errorf("reading definition %s: %w", path, err)
	}
	var doc struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name        string `yaml:"name"`
			DisplayName string `yaml:"displayName"`
		} `yaml:"metadata"`
		Spec    config.AgentConfig `yaml:"spec"`
		Backend string             `yaml:"backend"`
		Model   string             `yaml:"model"`
		Role    string             `yaml:"role"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", config.AgentConfig{}, fmt.Errorf("definition %s: %w", path, err)
	}
	var name string
	var ac config.AgentConfig
	if doc.Kind == "AgentDefinition" {
		name = strings.ToLower(strings.ReplaceAll(doc.Metadata.Name, " ", "-"))
		ac = doc.Spec
		ac.DisplayName = doc.Metadata.DisplayName
	} else {
		name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		ac = config.AgentConfig{Backend: doc.Backend, Model: doc.Model, Role: doc.Role, Managed: true}
	}
	ac.ID = name
	ac.Enabled = true
	ac.Replicas = 1
	return name, ac, nil
}

func TestNoLoaderServesOnlyTheEmptyRuntime(t *testing.T) {
	var nl noLoader
	defs, _, err := nl.LoadConfig("")
	if err != nil || len(defs) != 0 {
		t.Fatalf("empty path: %v %v", defs, err)
	}
	if _, _, err := nl.LoadConfig("hive.yaml"); err == nil || !strings.Contains(err.Error(), "no definition loader") {
		t.Fatalf("config without loader: %v", err)
	}
	if _, _, err := nl.LoadDefinitionFile("x.yaml"); err == nil || !strings.Contains(err.Error(), "no definition loader") {
		t.Fatalf("definition without loader: %v", err)
	}
	e := &Env{}
	e.defaults()
	if _, ok := e.Loader.(noLoader); !ok {
		t.Fatalf("Loader = %T, want noLoader", e.Loader)
	}
	if isDefinitionPath("scout") || !isDefinitionPath("scout.YAML") {
		t.Fatal("isDefinitionPath")
	}
}
