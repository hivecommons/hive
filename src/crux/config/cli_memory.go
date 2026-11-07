package config

// CLIMemoryConfig is the fleet policy for the coding CLIs' own persistent
// auto-memory (#17032): Claude Code's ~/.claude/projects/<project>/memory,
// Codex's $CODEX_HOME/memories, Gemini's global ~/.gemini/GEMINI.md. A hive
// agent is a lane worker that should start every kick from the kick prompt,
// beads and the knowledge primer — not from inferences its CLI wrote down on
// an earlier kick. Written under agents.cli_memory; fleet-wide only, because
// the settings files the disable is seeded into are shared across agents.
// The zero value is the default.
type CLIMemoryConfig struct {
	// AutoMemory leaves each CLI's auto-memory at the CLI's own default.
	// False (the default) seeds each CLI's documented "auto-memory off"
	// setting at launch. The seed is merge-only: a value already present in
	// the CLI's settings — an operator's explicit choice — is never
	// overwritten, so turning auto-memory back on after a seeded launch means
	// setting this true AND removing the seeded key.
	AutoMemory bool `yaml:"auto_memory,omitempty" json:"auto_memory,omitempty"`
	// ClearOnKick removes the agent's own CLI memory store before every
	// kick. Only stores inside a directory private to the agent are cleared;
	// a store reached through a symlink into shared state is skipped (and
	// logged once), so a clear never touches another agent's memory.
	ClearOnKick bool `yaml:"clear_on_kick,omitempty" json:"clear_on_kick,omitempty"`
}

// IsZero reports whether the block sets nothing, so the YAML encoder omits it.
func (c CLIMemoryConfig) IsZero() bool {
	return !c.AutoMemory && !c.ClearOnKick
}
