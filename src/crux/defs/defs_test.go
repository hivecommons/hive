package defs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/config"
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
  description: looks around
  emoji: "x"
  color: "#123456"
spec:
  backend: claude
  model: claude-sonnet-4-6
  role: scout
  mode: ADVISORY
  sortOrder: 7
  beadRole: worker
  staleTimeout: 60
  restartStrategy: never
  clearOnKick: true
  includeRepos: true
  laneKeywords: [a]
  detectKeywords: [b]
  aliases: [sc]
  cadences:
    default: "@hourly"
  tools:
    preset: read-only
  connections:
    - name: docs
      type: mcp
`

const overlayDefinition = `backend: codex
model: gpt-5
role: fixer
enabled: false
clear_on_kick: false
`

func TestLoadFilePortable(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "scout.yaml"), portableDefinition)
	name, ac, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if name != "scout-agent" {
		t.Fatalf("name = %q", name)
	}
	if ac.ID != "scout-agent" || ac.Backend != "claude" || ac.Model != "claude-sonnet-4-6" || ac.Role != "scout" {
		t.Fatalf("ac = %+v", ac)
	}
	if ac.DisplayName != "Scout" || ac.Description != "looks around" || ac.Emoji != "x" || ac.Color != "#123456" {
		t.Fatalf("presentation not merged: %+v", ac)
	}
	if ac.Mode != "ADVISORY" || ac.SortOrder != 7 || ac.BeadRole != "worker" || ac.StaleTimeout != 60 || ac.RestartStrategy != "never" {
		t.Fatalf("spec not merged: %+v", ac)
	}
	if !ac.ClearOnKick || ac.IncludeRepos == nil || !*ac.IncludeRepos {
		t.Fatalf("flags: clear_on_kick=%v include_repos=%v", ac.ClearOnKick, ac.IncludeRepos)
	}
	if len(ac.LaneKeywords) != 1 || len(ac.DetectKeywords) != 1 || len(ac.Aliases) != 1 || ac.Tools == nil || len(ac.Connections) != 1 {
		t.Fatalf("lists not merged: %+v", ac)
	}
	if !ac.Enabled || ac.Replicas != DefaultReplicas || ac.Name() != "scout-agent" || ac.SourceFile() != path {
		t.Fatalf("defaults: enabled=%v replicas=%d name=%q source=%q", ac.Enabled, ac.Replicas, ac.Name(), ac.SourceFile())
	}
}

func TestLoadFilePortableDefaultsAndExplicitClearOnKick(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "min.yaml"), "kind: AgentDefinition\nmetadata:\n  name: minimal\nspec: {}\n")
	name, ac, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if name != "minimal" || ac.Backend != DefaultBackend || ac.RestartStrategy != DefaultRestartStrategy || ac.Role != "minimal" {
		t.Fatalf("defaults not applied: name=%q ac=%+v", name, ac)
	}
	// A portable definition's clearOnKick is authoritative even at false:
	// the per-agent default must not override it.
	if ac.ClearOnKick {
		t.Fatalf("clear_on_kick = true, want the definition's false")
	}
}

func TestLoadFileOverlay(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "fixer.yaml"), overlayDefinition)
	name, ac, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if name != "fixer" || ac.ID != "fixer" || ac.Backend != "codex" || ac.Model != "gpt-5" || ac.Role != "fixer" {
		t.Fatalf("name=%q ac=%+v", name, ac)
	}
	if ac.Enabled || ac.ClearOnKick {
		t.Fatalf("explicit enabled/clear_on_kick overridden: %+v", ac)
	}
	if !ac.Managed || ac.Replicas != 1 {
		t.Fatalf("overlay shape: managed=%v replicas=%d", ac.Managed, ac.Replicas)
	}

	path = writeFile(t, filepath.Join(t.TempDir(), "bare.yaml"), "backend: copilot\n")
	_, ac, err = LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ac.Enabled || !ac.ClearOnKick || ac.Role != "bare" {
		t.Fatalf("overlay defaults: %+v", ac)
	}
}

func TestLoadFileErrors(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := LoadFile(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("missing file")
	}
	cases := map[string]string{
		"bad-yaml.yaml":     "backend: [\n",
		"no-name.yaml":      "kind: AgentDefinition\nspec: {}\n",
		"Bad Name.yaml":     "backend: copilot\n",
		"bad-kind.yaml":     "kind: AgentDefinition\nmetadata:\n  name: 'Not/Valid'\n",
		"bad-portable.yaml": "kind: AgentDefinition\nmetadata: [\n",
	}
	for file, content := range cases {
		path := writeFile(t, filepath.Join(dir, file), content)
		if _, _, err := LoadFile(path); err == nil {
			t.Errorf("%s: expected an error", file)
		}
	}
}

func TestLoadDirectoryAndFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "scout.yaml"), portableDefinition)
	writeFile(t, filepath.Join(dir, "fixer.yaml"), overlayDefinition)
	writeFile(t, filepath.Join(dir, "notes.txt"), "ignored")
	writeFile(t, filepath.Join(dir, "sub", "deep.yaml"), "backend: copilot\n")

	defs, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 2 {
		t.Fatalf("defs = %v", keys(defs))
	}
	if _, ok := defs["scout-agent"]; !ok {
		t.Fatalf("portable definition missing: %v", keys(defs))
	}
	if _, ok := defs["fixer"]; !ok {
		t.Fatalf("overlay missing: %v", keys(defs))
	}

	one, err := Load(filepath.Join(dir, "fixer.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one["fixer"].Model != "gpt-5" {
		t.Fatalf("single file: %v", one)
	}

	if _, err := Load(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("missing path")
	}
	writeFile(t, filepath.Join(dir, "broken.yaml"), "backend: [\n")
	if _, err := Load(dir); err == nil {
		t.Fatal("broken file in dir")
	}
}

func TestLoadRejectsDuplicateNames(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.yaml"), "kind: AgentDefinition\nmetadata:\n  name: same\n")
	writeFile(t, filepath.Join(dir, "b.yaml"), "kind: AgentDefinition\nmetadata:\n  name: same\n")
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "already defined") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoaderInterface(t *testing.T) {
	var l Loader
	defs, project, err := l.LoadConfig("")
	if err != nil || len(defs) != 0 || project.Org != "" {
		t.Fatalf("empty: %v %v %v", defs, project, err)
	}
	if _, _, err := l.LoadConfig(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing path")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fixer.yaml"), overlayDefinition)
	defs, _, err = l.LoadConfig(dir)
	if err != nil || len(defs) != 1 {
		t.Fatalf("dir: %v %v", defs, err)
	}
	name, _, err := l.LoadDefinitionFile(filepath.Join(dir, "fixer.yaml"))
	if err != nil || name != "fixer" {
		t.Fatalf("file: %q %v", name, err)
	}
}

func TestApplyDefaultsLeavesSetValues(t *testing.T) {
	ac := config.AgentConfig{ID: "x", Role: "r", Replicas: 3}
	ac.MarkExplicit(true, true)
	ApplyDefaults("name", &ac)
	if ac.ID != "x" || ac.Role != "r" || ac.Replicas != 3 || ac.Enabled || ac.ClearOnKick {
		t.Fatalf("ac = %+v", ac)
	}
}

func TestValidateName(t *testing.T) {
	if err := ValidateName(""); err == nil {
		t.Fatal("empty")
	}
	if err := ValidateName(strings.Repeat("a", MaxNameLen+1)); err == nil {
		t.Fatal("too long")
	}
	if err := ValidateName("../etc"); err == nil {
		t.Fatal("path")
	}
	if err := ValidateName("ok-name_1"); err != nil {
		t.Fatal(err)
	}
}

func TestParseDefinitionErrors(t *testing.T) {
	if _, err := ParseDefinition([]byte("kind: Other\nmetadata:\n  name: x\n")); err == nil {
		t.Fatal("wrong kind")
	}
	if _, err := ParseDefinition([]byte(":\n  - [")); err == nil {
		t.Fatal("bad yaml")
	}
}

func keys(m map[string]config.AgentConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
