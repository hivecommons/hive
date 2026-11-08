package config

// CLIMemoryConfig governs the persistent, cross-session "memory" the agent
// CLIs keep for themselves under the per-agent HOME (hivecommons/hive#17032):
// Claude Code's auto-memory (~/.claude/projects/<cwd>/memory/), Codex
// memories ($CODEX_HOME/memories*), Copilot CLI's agentic memory, and the
// Gemini CLI's saved ~/.gemini/GEMINI.md. It is written fleet-wide under
// agents.cli_memory and may be overridden per agent under
// agents.<name>.cli_memory.
//
// Hive agents are stateless workers driven by kick templates, beads and the
// knowledge system; a hidden per-CLI notebook that survives /clear is a
// second, unaudited memory that was observed drifting into false and stale
// "facts" (hivecommons/hive#17031). It is therefore OFF unless the operator
// opts in, and may additionally be wiped before every kick.
type CLIMemoryConfig struct {
	// AutoMemory lets the CLI keep its own persistent notes across sessions.
	// Nil or false pins the CLI's auto-memory feature off at launch
	// (CLAUDE_CODE_DISABLE_AUTO_MEMORY=1, codex [features] memories=false,
	// copilot settings.json memory=false); true leaves the CLI's own default
	// (and un-pins codex/copilot).
	AutoMemory *bool `yaml:"auto_memory,omitempty" json:"auto_memory,omitempty"`
	// ClearOnKick wipes the agent's persistent memory store before every
	// kick, alongside the /clear that resets the conversation, so each turn
	// starts from the kick template and the shared knowledge system only.
	ClearOnKick bool `yaml:"clear_on_kick,omitempty" json:"clear_on_kick,omitempty"`
	// DrainToKnowledge converts what the CLI wrote to its memory store into
	// draft knowledge entries before each kick (and before ClearOnKick wipes
	// the store), so an inference the CLI kept for itself becomes a
	// reviewable, scoped fact instead of a hidden per-agent notebook
	// (hivecommons/hive#17033). Entries are deduplicated by content hash and
	// never promoted automatically.
	DrainToKnowledge bool `yaml:"drain_to_knowledge,omitempty" json:"drain_to_knowledge,omitempty"`
	// DrainOnWrite also drains a Claude Code memory file as soon as the
	// session writes it, through a PostToolUse hook, instead of only at
	// SessionEnd and at the next kick (hivecommons/hive#17210). It is for
	// long-lived sessions that rarely end, and has no effect unless
	// DrainToKnowledge is also set.
	DrainOnWrite bool `yaml:"drain_on_write,omitempty" json:"drain_on_write,omitempty"`
}

// IsZero reports whether no field was set, so an absent block encodes as
// nothing and a per-agent nil pointer inherits the fleet value.
func (c CLIMemoryConfig) IsZero() bool {
	return c.AutoMemory == nil && !c.ClearOnKick && !c.DrainToKnowledge && !c.DrainOnWrite
}

// AutoMemoryEnabled reports whether the CLI may keep persistent memory. The
// receiver may be nil (no per-agent override and no fleet block): the
// default is off.
func (c *CLIMemoryConfig) AutoMemoryEnabled() bool {
	return c != nil && c.AutoMemory != nil && *c.AutoMemory
}

// ClearMemoryOnKick reports whether the memory store is wiped before each
// kick. The receiver may be nil: the default is off.
func (c *CLIMemoryConfig) ClearMemoryOnKick() bool {
	return c != nil && c.ClearOnKick
}

// DrainMemoryToKnowledge reports whether the memory store is drained into
// draft knowledge entries before each kick. The receiver may be nil: the
// default is off.
func (c *CLIMemoryConfig) DrainMemoryToKnowledge() bool {
	return c != nil && c.DrainToKnowledge
}

// DrainMemoryOnWrite reports whether a Claude memory write is drained at
// once by the PostToolUse hook. It needs DrainToKnowledge. The receiver may
// be nil: the default is off.
func (c *CLIMemoryConfig) DrainMemoryOnWrite() bool {
	return c.DrainMemoryToKnowledge() && c.DrainOnWrite
}
