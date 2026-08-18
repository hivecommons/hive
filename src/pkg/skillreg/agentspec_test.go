package skillreg

import (
	"os"
	"path/filepath"
	"testing"
)

const validSpec = `name: reviewer
backend: claude-code
model: claude-opus
mode: suggest
skills:
  - go-testing
  - pr-etiquette
`

func TestParseAgentSpec_Valid(t *testing.T) {
	spec, err := ParseAgentSpec([]byte(validSpec))
	if err != nil {
		t.Fatalf("ParseAgentSpec: %v", err)
	}
	if spec.AgentName() != "reviewer" {
		t.Errorf("name = %q", spec.AgentName())
	}
	if spec.Backend() != "claude-code" {
		t.Errorf("backend = %q", spec.Backend())
	}
	if spec.Model() != "claude-opus" {
		t.Errorf("model = %q", spec.Model())
	}
	if spec.Mode() != ModeSuggest {
		t.Errorf("mode = %q", spec.Mode())
	}
	if got := spec.DefaultSkills(); len(got) != 2 || got[0] != "go-testing" {
		t.Errorf("skills = %v", got)
	}
}

func TestParseAgentSpec_DefaultMode(t *testing.T) {
	spec, err := ParseAgentSpec([]byte("name: a\nbackend: b\nmodel: m\n"))
	if err != nil {
		t.Fatalf("ParseAgentSpec: %v", err)
	}
	if spec.Mode() != DefaultMode {
		t.Errorf("mode = %q, want default %q", spec.Mode(), DefaultMode)
	}
}

func TestSpecData_ModeAccessorDefault(t *testing.T) {
	// A zero-value SpecData's Mode accessor also returns the default.
	var s SpecData
	if s.Mode() != DefaultMode {
		t.Errorf("zero Mode = %q, want %q", s.Mode(), DefaultMode)
	}
}

func TestParseAgentSpec_Rejections(t *testing.T) {
	cases := map[string]string{
		"malformed yaml":  "name: [oops\n\tbad",
		"missing name":    "backend: b\nmodel: m\n",
		"missing backend": "name: a\nmodel: m\n",
		"missing model":   "name: a\nbackend: b\n",
		"unknown mode":    "name: a\nbackend: b\nmodel: m\nmode: rogue\n",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseAgentSpec([]byte(doc)); err == nil {
				t.Errorf("expected rejection for %s", name)
			}
		})
	}
}

func TestParseAgentSpec_AllModes(t *testing.T) {
	for _, m := range []AgentMode{ModeObserve, ModeSuggest, ModeAutonomous} {
		doc := "name: a\nbackend: b\nmodel: m\nmode: " + string(m) + "\n"
		spec, err := ParseAgentSpec([]byte(doc))
		if err != nil {
			t.Fatalf("mode %q rejected: %v", m, err)
		}
		if spec.Mode() != m {
			t.Errorf("mode = %q, want %q", spec.Mode(), m)
		}
	}
}

func TestLoadAgentSpec_FileAndMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(path, []byte(validSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := LoadAgentSpec(path)
	if err != nil {
		t.Fatalf("LoadAgentSpec: %v", err)
	}
	if spec.AgentName() != "reviewer" {
		t.Errorf("name = %q", spec.AgentName())
	}

	if _, err := LoadAgentSpec(filepath.Join(dir, "nope.yaml")); err == nil {
		t.Error("missing file should error")
	}
}
