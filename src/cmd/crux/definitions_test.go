package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const portableDefinition = `apiVersion: hive.kubestellar.io/v1
kind: AgentDefinition
metadata:
  name: Scout Agent
  displayName: Scout
spec:
  backend: claude
  model: claude-sonnet-4-6
  role: scout
  clearOnKick: true
`

const overlayDefinition = `backend: codex
model: gpt-5
role: fixer
`

func hiveYAML(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return writeFile(t, filepath.Join(dir, "hive.yaml"), `
project:
  org: hivecommons
  repos: [hivecommons/hive]
github:
  token: test-token
data:
  agents_dir: `+filepath.Join(dir, "agent-configs")+`
agents:
  scout:
    backend: claude
    model: claude-sonnet-4-6
  sleeper:
    backend: codex
    enabled: false
`)
}

func TestLoadDefinitionFile(t *testing.T) {
	var ld hiveLoader
	dir := t.TempDir()
	name, cfg, err := ld.LoadDefinitionFile(writeFile(t, filepath.Join(dir, "a.yaml"), portableDefinition))
	if err != nil || name != "scout-agent" || cfg.Backend != "claude" || cfg.DisplayName != "Scout" || cfg.Replicas != 1 {
		t.Fatalf("portable: %q %+v %v", name, cfg, err)
	}

	name, cfg, err = ld.LoadDefinitionFile(writeFile(t, filepath.Join(dir, "fixer.yaml"), overlayDefinition))
	if err != nil || name != "fixer" || cfg.Backend != "codex" || cfg.ID != "fixer" || !cfg.Managed {
		t.Fatalf("overlay: %q %+v %v", name, cfg, err)
	}

	if _, _, err := ld.LoadDefinitionFile(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("missing file: want error")
	}
	if _, _, err := ld.LoadDefinitionFile(writeFile(t, filepath.Join(t.TempDir(), "x.yaml"), "kind: AgentDefinition\nmetadata: {}\n")); err == nil {
		t.Fatal("portable without name: want error")
	}
	if _, _, err := ld.LoadDefinitionFile(writeFile(t, filepath.Join(t.TempDir(), "Bad Name.yaml"), overlayDefinition)); err == nil {
		t.Fatal("bad overlay name: want error")
	}
	if _, _, err := ld.LoadDefinitionFile(writeFile(t, filepath.Join(t.TempDir(), "x.yaml"), "kind: AgentDefinition\nmetadata:\n  name: 'a/b'\n")); err == nil {
		t.Fatal("portable with path name: want error")
	}
	if _, _, err := ld.LoadDefinitionFile(writeFile(t, filepath.Join(t.TempDir(), "x.yml"), overlayDefinition)); err == nil {
		t.Fatal(".yml overlay: want error (hive only reads .yaml)")
	}
	if _, _, err := ld.LoadDefinitionFile(writeFile(t, filepath.Join(t.TempDir(), "x.yaml"), ":\n  - [")); err == nil {
		t.Fatal("malformed yaml: want error")
	}
	if isPortableDefinition([]byte(":\n  - [")) {
		t.Fatal("malformed yaml is not a portable definition")
	}
}

func TestValidateDefinitionName(t *testing.T) {
	if err := validateDefinitionName(""); err == nil {
		t.Fatal("empty")
	}
	if err := validateDefinitionName(strings.Repeat("a", maxDefinitionNameLen+1)); err == nil {
		t.Fatal("too long")
	}
	if err := validateDefinitionName("ok-name_1"); err != nil {
		t.Fatal(err)
	}
}

func TestProjectContextFromConfig(t *testing.T) {
	if pc := projectContextFromConfig(nil); pc.Org != "" {
		t.Fatalf("nil: %+v", pc)
	}
	cfg, err := config.Load(hiveYAML(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Policies.LocalDir = "/x"
	cfg.Policies.Path = "p"
	pc := projectContextFromConfig(cfg)
	if pc.Org != "hivecommons" || len(pc.Repos) != 1 || pc.PolicyDir != "/x/p" || pc.RepoPaused == nil || pc.AgentRepos == nil {
		t.Fatalf("pc = %+v", pc)
	}
	cfg.GitHub.APIURL = "https://github.example.com/api/v3"
	if pc := projectContextFromConfig(cfg); pc.GHHost == "" {
		t.Fatalf("GHE host not derived: %+v", pc)
	}
}

func TestLoadConfig(t *testing.T) {
	var ld hiveLoader
	defs, project, err := ld.LoadConfig("")
	if err != nil || len(defs) != 0 || project.Org != "" {
		t.Fatalf("empty path: %v %+v %v", defs, project, err)
	}
	defs, project, err = ld.LoadConfig(hiveYAML(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := defs["scout"]; !ok || len(defs) != 1 {
		t.Fatalf("defs = %v, want only the enabled agent", defs)
	}
	if project.Org != "hivecommons" || project.PolicyDir != defaultPoliciesLocalDir {
		t.Fatalf("project = %+v", project)
	}
	if _, _, err := ld.LoadConfig(filepath.Join(t.TempDir(), "missing.yaml")); err == nil || !strings.Contains(err.Error(), "reading config") {
		t.Fatalf("missing config: %v", err)
	}
}
