package jev

import (
	"strings"
	"testing"
)

func TestSkillRelDir(t *testing.T) {
	for backend, want := range map[string]string{
		"claude": ".claude/skills/jev-decide", "codex": "skills/jev-decide",
		"copilot": ".copilot/skills/jev-decide", "gemini": ".gemini/skills/jev-decide",
		"goose": ".config/goose/skills/jev-decide", "aider": "", "bob": "",
	} {
		if got := SkillRelDir(backend); got != want {
			t.Errorf("%s: %q, want %q", backend, got, want)
		}
	}
	if !strings.Contains(string(SkillMarkdown()), "hive jev decide") || !strings.HasPrefix(string(SkillMarkdown()), "---\nname: jev-decide") {
		t.Error("embedded skill must be the jev-decide SKILL.md with front matter")
	}
}

func TestEnvironmentContract(t *testing.T) {
	if ModeEnvVar != "HIVE_JEV_MODE" || EndpointEnvVar != "HIVE_JEV_ENDPOINT" {
		t.Fatalf("env var names changed: %q %q", ModeEnvVar, EndpointEnvVar)
	}
	if !strings.HasPrefix(DefaultEndpoint, "http://127.0.0.1:") {
		t.Fatalf("DefaultEndpoint must be loopback, got %q", DefaultEndpoint)
	}
	if SkillFile != "SKILL.md" || !strings.HasSuffix(SkillRelDir("claude"), SkillName) {
		t.Fatalf("skill layout changed: %q %q", SkillFile, SkillRelDir("claude"))
	}
}
