package skillreg

import (
	"os"
	"path/filepath"
	"testing"
)

const validSpec = `name: reviewer
backend: claude
model: claude-opus
mode: suggest
launch_cmd: custom-agent --flag
prompt: "Start with the ADR checklist."
tools:
  preset: advisory
  rules:
    - pattern: mcp__github__create_issue
      action: deny
      reason: read-only reviewer
skills:
  - go-testing
  - pr-etiquette
`

func TestParseAgentSpecValid(t *testing.T) {
	spec, err := ParseAgentSpec([]byte(validSpec))
	if err != nil {
		t.Fatalf("ParseAgentSpec: %v", err)
	}
	if spec.AgentName() != "reviewer" {
		t.Errorf("name = %q", spec.AgentName())
	}
	if spec.Backend() != "claude" {
		t.Errorf("backend = %q", spec.Backend())
	}
	if spec.Model() != "claude-opus" {
		t.Errorf("model = %q", spec.Model())
	}
	if spec.Mode() != ModeSuggest {
		t.Errorf("mode = %q", spec.Mode())
	}
	if spec.LaunchCommand != "custom-agent --flag" {
		t.Errorf("launch = %q", spec.LaunchCommand)
	}
	if spec.Prompt != "Start with the ADR checklist." {
		t.Errorf("prompt = %q", spec.Prompt)
	}
	if spec.Tools == nil || spec.Tools.Preset != "advisory" || len(spec.Tools.Rules) != 1 {
		t.Fatalf("tools = %+v", spec.Tools)
	}
	if got := spec.DefaultSkills(); len(got) != 2 || got[0] != "go-testing" {
		t.Errorf("skills = %v", got)
	}
}

func TestParseAgentSpecDefaultMode(t *testing.T) {
	spec, err := ParseAgentSpec([]byte("name: a\nbackend: b\nmodel: m\n"))
	if err != nil {
		t.Fatalf("ParseAgentSpec: %v", err)
	}
	if spec.Mode() != DefaultMode {
		t.Errorf("mode = %q, want default %q", spec.Mode(), DefaultMode)
	}
}

func TestParseAgentSpecRejections(t *testing.T) {
	cases := map[string]string{
		"malformed yaml":    "name: [oops\n\tbad",
		"missing name":      "backend: b\nmodel: m\n",
		"missing backend":   "name: a\nmodel: m\n",
		"missing model":     "name: a\nbackend: b\n",
		"unknown mode":      "name: a\nbackend: b\nmodel: m\nmode: rogue\n",
		"bad tools preset":  "name: a\nbackend: b\nmodel: m\ntools:\n  preset: nope\n",
		"bad tools pattern": "name: a\nbackend: b\nmodel: m\ntools:\n  rules:\n    - action: deny\n",
		"bad tools action":  "name: a\nbackend: b\nmodel: m\ntools:\n  rules:\n    - pattern: x\n      action: nope\n",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseAgentSpec([]byte(doc)); err == nil {
				t.Errorf("expected rejection for %s", name)
			}
		})
	}
}

func TestLoadAgentSpecFileAndDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(path, []byte(validSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := LoadAgentSpec(path)
	if err != nil {
		t.Fatalf("LoadAgentSpec(file): %v", err)
	}
	if spec.AgentName() != "reviewer" {
		t.Errorf("file load name = %q", spec.AgentName())
	}
	spec, err = LoadAgentSpec(dir)
	if err != nil {
		t.Fatalf("LoadAgentSpec(dir): %v", err)
	}
	if spec.AgentName() != "reviewer" {
		t.Errorf("dir load name = %q", spec.AgentName())
	}
	if _, err := LoadAgentSpec(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing path should error")
	}
}
