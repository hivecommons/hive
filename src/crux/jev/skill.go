package jev

import (
	_ "embed"
	"path"
)

// skillMarkdown is the jev-decide skill installed into an enabled agent's CLI
// home. Same shape as an agentskills.io SKILL.md so every backend that reads
// skills picks it up without a backend-specific plugin.
//
//go:embed skill/SKILL.md
var skillMarkdown []byte

// SkillName is the skill directory name.
const SkillName = "jev-decide"

// SkillFile is the file name inside SkillDir.
const SkillFile = "SKILL.md"

// SkillMarkdown returns the skill body.
func SkillMarkdown() []byte { return skillMarkdown }

// SkillRelDir returns the skills directory for backend, relative to the home
// the backend reads (HOME for most CLIs, CODEX_HOME for codex). Empty means the
// backend has no skills directory the hive knows how to seed; the caller
// should log and skip rather than guess.
func SkillRelDir(backend string) string {
	switch backend {
	case "claude":
		return path.Join(".claude", "skills", SkillName)
	case "codex":
		// Relative to CODEX_HOME, which the manager sets per agent.
		return path.Join("skills", SkillName)
	case "copilot":
		return path.Join(".copilot", "skills", SkillName)
	case "gemini":
		return path.Join(".gemini", "skills", SkillName)
	case "goose":
		return path.Join(".config", "goose", "skills", SkillName)
	default:
		return ""
	}
}
